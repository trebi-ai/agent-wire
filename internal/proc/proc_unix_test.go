//go:build unix

package proc

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestKillTreeKillsProcessGroup pins group escalation: a grandchild that
// shares the process group dies with the child.
func TestKillTreeKillsProcessGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	p := startShell(t, Opts{}, `sleep 300 & echo $! > "$1"; wait`, "sh", pidFile)

	grandchild := 0
	waitCond(t, 5*time.Second, "the grandchild pid file", func() bool {
		data, err := os.ReadFile(pidFile)
		if err != nil {
			return false
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil || pid <= 0 {
			return false
		}
		grandchild = pid
		return true
	})
	if !Alive(p.PID()) {
		t.Fatalf("child %d is not alive before the kill", p.PID())
	}
	if !Alive(grandchild) {
		t.Fatalf("grandchild %d is not alive before the kill", grandchild)
	}

	_ = p.KillTree()

	waitCond(t, 5*time.Second, fmt.Sprintf("pid %d to disappear", grandchild), func() bool {
		return pidGone(grandchild)
	})
	waitCond(t, 5*time.Second, fmt.Sprintf("pid %d to disappear", p.PID()), func() bool {
		return pidGone(p.PID())
	})
}

// TestGracefulCloseOnStdinEOF pins the first stage: a child that reads stdin
// exits on the EOF well before the stage timeout.
func TestGracefulCloseOnStdinEOF(t *testing.T) {
	p := startShell(t, Opts{}, `cat >/dev/null; exit 0`)
	begin := time.Now()
	if err := p.GracefulClose(3 * time.Second); err != nil {
		t.Fatalf("close: %v", err)
	}
	if elapsed := time.Since(begin); elapsed >= 2*time.Second {
		t.Fatalf("child needed %s to exit on stdin EOF", elapsed)
	}
	if code := p.ExitCode(); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
}

// TestGracefulCloseWaitsForTheTermStage pins the second stage: the child gets
// the full flush window before SIGTERM, and its trap then runs.
func TestGracefulCloseWaitsForTheTermStage(t *testing.T) {
	const stage = 400 * time.Millisecond
	p := startShell(t, Opts{}, `trap 'exit 0' TERM; echo ready; while true; do sleep 0.05; done`)
	waitReady(t, p)

	begin := time.Now()
	if err := p.GracefulClose(stage); err != nil {
		t.Fatalf("close: %v", err)
	}
	if elapsed := time.Since(begin); elapsed < stage {
		t.Fatalf("SIGTERM arrived after %s, want the full stdin stage of %s", elapsed, stage)
	}
	// A zero exit code proves that the trap ran. The kill stage reports -1.
	if code := p.ExitCode(); code != 0 {
		t.Fatalf("exit code = %d, want 0 from the TERM trap", code)
	}
}

// TestGracefulCloseKillsAChildThatIgnoresTerm pins the last stage: a child
// that ignores SIGTERM still dies.
func TestGracefulCloseKillsAChildThatIgnoresTerm(t *testing.T) {
	const stage = 300 * time.Millisecond
	p := startShell(t, Opts{}, `trap '' TERM; echo ready; while true; do sleep 0.05; done`)
	waitReady(t, p)

	begin := time.Now()
	if err := p.GracefulClose(stage); err != nil {
		t.Fatalf("close: %v", err)
	}
	// The child ignores stdin and SIGTERM, so both stages time out.
	if elapsed := time.Since(begin); elapsed < 2*stage {
		t.Fatalf("close took %s, want at least two stages of %s", elapsed, stage)
	}
	if Alive(p.PID()) {
		t.Fatalf("child %d survived the kill stage", p.PID())
	}
}

