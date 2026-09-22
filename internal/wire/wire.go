// Package wire is the session plumbing shared by every harness driver: it
// owns the child process, the NDJSON framing, the event fan-out, and the
// graceful close sequence. Drivers own parse and encode only.
//
// The public event types live here and are re-exported by the root package,
// so a driver never imports the root and the root never reaches into a
// driver's framing.
package wire

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/trebi-ai/agent-wire/internal/proc"
)

// DefaultMaxFrameBytes bounds one NDJSON frame when the caller sets no limit.
// Giant tool payloads fit; a runaway line does not.
const DefaultMaxFrameBytes = 16 << 20

// exitFlushTimeout is how long the final EventExit waits for a consumer that
// stopped reading before the channel is closed without it.
const exitFlushTimeout = 2 * time.Second

// killDrainTimeout bounds the wait for the event channel to close after a
// hard kill.
const killDrainTimeout = 2 * time.Second

// EndReasonProtocol is the failure family of a wire-level failure. It mirrors
// the root package's FailureClass "protocol"; framing cannot import the root
// package, so the value is restated here.
const EndReasonProtocol = "protocol"

// Adapter is one vendor protocol codec.
type Adapter interface {
	// Handshake writes protocol initialization (control initialize, thread
	// setup) and returns any immediately-known events. It runs inside Start,
	// which blocks until it returns. A non-nil error fails the start.
	Handshake(ctx context.Context, w *Writer) ([]Event, error)
	// EncodePrompt renders one user turn as one frame.
	EncodePrompt(p Prompt) ([]byte, error)
	// Parse maps one stdout line to zero or more events.
	Parse(line []byte) []Event
	// EncodeDecision answers a permission request.
	EncodeDecision(id string, d Decision) ([]byte, error)
	// Exit maps process exit (after stdout EOF) to events. It is not called
	// when the session produced its own terminal result and exited cleanly.
	Exit(code int, err error, stderr string) []Event
}

// InterruptEncoder is the optional protocol-level interrupt frame.
type InterruptEncoder interface {
	EncodeInterrupt() ([]byte, error)
}

// Prompter is implemented by adapters whose prompt is a request with a
// response (Codex turn/start, turn/steer). They own ack handling and the
// busy-turn fallback instead of the fire-and-forget frame path.
type Prompter interface {
	PromptSession(ctx context.Context, p Prompt) error
}

// WriterSetter is implemented by adapters that need the stdin writer before
// Handshake runs (every JSON-RPC driver).
type WriterSetter interface {
	SetWriter(*Writer)
}

// Writer is an adapter's handle on the child's stdin plus the session id.
type Writer struct {
	s *Session
}

// Write sends one or more frames to the child.
func (w *Writer) Write(b []byte) error { return w.s.write(b) }

// SetSessionID records the vendor session id learned on the wire.
func (w *Writer) SetSessionID(id string) {
	if id != "" {
		w.s.setID(id)
	}
}

// Harness is the harness name the session runs.
func (w *Writer) Harness() string { return w.s.harness }

// Session is one conversation over a child process.
type Session struct {
	harness  string
	proc     *proc.Proc
	stdin    io.Writer
	adapter  Adapter
	events   chan Event
	maxFrame int

	writeMu sync.Mutex
	idMu    sync.Mutex
	id      string
	turn    int

	done      chan struct{}
	closeOnce sync.Once
	closeErr  error
	closing   chan struct{}
	onClose   []func()

	emitMu     sync.Mutex
	emitClosed bool

	log *frameLog

	errMu sync.Mutex
	err   error
}

// Config tunes one wire session.
type Config struct {
	// MaxFrameBytes bounds one frame. Zero uses DefaultMaxFrameBytes.
	MaxFrameBytes int
	// LogPath records every frame in and out as NDJSON, one line per frame.
	// Secrets are not redacted, so the file is created mode 0600.
	LogPath string
}

// frameLog writes raw frames for a debug trail.
type frameLog struct {
	mu sync.Mutex
	f  *os.File
}

func openFrameLog(path string) (*frameLog, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("frame log: %w", err)
	}
	return &frameLog{f: f}, nil
}

func (l *frameLog) write(dir string, frame []byte) {
	if l == nil {
		return
	}
	entry, err := json.Marshal(map[string]any{
		"at": time.Now().UTC().Format(time.RFC3339Nano), "dir": dir, "frame": json.RawMessage(frame),
	})
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = l.f.Write(append(entry, '\n'))
}

