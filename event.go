package agentwire

import "github.com/trebi-ai/agent-wire/internal/wire"

// The event vocabulary is declared in internal/wire, which owns framing and
// cannot import this package. These aliases make it part of the public API.

// Event is one normalized wire event from a vendor session.
type Event = wire.Event

// EventType is the normalized wire vocabulary.
type EventType = wire.EventType

// ToolEvent is one tool invocation (start or completion).
type ToolEvent = wire.ToolEvent

// ToolKind is the coarse class of a tool invocation.
type ToolKind = wire.ToolKind

// Permission is a vendor permission request awaiting a decision.
type Permission = wire.Permission

// Decision answers a Permission.
type Decision = wire.Decision

// Usage is token/cost accounting in vendor units.
type Usage = wire.Usage

// Result is the terminal turn payload.
type Result = wire.Result

// Prompt is one user turn written to a live session.
type Prompt = wire.Prompt

// Attachment is one out-of-band prompt payload (images first).
type Attachment = wire.Attachment

// SessionStatus is the activity view of a session.
type SessionStatus = wire.SessionStatus

// Event types.
const (
	EventInit       = wire.EventInit
	EventAssistant  = wire.EventAssistant
	EventThought    = wire.EventThought
	EventUser       = wire.EventUser
	EventTool       = wire.EventTool
	EventPermission = wire.EventPermission
	EventResult     = wire.EventResult
	EventError      = wire.EventError
	EventUsage      = wire.EventUsage
	EventStatus     = wire.EventStatus
	EventExit       = wire.EventExit
)

// Session statuses.
const (
	StatusUnknown = wire.StatusUnknown
	StatusRunning = wire.StatusRunning
	StatusWaiting = wire.StatusWaiting
	StatusIdle    = wire.StatusIdle
)

// Tool kinds.
const (
	ToolRead   = wire.ToolRead
	ToolEdit   = wire.ToolEdit
	ToolExec   = wire.ToolExec
	ToolMCP    = wire.ToolMCP
	ToolSearch = wire.ToolSearch
	ToolOther  = wire.ToolOther
)
