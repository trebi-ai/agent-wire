//go:build unix

package wire

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/trebi-ai/agent-wire/internal/proc"
)

// startSh spawns one shell script as the harness child.
func startSh(t *testing.T, script string) *proc.Proc {
	t.Helper()
	p, err := proc.Start(context.Background(), proc.Opts{Path: "sh", Args: []string{"-c", script}})
	if err != nil {
		t.Fatalf("start sh: %v", err)
	}
	return p
}

// TestExitEventIsLastAfterConcurrentClose proves EventExit survives a Close
// that runs while the consumer drains.
//
// A Close that wins the race is allowed to drop an assistant frame that the
// child had not written yet: the closing select exists to unblock a stalled
// consumer (G.1). The exit event is owed to the consumer either way, so it is
// the one thing this test asserts unconditionally. The child writes its frame
// before it exits, so a dropped frame means the close flush window expired
// first, never that the frame arrived after the exit.
func TestExitEventIsLastAfterConcurrentClose(t *testing.T) {
	p := startSh(t, `printf '{"type":"assistant","text":"late"}\n'; exit 3`)
	s, err := Start(context.Background(), "test", p, &testAdapter{}, Config{})
	if err != nil {
		t.Fatalf("start session: %v", err)
	}

	start := make(chan struct{})
	var (
		got []Event
		ok  bool
		wg  sync.WaitGroup
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		got, ok = collectEvents(s, 30*time.Second)
	}()
	close(start)

	if err := s.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
	wg.Wait()

	if !ok {
		t.Fatalf("timed out draining events; got %d", len(got))
	}
	if len(got) == 0 {
		t.Fatal("Close dropped the exit event")
	}
	last := got[len(got)-1]
	if last.Type != EventExit {
		t.Fatalf("last event is %+v, want EventExit", last)
	}
	if last.ExitCode == nil || *last.ExitCode != 3 {
		t.Fatalf("exit event code is %v, want 3", last.ExitCode)
	}
	// The assistant frame may be dropped by the close, but it can never
	// arrive after the exit event.
	for _, e := range got[:len(got)-1] {
		if e.Type == EventExit {
			t.Fatalf("an exit event is not last: %+v", got)
		}
	}
}

// TestErrReportsNonzeroExit proves Err records a nonzero child exit.
func TestErrReportsNonzeroExit(t *testing.T) {
	p := startSh(t, "exit 1")
	s, err := Start(context.Background(), "test", p, &testAdapter{}, Config{})
	if err != nil {
		t.Fatalf("start session: %v", err)
	}

	got := collect(t, s)
	if len(got) != 1 || got[0].Type != EventExit {
		t.Fatalf("got %+v, want one exit event", got)
	}
	if got[0].ExitCode == nil || *got[0].ExitCode != 1 {
		t.Fatalf("exit event code is %v, want 1", got[0].ExitCode)
	}
	waitDone(t, s)
	if s.Err() == nil {
		t.Fatal("Err() is nil after a nonzero exit")
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestSlowConsumerReachesTheEnd proves a consumer that reads one event at a
// time still sees every frame and the final exit event.
func TestSlowConsumerReachesTheEnd(t *testing.T) {
	const frames = 300
	script := `i=0
while [ "$i" -lt 300 ]; do
  printf '{"type":"assistant","text":"%d"}\n' "$i"
  i=$((i+1))
done`
	p := startSh(t, script)
	s, err := Start(context.Background(), "test", p, &testAdapter{}, Config{})
	if err != nil {
		t.Fatalf("start session: %v", err)
	}

	var (
		assistants int
		last       Event
	)
	deadline := time.After(60 * time.Second)
loop:
	for {
		select {
		case e, chOpen := <-s.Events():
			if !chOpen {
				break loop
			}
			last = e
			if e.Type == EventAssistant {
				assistants++
			}
			time.Sleep(200 * time.Microsecond)
		case <-deadline:
			t.Fatal("timed out draining events")
		}
	}

	if assistants != frames {
		t.Fatalf("saw %d assistant frames, want %d", assistants, frames)
	}
	if last.Type != EventExit {
		t.Fatalf("last event is %+v, want EventExit", last)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestStoppedConsumerDoesNotLeak proves Close frees the framing goroutine when
// the consumer never reads. TestMain enforces the leak check.
func TestStoppedConsumerDoesNotLeak(t *testing.T) {
	script := `i=0
while [ "$i" -lt 300 ]; do
  printf '{"type":"assistant","text":"%d"}\n' "$i"
  i=$((i+1))
done
cat >/dev/null`
	p := startSh(t, script)
	s, err := Start(context.Background(), "test", p, &testAdapter{}, Config{})
	if err != nil {
		t.Fatalf("start session: %v", err)
	}

	// Never read the event channel. Close must still drain the framing loop.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.Close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case <-s.Done():
	default:
		t.Fatal("Done() is not closed after Close")
	}
}

// TestHandshakeFailureKillsChild proves Start returns the handshake error and
// the child is gone.
func TestHandshakeFailureKillsChild(t *testing.T) {
	p := startSh(t, "sleep 30")
	ad := &testAdapter{handshakeErr: errors.New("handshake boom")}

	s, err := Start(context.Background(), "test", p, ad, Config{})
	if err == nil {
		t.Fatal("Start did not return the handshake error")
	}
	if s != nil {
		t.Fatal("Start returned a session on a handshake failure")
	}
	if err.Error() != "handshake boom" {
		t.Fatalf("Start error is %v, want the handshake error", err)
	}
	select {
	case <-p.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("child is still alive after a handshake failure")
	}
	if proc.Alive(p.PID()) {
		t.Fatalf("child pid %d is still alive after a handshake failure", p.PID())
	}
}