func (l *frameLog) close() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_ = l.f.Close()
}

// Start spawns the child and starts the framing loop. It blocks until the
// adapter handshake returns; a handshake failure kills the child and is
// returned as an error. ctx covers the handshake only: the process lifetime
// does not depend on it.
func Start(ctx context.Context, harness string, p *proc.Proc, adapter Adapter, cfg Config) (*Session, error) {
	s, err := newSession(harness, p, adapter, cfg)
	if err != nil {
		return nil, err
	}
	w := &Writer{s: s}
	if ws, ok := adapter.(WriterSetter); ok {
		ws.SetWriter(w)
	}
	go s.readLoop()
	evs, err := adapter.Handshake(ctx, w)
	if err != nil {
		s.abort()
		return nil, err
	}
	for _, e := range evs {
		s.emit(e)
	}
	return s, nil
}

// NewMemorySession builds an in-memory session: stdin is the writer the
// driver uses, stdout the reader the peer writes responses into. It is the
// test seam for driver tests and the substitution point for a scripted peer.
func NewMemorySession(harness string, adapter Adapter, stdin io.Writer, stdout io.Reader, ctx context.Context, cfg Config) *Session {
	s, err := newSession(harness, nil, adapter, cfg)
	if err != nil {
		// A memory session has no file to create; the only failure is a bad
		// log path, which the caller must see.
		panic(err)
	}
	s.stdin = stdin
	go func() {
		s.scan(stdout)
		s.finish()
	}()
	w := &Writer{s: s}
	if ws, ok := adapter.(WriterSetter); ok {
		ws.SetWriter(w)
	}
	if evs, err := adapter.Handshake(ctx, w); err == nil {
		for _, e := range evs {
			s.emit(e)
		}
	} else {
		s.emit(Event{Type: EventError, Error: err.Error(), Code: "handshake_failed", EndReason: EndReasonProtocol})
	}
	return s
}

func newSession(harness string, p *proc.Proc, adapter Adapter, cfg Config) (*Session, error) {
	maxFrame := cfg.MaxFrameBytes
	if maxFrame <= 0 {
		maxFrame = DefaultMaxFrameBytes
	}
	log, err := openFrameLog(cfg.LogPath)
	if err != nil {
		return nil, err
	}
	return &Session{
		harness:  harness,
		proc:     p,
		stdin:    procStdin(p),
		adapter:  adapter,
		events:   make(chan Event, 256),
		maxFrame: maxFrame,
		log:      log,
		done:     make(chan struct{}),
		closing:  make(chan struct{}),
	}, nil
}

func procStdin(p *proc.Proc) io.Writer {
	if p == nil {
		return nil
	}
	return p.Stdin()
}

// Harness is the session's harness name.
func (s *Session) Harness() string { return s.harness }

// Provider is the harness name (Session interface).
func (s *Session) Provider() string { return s.harness }

// ID is the vendor session id; it may be empty until init.
func (s *Session) ID() string {
	s.idMu.Lock()
	defer s.idMu.Unlock()
	return s.id
}

func (s *Session) setID(id string) {
	s.idMu.Lock()
	s.id = id
	s.idMu.Unlock()
}

// SetID records the vendor session id a driver learned or pre-assigned.
func (s *Session) SetID(id string) { s.setID(id) }

// PID is the child process id (0 for in-process sessions).
func (s *Session) PID() int {
	if s.proc == nil {
		return 0
	}
	return s.proc.PID()
}

// Pgid is the child's process group.
func (s *Session) Pgid() int {
	if s.proc == nil {
		return 0
	}
	return s.proc.Pgid()
}

// Argv is the child command line (ledger evidence).
func (s *Session) Argv() []string {
	if s.proc == nil {
		return nil
	}
	return s.proc.Argv()
}

// Events is closed after the session is finished and its terminal events are
// drained. A consumer MUST read until it closes, or call Close: a consumer
// that walks away without either leaks the framing goroutine.
func (s *Session) Events() <-chan Event { return s.events }

// Done is closed when the session ends and the event channel is closed.
func (s *Session) Done() <-chan struct{} { return s.done }

// Err returns the first failure the session recorded, if any.
func (s *Session) Err() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.err
}

func (s *Session) recordErr(err error) {
	if err == nil {
		return
	}
	s.errMu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.errMu.Unlock()
}

