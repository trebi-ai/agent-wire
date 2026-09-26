package agentwire

import (
	"fmt"
	"sync"
	"time"
)

// StallThreshold is the "running but no events and no in-flight tools" window
// that raises a stall. It is observability only; the consumer's max duration
// is the killer.
const StallThreshold = 5 * time.Minute

// Tracker is the per-run activity tracker. Wire events are the only input; a
// consumer reads snapshots instead of scraping a terminal.
type Tracker struct {
	mu            sync.Mutex
	lastEventAt   time.Time
	turnActive    bool
	inflight      map[string]time.Time
	openApprovals map[string]time.Time
	lastResult    *Result
	idleSince     time.Time
	waitingSince  time.Time
	stallSince    time.Time
	startedAt     time.Time
	lastEventKind EventType
	lastToolName  string
	exited        bool
	autoSeq       int
	pendingAuto   []string
}

// Snapshot is the derived state a consumer and its reaper read.
type Snapshot struct {
	State         SessionStatus
	TurnActive    bool
	Progressing   bool
	Stalled       bool
	StallSince    time.Time
	InflightTools int
	OpenApprovals int
	LastEventAt   time.Time
	LastEventKind EventType
	LastToolName  string
	IdleSince     time.Time
	WaitingSince  time.Time
	LastResult    *Result
	// Exited reports that the session's process ended.
	Exited bool
	// ExitedAt is the moment the exit event landed.
	ExitedAt time.Time
}

// NewTracker builds an empty tracker.
func NewTracker() *Tracker {
	now := time.Now()
	return &Tracker{
		lastEventAt:   now,
		inflight:      map[string]time.Time{},
		openApprovals: map[string]time.Time{},
		startedAt:     now,
	}
}

// PromptWritten marks the optimistic running state when a prompt is written:
// model latency is not idleness.
func (t *Tracker) PromptWritten() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.turnActive = true
	t.lastEventAt = time.Now()
	t.lastResult = nil
	t.idleSince = time.Time{}
	t.stallSince = time.Time{}
}

// Observe folds one event into the tracker and returns the derived snapshot.
func (t *Tracker) Observe(e Event) Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	if e.At.IsZero() {
		e.At = now
	}
	t.lastEventAt = e.At
	t.lastEventKind = e.Type
	switch e.Type {
	case EventAssistant, EventUser, EventUsage, EventStatus:
		// Progress: clears a stall window.
		t.stallSince = time.Time{}
	case EventInit:
		// Init alone is not turn progress.
	case EventPermission:
		if e.Permission != nil {
			t.openApprovals[e.Permission.ID] = now
		} else {
			t.openApprovals[""] = now
		}
		t.waitingSince = now
	case EventTool:
		t.observeTool(e.Tool, now)
	case EventResult:
		t.turnActive = false
		t.stallSince = time.Time{}
		// The turn is over: nothing can still be running (plan
		// 2026-09-26 B). A leaked phantom id must not hold stall
		// detection open for the rest of the session.
		clear(t.inflight)
		t.pendingAuto = nil
		if e.Result != nil {
			r := *e.Result
			t.lastResult = &r
		} else {
			t.lastResult = &Result{}
		}
		t.idleSince = now
	case EventExit:
		t.exited = true
		t.turnActive = false
		clear(t.inflight)
		t.pendingAuto = nil
		t.idleSince = now
	case EventError:
		if e.ExitCode != nil || e.EndReason != "" || e.Error != "" {
			// A classified failure ends the turn from the tracker's view.
			t.turnActive = false
		}
	}
	return t.snapshotLocked(now)
}

// observeTool folds one tool event. A tool with no wire id gets a synthetic id
// so a second unnamed tool cannot overwrite the first one's entry.
func (t *Tracker) observeTool(tool *ToolEvent, now time.Time) {
	if tool == nil {
		return
	}
	t.lastToolName = tool.Name
	id := tool.ID
	switch tool.Status {
	case "completed", "failed":
		if id == "" {
			id = t.takePendingAuto()
		}
		delete(t.inflight, id)
	default:
		if id == "" {
			t.autoSeq++
			id = fmt.Sprintf("auto-%d", t.autoSeq)
			t.pendingAuto = append(t.pendingAuto, id)
		}
		t.inflight[id] = now
	}
}

// takePendingAuto pairs an unnamed completion with the oldest unnamed start.
func (t *Tracker) takePendingAuto() string {
	if len(t.pendingAuto) == 0 {
		// A completion with no matching start still needs its own slot, so it
		// cannot clear a named tool.
		t.autoSeq++
		return fmt.Sprintf("auto-%d", t.autoSeq)
	}
	id := t.pendingAuto[0]
	t.pendingAuto = t.pendingAuto[1:]
	return id
}

// PermissionDecided clears an open approval. An empty id clears the oldest
// one (approvals that arrived without a wire id have none).
func (t *Tracker) PermissionDecided(id string) Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	if id == "" {
		var oldest string
		var at time.Time
		for k, v := range t.openApprovals {
			if oldest == "" || v.Before(at) {
				oldest, at = k, v
			}
		}
		delete(t.openApprovals, oldest)
	} else {
		delete(t.openApprovals, id)
	}
	if len(t.openApprovals) == 0 {
		t.waitingSince = time.Time{}
	}
	return t.snapshotLocked(time.Now())
}

// PermissionAdded registers an approval the wire did not open.
func (t *Tracker) PermissionAdded(id string) Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	t.openApprovals[id] = now
	t.waitingSince = now
	return t.snapshotLocked(now)
}

// OldestApprovalID returns the oldest open wire permission id, or "" when the
// pending approval has none.
func (t *Tracker) OldestApprovalID() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var oldest string
	var at time.Time
	for k, v := range t.openApprovals {
		if k == "" {
			continue
		}
		if oldest == "" || v.Before(at) {
			oldest, at = k, v
		}
	}
	return oldest
}

// Snapshot returns the derived state.
func (t *Tracker) Snapshot() Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.snapshotLocked(time.Now())
}

// InflightCount reports tool invocations without a matching result.
func (t *Tracker) InflightCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.inflight)
}

func (t *Tracker) snapshotLocked(now time.Time) Snapshot {
	inWaiting := len(t.openApprovals) > 0
	state := StatusIdle
	switch {
	case inWaiting:
		state = StatusWaiting
	case t.turnActive:
		state = StatusRunning
	}
	snap := Snapshot{
		State:         state,
		TurnActive:    t.turnActive,
		InflightTools: len(t.inflight),
		OpenApprovals: len(t.openApprovals),
		LastEventAt:   t.lastEventAt,
		LastEventKind: t.lastEventKind,
		LastToolName:  t.lastToolName,
		IdleSince:     t.idleSince,
		WaitingSince:  t.waitingSince,
		LastResult:    t.lastResult,
		Exited:        t.exited,
	}
	if t.exited {
		snap.ExitedAt = t.lastEventAt
	}
	if t.turnActive {
		gap := now.Sub(t.lastEventAt)
		if len(t.inflight) > 0 || gap < StallThreshold {
			snap.Progressing = true
			if len(t.inflight) > 0 {
				snap.StallSince = time.Time{}
				t.stallSince = time.Time{}
			}
		} else {
			if t.stallSince.IsZero() {
				t.stallSince = t.lastEventAt.Add(StallThreshold)
			}
			snap.Stalled = true
			snap.StallSince = t.stallSince
		}
	}
	if snap.State == StatusIdle && t.idleSince.IsZero() && !t.startedAt.IsZero() {
		snap.IdleSince = t.lastEventAt
	}
	return snap
}
