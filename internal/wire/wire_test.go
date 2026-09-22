package wire

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// TestMain fails the package when any test leaks a goroutine.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// testAdapter is a scripted codec for the plumbing tests. Its vocabulary is a
// tiny JSON set: assistant, result, error, and init frames.
type testAdapter struct {
	handshakeErr error
	handshake    [][]byte
}

// Handshake writes the prepared frames and returns the scripted error.
func (a *testAdapter) Handshake(ctx context.Context, w *Writer) ([]Event, error) {
	if a.handshakeErr != nil {
		return nil, a.handshakeErr
	}
	for _, frame := range a.handshake {
		if err := w.Write(frame); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

// EncodePrompt renders one user turn.
func (a *testAdapter) EncodePrompt(p Prompt) ([]byte, error) {
	return jsonLine(map[string]string{"type": "user", "text": p.Text})
}

// Parse maps one stdout line to events.
func (a *testAdapter) Parse(line []byte) []Event {
	var m struct {
		Type  string `json:"type"`
		Text  string `json:"text"`
		Error string `json:"error"`
		Code  string `json:"code"`
		ID    string `json:"id"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(line), &m); err != nil {
		return []Event{{Type: EventError, Code: "parse_failed", Error: err.Error()}}
	}
	switch m.Type {
	case "assistant":
		return []Event{{Type: EventAssistant, Text: m.Text}}
	case "result":
		return []Event{{Type: EventResult, Result: &Result{Text: m.Text}}}
	case "error":
		return []Event{{Type: EventError, Error: m.Error, Code: m.Code}}
	case "init":
		return []Event{{Type: EventInit, SessionID: m.ID}}
	default:
		return nil
	}
}

// EncodeDecision answers one permission request.
func (a *testAdapter) EncodeDecision(id string, d Decision) ([]byte, error) {
	return jsonLine(map[string]any{"type": "decision", "id": id, "allow": d.Allow})
}

// Exit maps the process exit to events. The wire exit event carries it.
func (a *testAdapter) Exit(code int, err error, stderr string) []Event { return nil }

// collectEvents drains the event channel until it closes or the timeout ends.
// ok is false when the timeout ends first. It is safe to call from any
// goroutine.
func collectEvents(s *Session, timeout time.Duration) (got []Event, ok bool) {
	deadline := time.After(timeout)
	for {
		select {
		case e, chOpen := <-s.Events():
			if !chOpen {
				return got, true
			}
			got = append(got, e)
		case <-deadline:
			return got, false
		}
	}
}

// collect drains the event channel and fails the test on a timeout.
func collect(t *testing.T, s *Session) []Event {
	t.Helper()
	got, ok := collectEvents(s, 30*time.Second)
	if !ok {
		t.Fatalf("timed out waiting for the event channel to close; got %d events", len(got))
	}
	return got
}

// waitDone fails the test when Done does not close in time.
func waitDone(t *testing.T, s *Session) {
	t.Helper()
	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done() did not close")
	}
}

// TestFrameTooLargeResyncs proves a dropped frame does not freeze the reader.
func TestFrameTooLargeResyncs(t *testing.T) {
	big := `{"type":"assistant","text":"` + strings.Repeat("a", 2000) + `"}`
	stream := big + "\n" + `{"type":"assistant","text":"after"}` + "\n"
	s := NewMemorySession("test", &testAdapter{}, io.Discard, strings.NewReader(stream),
		context.Background(), Config{MaxFrameBytes: 1024})

	got := collect(t, s)
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2: %+v", len(got), got)
	}
	if got[0].Type != EventError || got[0].Code != "frame_too_large" {
		t.Fatalf("first event is %+v, want an error with code frame_too_large", got[0])
	}
	if got[1].Type != EventAssistant || got[1].Text != "after" {
		t.Fatalf("second event is %+v, want the assistant frame after the dropped one", got[1])
	}
}

// TestFrameTooLargeAtDefaultCap proves a frame past the real 16 MiB cap is
// dropped and the reader resyncs. It also proves the reader does not buffer
// the whole line before it fails.
func TestFrameTooLargeAtDefaultCap(t *testing.T) {
	if 17<<20 <= DefaultMaxFrameBytes {
		t.Fatalf("the test frame is not larger than the default cap %d", DefaultMaxFrameBytes)
	}
	big := `{"type":"assistant","text":"` + strings.Repeat("a", 17<<20) + `"}`
	stream := big + "\n" + `{"type":"assistant","text":"after"}` + "\n"
	s := NewMemorySession("test", &testAdapter{}, io.Discard, strings.NewReader(stream),
		context.Background(), Config{})

	got := collect(t, s)
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2", len(got))
	}
	if got[0].Type != EventError || got[0].Code != "frame_too_large" {
		t.Fatalf("first event is %+v, want frame_too_large", got[0])
	}
	if got[1].Type != EventAssistant || got[1].Text != "after" {
		t.Fatalf("second event is %+v, want the assistant frame after the dropped one", got[1])
	}
}

// TestTruncatedFinalLineParsed proves a last line without a newline is not
// lost.
func TestTruncatedFinalLineParsed(t *testing.T) {
	s := NewMemorySession("test", &testAdapter{}, io.Discard,
		strings.NewReader(`{"type":"assistant","text":"tail"}`), context.Background(), Config{})

	got := collect(t, s)
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1: %+v", len(got), got)
	}
	if got[0].Type != EventAssistant || got[0].Text != "tail" {
		t.Fatalf("event is %+v, want the truncated final line parsed", got[0])
	}
}

// TestDoneAndErrOnErrorEvent proves Done closes after the event channel and
// Err reports an error event.
func TestDoneAndErrOnErrorEvent(t *testing.T) {
	stream := `{"type":"error","error":"boom","code":"kaboom"}` + "\n"
	s := NewMemorySession("test", &testAdapter{}, io.Discard, strings.NewReader(stream),
		context.Background(), Config{})

	got := collect(t, s)
	if len(got) != 1 || got[0].Type != EventError {
		t.Fatalf("got %+v, want one error event", got)
	}
	waitDone(t, s)
	if err := s.Err(); err == nil || err.Error() != "boom" {
		t.Fatalf("Err() is %v, want the first error event", err)
	}
}

// TestFrameLogRecordsBothDirections proves every frame lands in the NDJSON log
// with its direction, and the file mode is private.
func TestFrameLogRecordsBothDirections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "frames.ndjson")
	ad := &testAdapter{handshake: [][]byte{[]byte(`{"type":"user","text":"hello"}` + "\n")}}
	stream := `{"type":"assistant","text":"hi"}` + "\n"
	s := NewMemorySession("test", ad, io.Discard, strings.NewReader(stream),
		context.Background(), Config{LogPath: path})

	collect(t, s)
	waitDone(t, s)
	if err := s.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read frame log: %v", err)
	}
	dirs := map[string]int{}
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var entry struct {
			Dir   string          `json:"dir"`
			Frame json.RawMessage `json:"frame"`
		}
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("log line %q is not NDJSON: %v", line, err)
		}
		if len(entry.Frame) == 0 {
			t.Fatalf("log line %q carries no frame", line)
		}
		dirs[entry.Dir]++
	}
	if dirs["out"] == 0 {
		t.Fatalf("frame log has no out line: %v", dirs)
	}
	if dirs["in"] == 0 {
		t.Fatalf("frame log has no in line: %v", dirs)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat frame log: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("frame log mode is %o, want 600", perm)
	}
}

// TestSetIDAndEventSessionID proves a parsed event carries the session id and
// an event id updates the session.
func TestSetIDAndEventSessionID(t *testing.T) {
	pr, pw := io.Pipe()
	s := NewMemorySession("test", &testAdapter{}, io.Discard, pr, context.Background(), Config{})
	s.SetID("sess-1")
	if s.ID() != "sess-1" {
		t.Fatalf("ID() is %q after SetID", s.ID())
	}
	go func() {
		_, _ = pw.Write([]byte(`{"type":"assistant","text":"a"}` + "\n"))
		_, _ = pw.Write([]byte(`{"type":"init","id":"vendor-9"}` + "\n"))
		_ = pw.Close()
	}()

	got := collect(t, s)
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2: %+v", len(got), got)
	}
	if got[0].SessionID != "sess-1" {
		t.Fatalf("event session id is %q, want the value from SetID", got[0].SessionID)
	}
	if got[1].SessionID != "vendor-9" {
		t.Fatalf("init event session id is %q, want vendor-9", got[1].SessionID)
	}
	if s.ID() != "vendor-9" {
		t.Fatalf("ID() is %q, want the id from the init event", s.ID())
	}
}
