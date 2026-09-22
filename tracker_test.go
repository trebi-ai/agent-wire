package agentwire

import (
	"testing"
	"time"
)

// TestTrackerStartsIdlePromptRunsObservesExit covers the state ladder: a new
// tracker is idle, a written prompt makes the turn active, and the exit event
// ends the turn.
func TestTrackerStartsIdlePromptRunsObservesExit(t *testing.T) {
	t.Parallel()
	tr := NewTracker()
	if got := tr.Snapshot().State; got != StatusIdle {
		t.Fatalf("new tracker state = %q, want idle", got)
	}
	tr.PromptWritten()
	if snap := tr.Snapshot(); snap.State != StatusRunning || !snap.TurnActive {
		t.Fatalf("after PromptWritten: %+v", snap)
	}
	snap := tr.Observe(Event{Type: EventAssistant, Text: "working"})
	if snap.State != StatusRunning || !snap.Progressing || snap.Stalled {
		t.Fatalf("during the turn: %+v", snap)
	}
	if snap.LastEventKind != EventAssistant {
		t.Fatalf("LastEventKind = %q, want assistant", snap.LastEventKind)
	}
	snap = tr.Observe(Event{Type: EventExit})
	if !snap.Exited {
		t.Fatalf("EventExit did not set Exited: %+v", snap)
	}
	if snap.ExitedAt.IsZero() {
		t.Fatalf("EventExit did not set ExitedAt: %+v", snap)
	}
	if snap.State != StatusIdle || snap.TurnActive {
		t.Fatalf("state after EventExit = %q, want idle: %+v", snap.State, snap)
	}
}

// TestTrackerUnnamedToolsKeepSeparateSlots covers the Part G.9 fix: a tool
// event with an empty wire id must not overwrite another unnamed tool.
func TestTrackerUnnamedToolsKeepSeparateSlots(t *testing.T) {
	t.Parallel()
	tr := NewTracker()
	tr.PromptWritten()
	for _, name := range []string{"read", "grep"} {
		tr.Observe(Event{Type: EventTool, Tool: &ToolEvent{Name: name, Status: "started"}})
	}
	if got := tr.InflightCount(); got != 2 {
		t.Fatalf("inflight after two unnamed starts = %d, want 2", got)
	}
	snap := tr.Observe(Event{Type: EventTool, Tool: &ToolEvent{Name: "read", Status: "completed"}})
	if snap.InflightTools != 1 {
		t.Fatalf("inflight after one unnamed completion = %d, want 1", snap.InflightTools)
	}
	if !snap.Progressing {
		t.Fatalf("an in-flight tool must keep the turn progressing: %+v", snap)
	}
	// A named tool owns its own slot, so a completion pairs by id and never
	// steals an unnamed one.
	tr.Observe(Event{Type: EventTool, Tool: &ToolEvent{ID: "t1", Name: "Bash", Status: "started"}})
	if got := tr.InflightCount(); got != 2 {
		t.Fatalf("inflight with a named tool = %d, want 2", got)
	}
	tr.Observe(Event{Type: EventTool, Tool: &ToolEvent{ID: "t1", Name: "Bash", Status: "completed"}})
	if got := tr.InflightCount(); got != 1 {
		t.Fatalf("named completion cleared the wrong slot: inflight = %d, want 1", got)
	}
	tr.Observe(Event{Type: EventTool, Tool: &ToolEvent{Name: "grep", Status: "completed"}})
	if got := tr.InflightCount(); got != 0 {
		t.Fatalf("inflight after every completion = %d, want 0", got)
	}
}

// TestTrackerStallBoundary covers the stall window: the tracker reports a
// stall only when the turn is active, no tool is in flight, and the last event
// is older than StallThreshold.
func TestTrackerStallBoundary(t *testing.T) {
	t.Parallel()
	tr := NewTracker()
	tr.PromptWritten()
	stale := time.Now().Add(-2 * StallThreshold)
	snap := tr.Observe(Event{Type: EventAssistant, Text: "slow", At: stale})
	if !snap.Stalled {
		t.Fatalf("no stall after %s of silence: %+v", 2*StallThreshold, snap)
	}
	if snap.Progressing {
		t.Fatalf("a stalled snapshot must not report progress: %+v", snap)
	}
	if want := stale.Add(StallThreshold); !snap.StallSince.Equal(want) {
		t.Fatalf("StallSince = %s, want %s", snap.StallSince, want)
	}
	// The window is a property of the tracker, not of one Observe call.
	if again := tr.Snapshot(); !again.Stalled || !again.StallSince.Equal(stale.Add(StallThreshold)) {
		t.Fatalf("Snapshot lost the stall: %+v", again)
	}
}

