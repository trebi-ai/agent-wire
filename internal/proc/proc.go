// Package proc spawns and reaps one child process with explicit pipes, group
// control, and a single Wait.
//
// It is the only lifecycle mechanism for harness children. exec.CommandContext
// is deliberately not used: a canceled context must not orphan a child in the
// middle of a protocol write, and a vendor child needs a flush window before
// it is signalled.
package proc

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// GracefulCloseTimeout is the default flush window between close-stdin,
// SIGTERM, and SIGKILL. The vendor SDK comment is load-bearing: SIGTERM too
// early can truncate the vendor session file and lose the last assistant
// message.
const GracefulCloseTimeout = 5 * time.Second

// stderrTailMax bounds the retained stderr tail. Reading continues past the
// cap: closing the pipe early would hand the child a SIGPIPE and change the
// failure it reports.
const stderrTailMax = 32 << 10

// Opts describes one child to spawn.
type Opts struct {
	// Path is the executable. It must be a native binary on Windows; .cmd and
	// .bat shims are refused.
	Path string
	Args []string
	Dir  string
	// Env is the full child environment in "K=V" form. Nil inherits the
	// parent environment.
	Env []string
	// OnStderr receives each stderr line as it is read. It is optional and
	// runs on the drain goroutine, so it must not block.
	OnStderr func(string)
}

// Proc is a spawned child.
type Proc struct {
	cmd    *exec.Cmd
	stdin  *os.File
	stdout *os.File
	stderr *os.File
	pid    int
	pgid   int

	platform platform

	stdinOnce sync.Once
	waitOnce  sync.Once
	waitErr   error
	waitDone  chan struct{}

	exitCode atomic.Int64

	onStderr func(string)

	// stderrTail keeps the last chunk of stderr for classification when the
	// process dies without a protocol result.
	stderrMu   sync.Mutex
	stderrTail []byte
}

// rejectShim refuses a .cmd/.bat wrapper. A shim re-parses the command line,
// so a structured-protocol child must be a native executable.
func rejectShim(path string) error {
	if !isShim(path) {
		return nil
	}
	resolved := path
	if p, err := exec.LookPath(path); err == nil {
		resolved = p
	}
	return &shimError{path: resolved}
}

// shimError reports a refused .cmd/.bat wrapper.
type shimError struct{ path string }

func (e *shimError) Error() string {
	return "proc: refusing " + e.path + ": a .cmd/.bat shim re-parses the command line, so a structured-protocol " +
		"child must be a native executable; resolve the shim target (for npm installs, `node <path>/cli.js`) or pin a native binary"
}

// Start spawns a child. The returned Proc has a bounded stderr tail reader
// attached; stdout is the caller's to frame.
func Start(ctx context.Context, opts Opts) (*Proc, error) {
	if opts.Path == "" {
		return nil, errors.New("proc: empty command path")
	}
	if err := rejectShim(opts.Path); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		_ = inR.Close()
		_ = inW.Close()
		return nil, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		_ = inR.Close()
		_ = inW.Close()
		_ = outR.Close()
		_ = outW.Close()
		return nil, err
	}

	cmd := exec.Command(opts.Path, opts.Args...)
	cmd.Dir = opts.Dir
	cmd.Env = opts.Env
	cmd.Stdin = inR
	cmd.Stdout = outW
	cmd.Stderr = errW
	cmd.SysProcAttr = sysProcAttr()

	if err := cmd.Start(); err != nil {
		for _, f := range []*os.File{inR, inW, outR, outW, errR, errW} {
			_ = f.Close()
		}
		return nil, fmt.Errorf("start %s: %w", opts.Path, err)
	}
	// The parent copies of the child ends go away now.
	_ = inR.Close()
	_ = outW.Close()
	_ = errW.Close()

	p := &Proc{
		cmd:      cmd,
		stdin:    inW,
		stdout:   outR,
		stderr:   errR,
		pid:      cmd.Process.Pid,
		waitDone: make(chan struct{}),
		onStderr: opts.OnStderr,
	}
	p.pgid = processGroupID(cmd.Process.Pid)
	_ = p.platform.afterStart(cmd)
	go p.wait()
	go p.drainStderr()
	return p, nil
}

// wait blocks on the child exactly once and records the exit code.
func (p *Proc) wait() {
	p.waitOnce.Do(func() {
		err := p.cmd.Wait()
		p.waitErr = err
		code := 0
		if err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				code = ee.ExitCode()
			} else {
				code = -1
			}
		}
		p.exitCode.Store(int64(code))
		p.platform.afterWait()
		close(p.waitDone)
	})
}