// TestStderrPastCapKeepsTheTail pins the stderr drain: the reader continues
// past the cap, so the child never sees SIGPIPE, and the tail holds the last
// bytes instead of the first.
func TestStderrPastCapKeepsTheTail(t *testing.T) {
	const lineCount = 1000
	const firstLine = "line-00000-"
	const lastLine = "tail-marker-END"

	var mu sync.Mutex
	var seen []string
	p := startShell(t, Opts{OnStderr: func(line string) {
		mu.Lock()
		seen = append(seen, line)
		mu.Unlock()
	}}, fmt.Sprintf(`i=0
while [ "$i" -lt %d ]; do
  printf 'line-%%05d-%%s\n' "$i" '%s' >&2
  i=$((i+1))
done
printf '%%s\n' '%s' >&2`, lineCount, strings.Repeat("x", 54), lastLine))

	if !p.WaitTimeout(10 * time.Second) {
		t.Fatal("child did not exit")
	}
	// A zero exit code proves that the child never got SIGPIPE from us.
	if code := p.ExitCode(); code != 0 {
		t.Fatalf("exit code = %d, want 0; the stderr reader stopped draining", code)
	}

	waitCond(t, 5*time.Second, "the stderr tail to hold the last line", func() bool {
		return strings.Contains(p.Stderr(), lastLine)
	})
	tail := p.Stderr()
	if len(tail) > stderrTailMax {
		t.Fatalf("stderr tail is %d bytes, want at most %d", len(tail), stderrTailMax)
	}
	if strings.Contains(tail, firstLine) {
		t.Fatal("stderr tail holds the first line; the cap must drop it")
	}

	waitCond(t, 5*time.Second, "OnStderr to report every line", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(seen) == lineCount+1
	})
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != lineCount+1 {
		t.Fatalf("OnStderr reported %d lines, want %d", len(seen), lineCount+1)
	}
	if !strings.HasPrefix(seen[0], firstLine) {
		t.Fatalf("first reported line = %q, want the %s prefix", seen[0], firstLine)
	}
	if seen[len(seen)-1] != lastLine {
		t.Fatalf("last reported line = %q, want %q", seen[len(seen)-1], lastLine)
	}
}

// TestWaitTimeoutAndExitCode pins the exit code before and after the wait,
// and the default timeout of WaitTimeout(0).
func TestWaitTimeoutAndExitCode(t *testing.T) {
	p := startShell(t, Opts{}, `sleep 0.4; exit 7`)
	if code := p.ExitCode(); code != -1 {
		t.Fatalf("ExitCode before the exit = %d, want -1", code)
	}
	if !p.WaitTimeout(0) {
		t.Fatal("WaitTimeout(0) returned before the child exited; it must use the default")
	}
	if code := p.ExitCode(); code != 7 {
		t.Fatalf("exit code after the wait = %d, want 7", code)
	}

	live := startShell(t, Opts{}, `sleep 300`)
	if live.WaitTimeout(50 * time.Millisecond) {
		t.Fatal("WaitTimeout returned true for a live child")
	}
	if code := live.ExitCode(); code != -1 {
		t.Fatalf("ExitCode for a live child = %d, want -1", code)
	}
	_ = live.KillTree()
	waitCond(t, 3*time.Second, "the killed child to disappear", func() bool {
		return !Alive(live.PID())
	})
}

// TestAliveAndStartToken pins the pid probes.
func TestAliveAndStartToken(t *testing.T) {
	if !Alive(os.Getpid()) {
		t.Fatalf("Alive(%d) = false for the test process", os.Getpid())
	}
	if Alive(1 << 30) {
		t.Fatal("Alive(1<<30) = true")
	}
	if Alive(0) || Alive(-1) {
		t.Fatal("Alive of a non-positive pid = true")
	}
	if token := StartToken(os.Getpid()); token == "" {
		t.Fatalf("StartToken(%d) is empty", os.Getpid())
	}
	if token := StartToken(0); token != "" {
		t.Fatalf("StartToken(0) = %q, want empty", token)
	}

	// A child that exited and was reaped is a pid that certainly does not
	// exist.
	p := startShell(t, Opts{}, `exit 0`)
	if !p.WaitTimeout(5 * time.Second) {
		t.Fatal("child did not exit")
	}
	waitCond(t, 3*time.Second, "the reaped child pid to disappear", func() bool {
		return !Alive(p.PID())
	})
}

// startShell starts `sh -c script` and kills the child when the test ends.
// Extra args become the shell positional parameters, so args[0] is $0.
func startShell(t *testing.T, opts Opts, script string, args ...string) *Proc {
	t.Helper()
	opts.Path = "sh"
	opts.Args = append([]string{"-c", script}, args...)
	p, err := Start(context.Background(), opts)
	if err != nil {
		t.Fatalf("start sh: %v", err)
	}
	t.Cleanup(func() {
		select {
		case <-p.Done():
		default:
			_ = p.KillTree()
		}
	})
	return p
}

// waitReady blocks until the child prints its ready marker. The marker proves
// that the child installed its signal handlers.
func waitReady(t *testing.T, p *Proc) {
	t.Helper()
	line := make(chan string, 1)
	go func() {
		s, _ := bufio.NewReader(p.Stdout()).ReadString('\n')
		line <- s
	}()
	select {
	case s := <-line:
		if strings.TrimSpace(s) != "ready" {
			t.Fatalf("child printed %q, want the ready marker", s)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("child never printed the ready marker")
	}
}

// waitCond polls cond until it holds or the deadline passes.
func waitCond(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", d, what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// pidGone reports whether the pid no longer exists.
func pidGone(pid int) bool {
	return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}
