package agentwire

import (
	"context"
	"slices"
	"testing"
	"time"
)

// rtTestFakeScript emits one frame of each vocabulary kind and exits cleanly.
const rtTestFakeScript = `echo '{"type":"init","session_id":"s1"}'
echo '{"type":"assistant","text":"hello"}'
echo '{"type":"tool","id":"t1","name":"Bash","status":"started"}'
echo '{"type":"tool","id":"t1","name":"Bash","status":"completed"}'
echo '{"type":"permission","id":"p1","tool":"Bash","question":"run it?"}'
echo '{"type":"result","subtype":"success","text":"done"}'`

// rtTestCollectEvents drains a session until its event channel closes.
func rtTestCollectEvents(t *testing.T, s Session) []Event {
	t.Helper()
	var out []Event
	deadline := time.After(15 * time.Second)
	for {
		select {
		case e, ok := <-s.Events():
			if !ok {
				return out
			}
			out = append(out, e)
		case <-deadline:
			t.Fatalf("timed out with events %v", rtTestEventTypes(out))
		}
	}
}

// rtTestEventTypes lists the event types of a transcript, for failure output.
func rtTestEventTypes(events []Event) []EventType {
	out := make([]EventType, 0, len(events))
	for _, e := range events {
		out = append(out, e.Type)
	}
	return out
}

// TestFakeDriverScriptEventOrder covers the scripted transcript: each frame
// becomes its event, in order, and EventExit is last.
func TestFakeDriverScriptEventOrder(t *testing.T) {
	rtTestSkipWithoutShell(t)
	sess, err := FakeDriver{Script: rtTestFakeScript}.Start(context.Background(), StartRequest{Harness: Fake})
	if err != nil {
		t.Fatalf("start the fake session: %v", err)
	}
	t.Cleanup(func() { rtTestCloseSession(t, sess) })

	events := rtTestCollectEvents(t, sess)
	want := []EventType{EventInit, EventAssistant, EventTool, EventTool, EventPermission, EventResult, EventExit}
	if got := rtTestEventTypes(events); !slices.Equal(got, want) {
		t.Fatalf("event types = %v, want %v", got, want)
	}
	if got := sess.ID(); got != "s1" {
		t.Fatalf("session id = %q, want s1", got)
	}
	if events[1].Text != "hello" || events[1].SessionID != "s1" {
		t.Fatalf("assistant event = %+v, want the scripted text", events[1])
	}
	if tool := events[2].Tool; tool == nil || tool.ID != "t1" || tool.Name != "Bash" || tool.Status != "started" {
		t.Fatalf("tool start event = %+v", events[2].Tool)
	}
	if tool := events[3].Tool; tool == nil || tool.ID != "t1" || tool.Status != "completed" {
		t.Fatalf("tool completion event = %+v", events[3].Tool)
	}
	if perm := events[4].Permission; perm == nil || perm.ID != "p1" || perm.Tool != "Bash" {
		t.Fatalf("permission event = %+v", events[4].Permission)
	}
	if res := events[5].Result; res == nil || res.Subtype != "success" || res.IsError {
		t.Fatalf("result event = %+v", events[5].Result)
	}
	last := events[len(events)-1]
	if last.Type != EventExit || last.ExitCode == nil || *last.ExitCode != 0 {
		t.Fatalf("last event = %+v, want a clean exit", last)
	}
}

// TestFakeDriverEnvScript covers the environment override: an empty Script
// takes the script from AGENTWIRE_FAKE_SCRIPT.
func TestFakeDriverEnvScript(t *testing.T) {
	rtTestSkipWithoutShell(t)
	const script = `echo '{"type":"init","session_id":"env-1"}'
echo '{"type":"assistant","text":"from-env"}'
echo '{"type":"result","subtype":"success"}'`
	t.Setenv(FakeScriptEnv, script)

	sess, err := FakeDriver{}.Start(context.Background(), StartRequest{Harness: Fake})
	if err != nil {
		t.Fatalf("start the fake session: %v", err)
	}
	t.Cleanup(func() { rtTestCloseSession(t, sess) })

	events := rtTestCollectEvents(t, sess)
	want := []EventType{EventInit, EventAssistant, EventResult, EventExit}
	if got := rtTestEventTypes(events); !slices.Equal(got, want) {
		t.Fatalf("event types = %v, want %v", got, want)
	}
	if events[0].SessionID != "env-1" || events[1].Text != "from-env" {
		t.Fatalf("env script events = %+v", events)
	}
}

// TestFakeDriverNonZeroExit covers the failure path: a script that exits
// non-zero produces an error event and a non-zero EventExit.
func TestFakeDriverNonZeroExit(t *testing.T) {
	rtTestSkipWithoutShell(t)
	sess, err := FakeDriver{Script: "exit 3"}.Start(context.Background(), StartRequest{Harness: Fake})
	if err != nil {
		t.Fatalf("start the fake session: %v", err)
	}
	t.Cleanup(func() { rtTestCloseSession(t, sess) })

	events := rtTestCollectEvents(t, sess)
	if len(events) == 0 {
		t.Fatal("no events for a failing script")
	}
	last := events[len(events)-1]
	if last.Type != EventExit {
		t.Fatalf("last event = %q, want exit: %v", last.Type, rtTestEventTypes(events))
	}
	if last.ExitCode == nil || *last.ExitCode == 0 {
		t.Fatalf("exit event = %+v, want a non-zero exit code", last)
	}
	var sawError bool
	for _, e := range events {
		if e.Type != EventError {
			continue
		}
		sawError = true
		if e.ExitCode == nil || *e.ExitCode != 3 {
			t.Fatalf("error event exit code = %v, want 3", e.ExitCode)
		}
	}
	if !sawError {
		t.Fatalf("no EventError for a non-zero exit: %v", rtTestEventTypes(events))
	}
}

// TestFakeDriverOverridesHarnessOnRuntime covers the substitution point: a
// FakeDriver replaces the built-in driver for a harness, and Runtime.Start
// then drives the fake.
func TestFakeDriverOverridesHarnessOnRuntime(t *testing.T) {
	rtTestSkipWithoutShell(t)
	rt := New(Options{})
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	if d, ok := rt.Driver(Claude); !ok || d.Name() != string(Claude) {
		t.Fatalf("built-in driver = %v ok=%v, want the claude driver", d, ok)
	}
	rt.SetDriver(Claude, FakeDriver{Script: rtTestFakeScript})
	if d, ok := rt.Driver(Claude); !ok || d.Name() != string(Fake) {
		t.Fatalf("overridden driver = %v ok=%v, want the fake", d, ok)
	}

	sess, err := rt.Start(context.Background(), StartRequest{Harness: Claude})
	if err != nil {
		t.Fatalf("Start through the fake override: %v", err)
	}
	t.Cleanup(func() { rtTestCloseSession(t, sess) })
	events := rtTestCollectEvents(t, sess)
	if len(events) < 2 || events[0].Type != EventInit || events[1].Text != "hello" {
		t.Fatalf("Runtime.Start did not drive the fake script: %v", rtTestEventTypes(events))
	}
	if sess.Provider() != string(Fake) {
		t.Fatalf("session provider = %q, want fake", sess.Provider())
	}
}
