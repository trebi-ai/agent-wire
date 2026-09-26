// Package native is the in-process harness: a provider-agnostic tool loop
// with the same Session and Event model as the CLI drivers (plan
// 2026-09-26 I). A consumer supplies a Model from a provider module, tools,
// skills, a permission policy, and credentials; the package drives the turn
// loop and emits agentwire events.
//
// The root agent-wire module stays free of provider SDKs. The provider
// adapters live in native/provider/*, one nested module each.
package native

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
)

// ErrNotFound is what a Store.Load returns for a session it has no log for.
var ErrNotFound = errors.New("native: no stored session")

// Role labels one message side of the conversation.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is one provider-neutral conversation entry. The loop persists
// these verbatim, so a resume replays exactly, including provider metadata
// such as thinking signatures.
type Message struct {
	Role  Role  `json:"role"`
	Parts []Part `json:"parts"`
}

// Part holds exactly one of the payload fields. A nil-valued union keeps
// JSON round-trips honest: one kind per Part.
type Part struct {
	Text       *TextPart       `json:"text,omitempty"`
	File       *FilePart       `json:"file,omitempty"`
	Reasoning  *ReasoningPart  `json:"reasoning,omitempty"`
	ToolCall   *ToolCall       `json:"tool_call,omitempty"`
	ToolResult *ToolResult     `json:"tool_result,omitempty"`
}

// TextPart is plain text.
type TextPart struct {
	Text string `json:"text"`
}

// FilePart is one attachment. Exactly one of Data or URL is set.
type FilePart struct {
	MediaType string `json:"media_type,omitempty"`
	Data      []byte `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

// ReasoningPart is one reasoning block. ProviderMeta carries opaque
// provider state, such as an Anthropic thinking signature, so a replay is
// valid.
type ReasoningPart struct {
	Text         string          `json:"text"`
	ProviderMeta json.RawMessage `json:"provider_meta,omitempty"`
}

// ToolCall is one model-requested tool invocation.
type ToolCall struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input,omitempty"`
}

// ToolResult is one tool answer fed back to the model.
type ToolResult struct {
	CallID  string `json:"call_id"`
	Name    string `json:"name,omitempty"`
	Content []Part `json:"content"`
	IsError bool   `json:"is_error,omitempty"`
}

// StreamKind labels one streaming part.
type StreamKind int

const (
	StreamTextDelta StreamKind = iota
	StreamReasoningDelta
	StreamToolInputStart
	StreamToolInputDelta
	StreamToolCallDone
	StreamFinish
)

// StreamPart is one element of a Model.Stream sequence.
type StreamPart struct {
	Kind   StreamKind
	// ID is the tool-call id for the tool parts.
	ID string
	// Delta is the text of a text or reasoning delta, or one JSON fragment
	// of a tool input.
	Delta string
	// Call is the complete call on StreamToolCallDone.
	Call *ToolCall
	// Finish terminates the stream.
	Finish *Finish
}

// Stream is the sequence type of Model.Stream. It yields StreamParts and at
// most one error, after which the sequence ends.
type Stream = Seq[StreamPart]

// Seq is an alias so provider modules need no iter import for the signature.
type Seq[T any] = iter.Seq2[T, error]

// FinishReason unifies vendor stop reasons. Raw keeps the vendor string.
type FinishReason string

const (
	FinishStop         FinishReason = "stop"
	FinishLength       FinishReason = "length"
	FinishToolCalls    FinishReason = "tool_calls"
	FinishContentFilter FinishReason = "content_filter"
	FinishRefusal      FinishReason = "refusal"
	FinishError        FinishReason = "error"
	FinishOther        FinishReason = "other"
)

// Finish is the terminal stream part.
type Finish struct {
	Reason FinishReason
	Raw    string
	Usage  Usage
}

// Usage is token accounting for one model call.
type Usage struct {
	Input     int64
	Output    int64
	CacheRead int64
	CacheWrite int64
	Reasoning int64
}

// Model is the provider abstraction a session drives. Implementations live
// in native/provider/*.
type Model interface {
	// Name is the model identifier the driver reports on init.
	Name() string
	// Stream runs one model call and yields the response as stream parts.
	// The sequence must yield exactly one StreamFinish.
	Stream(ctx context.Context, req Request) Seq[StreamPart]
}

// Request is one model call.
type Request struct {
	// System is the system prompt.
	System string
	// Messages is the full conversation, tool results included.
	Messages []Message
	// Tools is the callable surface for this call.
	Tools []ToolSpec
	// ToolChoice steers tool use. The zero value is Auto.
	ToolChoice ToolChoice
	// MaxOutputTokens bounds one response. Zero means the provider default.
	MaxOutputTokens int
	// Effort is low | medium | high | max. The provider maps it onto its own
	// thinking budget or reasoning effort.
	Effort string
	// ProviderOptions carries provider-specific extensions. The provider
	// modules document their keys.
	ProviderOptions map[string]json.RawMessage
}

// ToolChoiceMode says how the provider may pick tools.
type ToolChoiceMode int

const (
	ToolAuto ToolChoiceMode = iota
	ToolNone
	ToolRequired
	ToolNamed
)

// ToolChoice is the tool-use steering of one call.
type ToolChoice struct {
	Mode ToolChoiceMode
	// Name names the tool when Mode is ToolNamed.
	Name string
}