// TestTrackerStallNeedsNoInflightTool covers the other half of the rule: an
// in-flight tool suppresses the stall, and a fresh progress event clears one.
func TestTrackerStallNeedsNoInflightTool(t *testing.T) {
	t.Parallel()
	tr := NewTracker()
	tr.PromptWritten()
	stale := time.Now().Add(-2 * StallThreshold)
	tr.Observe(Event{Type: EventTool, Tool: &ToolEvent{ID: "t1", Name: "Bash", Status: "started"}, At: stale})
	if snap := tr.Snapshot(); snap.Stalled || !snap.Progressing {
		t.Fatalf("an in-flight tool must suppress the stall: %+v", snap)
	}
	// The completion lands on the same stale instant, so the window is real
	// now that nothing is in flight.
	snap := tr.Observe(Event{Type: EventTool, Tool: &ToolEvent{ID: "t1", Name: "Bash", Status: "completed"}, At: stale})
	if snap.InflightTools != 0 || !snap.Stalled {
		t.Fatalf("no stall after the last tool completed: %+v", snap)
	}
	// A usage event is progress and clears the window.
	snap = tr.Observe(Event{Type: EventUsage, Usage: &Usage{Input: 10}})
	if snap.Stalled || !snap.Progressing {
		t.Fatalf("progress did not clear the stall: %+v", snap)
	}
}

// TestTrackerPermissionLifecycle covers the waiting state: a permission event
// suspends the turn, and a decision resumes it.
func TestTrackerPermissionLifecycle(t *testing.T) {
	t.Parallel()
	tr := NewTracker()
	tr.PromptWritten()
	snap := tr.Observe(Event{Type: EventPermission, Permission: &Permission{ID: "p1", Tool: "Bash"}})
	if snap.State != StatusWaiting || snap.OpenApprovals != 1 {
		t.Fatalf("permission did not suspend the turn: %+v", snap)
	}
	if snap.WaitingSince.IsZero() {
		t.Fatalf("WaitingSince not set: %+v", snap)
	}
	if got := tr.OldestApprovalID(); got != "p1" {
		t.Fatalf("OldestApprovalID = %q, want p1", got)
	}
	snap = tr.PermissionDecided("p1")
	if snap.State != StatusRunning || snap.OpenApprovals != 0 {
		t.Fatalf("the decision did not resume the turn: %+v", snap)
	}
	if !snap.WaitingSince.IsZero() {
		t.Fatalf("WaitingSince not cleared: %+v", snap)
	}
}

// TestTrackerPermissionDecidedEmptyIDClearsOldest covers the fallback: a
// decision with no id clears the oldest open approval.
func TestTrackerPermissionDecidedEmptyIDClearsOldest(t *testing.T) {
	t.Parallel()
	tr := NewTracker()
	tr.PromptWritten()
	tr.Observe(Event{Type: EventPermission, Permission: &Permission{ID: "p1", Tool: "Bash"}})
	time.Sleep(2 * time.Millisecond)
	tr.Observe(Event{Type: EventPermission, Permission: &Permission{ID: "p2", Tool: "Write"}})
	snap := tr.PermissionDecided("")
	if snap.OpenApprovals != 1 {
		t.Fatalf("empty id left %d approvals open, want 1", snap.OpenApprovals)
	}
	if got := tr.OldestApprovalID(); got != "p2" {
		t.Fatalf("OldestApprovalID after the empty-id decision = %q, want p2", got)
	}
}

// TestTrackerPermissionAddedRegistersHookApproval covers an approval the wire
// did not open, and its removal.
func TestTrackerPermissionAddedRegistersHookApproval(t *testing.T) {
	t.Parallel()
	tr := NewTracker()
	tr.PromptWritten()
	tr.Observe(Event{Type: EventPermission, Permission: &Permission{ID: "wire-1"}})
	tr.PermissionAdded("hook-1")
	if snap := tr.Snapshot(); snap.OpenApprovals != 2 {
		t.Fatalf("open approvals = %d, want 2", snap.OpenApprovals)
	}
	if snap := tr.PermissionDecided("wire-1"); snap.OpenApprovals != 1 || snap.State != StatusWaiting {
		t.Fatalf("the wire decision did not leave the hook approval open: %+v", snap)
	}
	if snap := tr.PermissionDecided("hook-1"); snap.OpenApprovals != 0 || snap.State != StatusRunning {
		t.Fatalf("state after every decision: %+v", snap)
	}
}