// OnClose registers a cleanup hook (temp dirs, files). Hooks run once, after
// the session ends.
func (s *Session) OnClose(fn func()) {
	if fn == nil {
		return
	}
	s.emitMu.Lock()
	s.onClose = append(s.onClose, fn)
	s.emitMu.Unlock()
}

func (s *Session) runOnClose() {
	s.emitMu.Lock()
	hooks := s.onClose
	s.onClose = nil
	s.emitMu.Unlock()
	for _, fn := range hooks {
		fn()
	}
}

// Turn is the number of user turns this session has written.
func (s *Session) Turn() int {
	s.idMu.Lock()
	defer s.idMu.Unlock()
	return s.turn
}

func (s *Session) bumpTurn() int {
	s.idMu.Lock()
	defer s.idMu.Unlock()
	s.turn++
	return s.turn
}

func (s *Session) emit(e Event) {
	if e.At.IsZero() {
		e.At = time.Now()
	}
	if e.SessionID == "" {
		e.SessionID = s.ID()
	} else {
		s.setID(e.SessionID)
	}
	if e.Turn == 0 {
		e.Turn = s.Turn()
	}
	if e.Type == EventError {
		if e.Error != "" {
			s.recordErr(errors.New(e.Error))
		} else if e.Code != "" {
			s.recordErr(errors.New(e.Code))
		}
	}
	s.emitMu.Lock()
	defer s.emitMu.Unlock()
	if s.emitClosed {
		return
	}
	select {
	case s.events <- e:
	case <-s.closing:
		// After Close, only EventExit is still owed to the consumer.
		if e.Type != EventExit {
			return
		}
		select {
		case s.events <- e:
		case <-time.After(exitFlushTimeout):
		}
	}
}

func (s *Session) write(b []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.stdin == nil {
		return errNoStdin
	}
	s.log.write("out", bytes.TrimSpace(b))
	_, err := s.stdin.Write(b)
	return err
}

var errNoStdin = errors.New("agentwire: session stdin is closed")

// Prompt writes one user turn. The child must still be alive.
func (s *Session) Prompt(ctx context.Context, p Prompt) error {
	s.bumpTurn()
	if sp, ok := s.adapter.(Prompter); ok {
		return sp.PromptSession(ctx, p)
	}
	frame, err := s.adapter.EncodePrompt(p)
	if err != nil {
		return err
	}
	return s.WriteFrame(frame)
}

// WriteFrame writes one newline-terminated NDJSON frame.
func (s *Session) WriteFrame(frame []byte) error {
	if frame == nil {
		return nil
	}
	if len(frame) == 0 || frame[len(frame)-1] != '\n' {
		frame = append(frame, '\n')
	}
	return s.write(frame)
}

// AnswerPermission routes a decision back to the vendor.
func (s *Session) AnswerPermission(ctx context.Context, permissionID string, d Decision) error {
	frame, err := s.adapter.EncodeDecision(permissionID, d)
	if err != nil {
		return err
	}
	return s.WriteFrame(frame)
}

// InterruptTurn sends the protocol interrupt when the adapter has one, else a
// graceful signal to the process group. The session survives a protocol
// interrupt; a signal-based interrupt asks the child to stop the turn itself.
func (s *Session) InterruptTurn(ctx context.Context) error {
	if ie, ok := s.adapter.(InterruptEncoder); ok {
		frame, err := ie.EncodeInterrupt()
		if err != nil {
			return err
		}
		return s.WriteFrame(frame)
	}
	if s.proc == nil {
		return ErrUnsupported
	}
	s.proc.SignalGroup(proc.TermSignal())
	return nil
}

// Interrupt is the Session method: a protocol interrupt where available, a
// signal otherwise.
func (s *Session) Interrupt(ctx context.Context) error { return s.InterruptTurn(ctx) }

// ErrUnsupported reports a capability the harness or protocol does not have.
var ErrUnsupported = errors.New("agentwire: unsupported by this harness")

// Close runs the graceful sequence and waits for the event channel to drain.
func (s *Session) Close(ctx context.Context) error {
	s.closeOnce.Do(func() {
		if s.proc != nil {
			s.closeErr = s.proc.GracefulClose(proc.GracefulCloseTimeout)
			// Unblock a stalled consumer so the framing loop can finish.
			close(s.closing)
		} else {
			close(s.closing)
		}
	})
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-s.done:
	case <-ctx.Done():
	}
	s.runOnClose()
	return s.closeErr
}

