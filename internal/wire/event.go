package wire

import (
	"encoding/json"
	"time"
)

// EventType is the normalized wire vocabulary. One vendor turn emits status,
// progress, permissions, and exactly one terminal result.
type EventType string

const (
	// EventInit reports the vendor session is live (system/init, thread id).
	EventInit EventType = "init"
	// EventAssistant is assistant text; Delta=true means a stream delta.
	EventAssistant EventType = "assistant"
	// EventThought is reasoning text the model surfaces separately from the
	// answer; Delta works as for EventAssistant.
	EventThought EventType = "thought"
	// EventUser echoes a user turn (prompt write).
	EventUser EventType = "user"
	// EventTool is a tool lifecycle event (started/completed/failed).
	EventTool EventType = "tool"
	// EventPermission is a human decision request (can_use_tool, approvals).
	EventPermission EventType = "permission"
	// EventResult is the terminal turn event. Exactly one per turn.
	EventResult EventType = "result"
	// EventError is a classified failure (wire or process).
	EventError EventType = "error"
	// EventUsage carries token/cost counters.
	EventUsage EventType = "usage"
	// EventStatus is a coarse session status signal (busy/idle/compaction).
	EventStatus EventType = "status"
	// EventExit reports the child process ended. It is always the last event.
	EventExit EventType = "exit"
	// EventLimits carries the plan limit windows of the account.
	EventLimits EventType = "limits"
)

// SessionStatus is the activity view of a session.
type SessionStatus string

const (
	StatusUnknown SessionStatus = ""
	StatusRunning SessionStatus = "running"
	StatusWaiting SessionStatus = "waiting"
	StatusIdle    SessionStatus = "idle"
)

// ToolKind is the coarse class of a tool invocation. Consumers use it to
// decide what to render or auto-approve without parsing vendor tool names.
type ToolKind string

const (
	ToolRead   ToolKind = "read"
	ToolEdit   ToolKind = "edit"
	ToolExec   ToolKind = "exec"
	ToolMCP    ToolKind = "mcp"
	ToolSearch ToolKind = "search"
	ToolOther  ToolKind = "other"
)

// Event is one normalized wire event from a vendor session.
type Event struct {
	Type EventType
	At   time.Time

	// Turn counts completed user turns. It is 1 for the first turn.
	Turn int

	// SessionID carries the vendor session/thread id on init and result.
	SessionID string

	// Text is assistant/user text or a short human summary.
	Text string
	// Delta marks Text as a stream delta rather than a complete block.
	Delta bool

	Tool       *ToolEvent
	Permission *Permission
	Result     *Result
	Usage      *Usage
	Limits     *Limits
	// Status is the tracker hint of a status event.
	Status SessionStatus

	// Error is a human-readable failure for EventError/EventExit.
	Error string
	// Code is the stable machine code of a failure (for example
	// "frame_too_large", "process_exited"). Consumers map it to their own
	// text; the library never ships operator prose.
	Code string
	// EndReason is the classified failure family (auth|limit|entitlement|...).
	EndReason string
	// ExitCode is set on EventExit.
	ExitCode *int

	// Raw is the vendor frame, kept for fixtures and the run log.
	Raw []byte
}

// ToolEvent is one tool invocation (start or completion).
type ToolEvent struct {
	ID     string
	Name   string
	Kind   ToolKind
	Input  string
	Output string
	Status string // started|completed|failed
	// Paths lists the files an edit touches, where the protocol says so.
	Paths    []string
	ExitCode *int
}

// Permission is a vendor permission request awaiting a decision.
type Permission struct {
	ID       string
	Tool     string
	Kind     ToolKind
	Question string
	Input    string
	Options  []string
	// ToolUseID is the tool_use id the request belongs to, when the vendor
	// carries it. A consumer joins an approval card to its tool chip.
	ToolUseID string
}

// Decision answers a Permission. Message is shown to the model on deny.
type Decision struct {
	Allow        bool
	Message      string
	UpdatedInput json.RawMessage
}

// Usage is token/cost accounting (vendor units).
type Usage struct {
	Input         int64
	Output        int64
	CacheRead     int64
	CacheCreation int64
	Model         string
	CostUSD       float64
}

// Window keys of a LimitWindow.
const (
	WindowFiveHour = "five_hour"
	WindowDaily    = "daily"
	WindowWeekly   = "weekly"
	WindowMonthly  = "monthly"
	WindowCustom   = "custom"
)

// LimitWindow is the use of one plan limit window.
type LimitWindow struct {
	// Key is five_hour, daily, weekly, monthly, or custom.
	Key string
	// Scope narrows the window, for example "opus" or a Codex limit id.
	Scope string
	// Minutes is the window length. It is 0 when not known.
	Minutes int
	// UsedPercent is 0–100.
	UsedPercent float64
	// ResetsAt is zero when not known.
	ResetsAt time.Time
}

// Limits is the plan limit state of the account behind a session.
type Limits struct {
	// Plan is the vendor plan name, for example "plus". Empty when not known.
	Plan string
	// Rejected reports that the harness refused the last request on a limit.
	Rejected bool
	Windows  []LimitWindow
}

// WindowKey maps a window length in minutes to its key.
func WindowKey(minutes int) string {
	switch minutes {
	case 300:
		return WindowFiveHour
	case 1440:
		return WindowDaily
	case 10080:
		return WindowWeekly
	case 43200:
		return WindowMonthly
	}
	return WindowCustom
}

// Result is the terminal turn payload. Subtype and IsError come from the
// vendor; EndReason is the classified family when the result is an error.
type Result struct {
	Subtype   string
	IsError   bool
	Text      string
	SessionID string
	Usage     Usage
	EndReason string
	// Code is the classified machine code of an error result.
	Code string
}

// Attachment is one out-of-band prompt payload. Data wins over Path when both
// are set. Images are sent before text.
type Attachment struct {
	// MIME is the media type, for example "image/png".
	MIME string
	// Path is a file on disk the library reads and encodes itself.
	Path string
	// Data is the raw bytes, used when Path is empty.
	Data []byte
}

// Prompt is one user turn written to a live session.
type Prompt struct {
	Text string
	// Kind is the origin: job|assistant|agent|memory_fix|followup|wake. The
	// library never interprets it; it is carried for consumer log lines.
	Kind string
	// Attachments are images or files sent with the turn, images first.
	Attachments []Attachment
}