// drainStderr keeps the last stderrTailMax bytes and reports every line to
// OnStderr. The pipe is read to EOF even past the cap, so the child never
// sees SIGPIPE from us.
func (p *Proc) drainStderr() {
	defer p.stderr.Close()
	sc := bufio.NewScanner(p.stderr)
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		p.stderrMu.Lock()
		p.stderrTail = append(p.stderrTail, line...)
		p.stderrTail = append(p.stderrTail, '\n')
		if len(p.stderrTail) > stderrTailMax {
			p.stderrTail = p.stderrTail[len(p.stderrTail)-stderrTailMax:]
		}
		p.stderrMu.Unlock()
		if p.onStderr != nil {
			p.onStderr(string(line))
		}
	}
	// An oversized line stops the scanner; keep draining so the child never
	// blocks on a full stderr pipe.
	_, _ = io.Copy(io.Discard, p.stderr)
}

// PID is the child process id.
func (p *Proc) PID() int { return p.pid }

// Pgid is the process group id (equal to PID on platforms without groups).
func (p *Proc) Pgid() int { return p.pgid }

// Argv is the full command line of the child.
func (p *Proc) Argv() []string {
	if p.cmd == nil {
		return nil
	}
	return append([]string{p.cmd.Path}, p.cmd.Args[1:]...)
}

// Stdin is the child's stdin writer.
func (p *Proc) Stdin() io.Writer { return p.stdin }

// Stdout is the child's stdout reader.
func (p *Proc) Stdout() io.Reader { return p.stdout }

// CloseStdin closes the write side of the child's stdin once.
func (p *Proc) CloseStdin() error {
	var err error
	p.stdinOnce.Do(func() { err = p.stdin.Close() })
	return err
}

// Wait blocks until the child exits or ctx ends. It returns the exit code (-1
// when ctx ended first) and the wait error.
func (p *Proc) Wait(ctx context.Context) (int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-p.waitDone:
	case <-ctx.Done():
		return -1, ctx.Err()
	}
	return int(p.exitCode.Load()), p.waitErr
}

// WaitTimeout waits up to d for the child to exit. ok=false means still alive.
func (p *Proc) WaitTimeout(d time.Duration) bool {
	if d <= 0 {
		d = GracefulCloseTimeout
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-p.waitDone:
		return true
	case <-t.C:
		return false
	}
}

// ExitCode returns the code once Wait has completed (-1 before).
func (p *Proc) ExitCode() int {
	select {
	case <-p.waitDone:
		return int(p.exitCode.Load())
	default:
		return -1
	}
}

// Done is closed when the child exits.
func (p *Proc) Done() <-chan struct{} { return p.waitDone }

// Stderr returns the accumulated stderr tail.
func (p *Proc) Stderr() string {
	p.stderrMu.Lock()
	defer p.stderrMu.Unlock()
	return string(p.stderrTail)
}

// KillTree hard-kills the process tree and waits for the child.
func (p *Proc) KillTree() error {
	p.killTreePlatform()
	<-p.waitDone
	return p.waitErr
}

// SignalGroup delivers sig to the whole process group when possible, else to
// the process.
func (p *Proc) SignalGroup(sig os.Signal) { p.signalGroup(sig) }

// TermSignal is the graceful stop request this platform can deliver to a
// process group.
func TermSignal() os.Signal { return termSignal() }

// GracefulClose closes stdin, waits for the child to flush, then escalates
// SIGTERM and SIGKILL on the process group. timeout is per stage (default
// GracefulCloseTimeout).
func (p *Proc) GracefulClose(timeout time.Duration) error {
	if timeout <= 0 {
		timeout = GracefulCloseTimeout
	}
	_ = p.CloseStdin()
	if p.WaitTimeout(timeout) {
		return nil
	}
	p.signalTerm()
	if p.WaitTimeout(timeout) {
		return nil
	}
	p.killTreePlatform()
	<-p.waitDone
	return nil
}

// signalTerm asks the group to shut down. On Windows the group receives a
// console control event, because os.Process.Signal cannot deliver os.Interrupt.
func (p *Proc) signalTerm() { p.signalGroup(termSignal()) }

// FirstLine returns the first line of s, trimmed.
func FirstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