// KillTree escalates straight to a hard kill of the process tree.
func (s *Session) KillTree() error {
	var err error
	if s.proc != nil {
		err = s.proc.KillTree()
	}
	select {
	case <-s.done:
	case <-time.After(killDrainTimeout):
	}
	s.runOnClose()
	return err
}

// abort is the handshake-failure path (unexported: Start owns it).
func (s *Session) abort() {
	s.closeOnce.Do(func() {
		if s.proc != nil {
			s.closeErr = s.proc.KillTree()
		}
		close(s.closing)
	})
	select {
	case <-s.done:
	case <-time.After(killDrainTimeout):
	}
	s.runOnClose()
}

// finish closes the event channel and marks the session done. It runs on the
// framing goroutine, after the exit event.
func (s *Session) finish() {
	s.emitMu.Lock()
	s.emitClosed = true
	close(s.events)
	s.emitMu.Unlock()
	s.log.close()
	close(s.done)
}

// readLoop frames stdout, parses frames into events, and closes the channel
// after the process-exit events.
func (s *Session) readLoop() {
	s.scan(s.proc.Stdout())
	// stdout is exhausted: the child is exiting (or gone). Reap it, then let
	// the adapter classify the exit.
	code, err := s.proc.Wait(context.Background())
	stderr := s.proc.Stderr()
	for _, e := range s.adapter.Exit(code, err, stderr) {
		s.emit(e)
	}
	s.exitEvent(code, err, stderr)
	s.finish()
}

// readFailure is a stream error the session reports as an event.
type readFailure struct{ err error }

func (e *readFailure) Error() string { return e.err.Error() }
func (e *readFailure) Unwrap() error { return e.err }

// errFrameTooLarge marks a frame that exceeded MaxFrameBytes and was dropped.
var errFrameTooLarge = errors.New("wire: frame too large")

// scan frames NDJSON lines into events until r ends.
func (s *Session) scan(r io.Reader) {
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		line, err := s.readFrame(br)
		switch {
		case errors.Is(err, errFrameTooLarge):
			s.emit(Event{
				Type: EventError, Code: "frame_too_large",
				Error: fmt.Sprintf("wire: frame exceeds %d bytes and was discarded", s.maxFrame),
			})
			continue
		case err != nil:
			if !errors.Is(err, io.EOF) {
				s.emit(Event{Type: EventError, Code: "read_failed", Error: err.Error()})
			}
			return
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		s.log.write("in", bytes.TrimSpace(line))
		for _, e := range s.adapter.Parse(line) {
			if len(e.Raw) == 0 {
				e.Raw = append([]byte(nil), line...)
			}
			s.emit(e)
		}
	}
}

// readFrame returns one newline-terminated frame. A frame longer than the cap
// is dropped whole and reported as errFrameTooLarge, so the next frame still
// parses. A final line without a newline is returned as-is.
func (s *Session) readFrame(br *bufio.Reader) ([]byte, error) {
	buf := make([]byte, 0, 4096)
	overflow := false
	for {
		chunk, err := br.ReadSlice('\n')
		if !overflow {
			if len(buf)+len(chunk) > s.maxFrame {
				overflow = true
				buf = nil
			} else {
				buf = append(buf, chunk...)
			}
		}
		switch {
		case err == nil:
			if overflow {
				return nil, errFrameTooLarge
			}
			return buf, nil
		case errors.Is(err, bufio.ErrBufferFull):
			// The line continues past the read buffer; keep going.
		case errors.Is(err, io.EOF):
			if overflow {
				return nil, errFrameTooLarge
			}
			if len(buf) > 0 {
				return buf, nil
			}
			return nil, io.EOF
		default:
			return nil, &readFailure{err: err}
		}
	}
}

// exitEvent always reports the process death after any classified events.
func (s *Session) exitEvent(code int, err error, stderr string) {
	ev := Event{Type: EventExit, ExitCode: &code}
	if err != nil {
		ev.Error = err.Error()
	} else if code != 0 {
		ev.Error = proc.FirstLine(stderr)
		ev.Code = "process_exited"
	}
	s.recordErr(err)
	s.emit(ev)
}

// jsonLine marshals v into one NDJSON frame.
func jsonLine(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode frame: %w", err)
	}
	return append(b, '\n'), nil
}

// JSONLine marshals v into one newline-terminated NDJSON frame.
func JSONLine(v any) ([]byte, error) { return jsonLine(v) }
