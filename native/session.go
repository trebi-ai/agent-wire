package native

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/trebi-ai/agent-wire"
)

// DefaultMaxTurns bounds one prompt when Config.MaxTurns is unset.
const DefaultMaxTurns = 20

// session is one live native conversation. It implements agentwire.Session:
// Prompt blocks until the turn ends, and Events carries the wire events.
type session struct {
	drv       *Driver
	model     Model
	sessionID string
	opts      sessionOpts
	tools     map[string]nativeTool
	skills    SkillCatalog
	store     Store

	events chan agentwire.Event
	done   chan struct{}
	closed sync.Once

	mu       sync.Mutex
	msgs     []Message
	usage    Usage
	turnNo   int
	turnStop context.CancelFunc
	err      error

	approvals *sessionApprovals
}

// sessionOpts is the per-session configuration the driver assembles.
type sessionOpts struct {
	system          string
	maxTurns        int
	maxOutputTokens int
	contextTokens   int
	approver        Approver
	beforeCall      func(context.Context) error
	afterCall       func(context.Context, Usage)
}

// nativeTool pairs a tool with its spec.
type nativeTool struct {
	tool Tool
	spec ToolSpec
}

// newSession assembles one native session.
func newSession(drv *Driver, model Model, sessionID string, opts sessionOpts, tools []nativeTool, skills SkillCatalog) *session {
	var store Store
	if drv != nil {
		store = drv.cfg.Store
	}
	s := &session{
		drv:       drv,
		model:     model,
		sessionID: sessionID,
		opts:      opts,
		tools:     map[string]nativeTool{},
		skills:    skills,
		store:     store,
		events:    make(chan agentwire.Event, 256),
		done:      make(chan struct{}),
		approvals: newSessionApprovals(),
	}
	for _, t := range tools {
		s.tools[t.spec.Name] = t
	}
	if s.opts.maxTurns <= 0 {
		s.opts.maxTurns = DefaultMaxTurns
	}
	if s.opts.approver == nil {
		s.opts.approver = PolicyApprover{Policy: agentwire.PermissionPolicy{Mode: agentwire.PermissionAsk}}
	}
	return s
}

// initEvent is the init event of a native session.
func (s *session) initEvent() agentwire.Event {
	return agentwire.Event{Type: agentwire.EventInit, SessionID: s.ID(),
		Text: s.model.Name(), Turn: s.currentTurn(), At: time.Now().UTC()}
}

// Prompt runs one turn. It blocks until the turn ends; events arrive on
// Events while it runs.
func (s *session) Prompt(ctx context.Context, p agentwire.Prompt) error {
	s.mu.Lock()
	if s.turnStop != nil {
		s.mu.Unlock()
		return fmt.Errorf("native session is still running the previous turn")
	}
	s.turnNo++
	turn := s.turnNo
	s.msgs = append(s.msgs, Message{Role: RoleUser, Parts: []Part{{Text: &TextPart{Text: p.Text}}}})
	s.appendLocked(turn, Message{Role: RoleUser, Parts: []Part{{Text: &TextPart{Text: p.Text}}}})
	turnCtx, stop := context.WithCancel(ctx)
	s.turnStop = stop
	s.mu.Unlock()

	s.emit(agentwire.Event{Type: agentwire.EventStatus, Status: agentwire.StatusRunning, Turn: turn, At: time.Now().UTC()})

	endReason, err := s.runTurn(turnCtx, turn)
	stop()

	now := time.Now().UTC()
	switch {
	case errors.Is(err, context.Canceled):
		s.emit(agentwire.Event{Type: agentwire.EventResult,
			Result: &agentwire.Result{Subtype: "interrupted", IsError: true, EndReason: "cancelled"}, Turn: turn, At: now})
	case err != nil:
		s.setErr(err)
		s.emit(agentwire.Event{Type: agentwire.EventError, Error: err.Error(), Code: "native_turn_failed", Turn: turn, At: now})
		s.emit(agentwire.Event{Type: agentwire.EventResult,
			Result: &agentwire.Result{Subtype: "error", IsError: true, EndReason: "error"}, Turn: turn, At: now})
	default:
		s.emit(agentwire.Event{Type: agentwire.EventResult,
			Result: &agentwire.Result{Subtype: "success", EndReason: endReason}, Turn: turn, At: now})
	}
	s.emit(agentwire.Event{Type: agentwire.EventStatus, Status: agentwire.StatusIdle, Turn: turn, At: now})

	s.mu.Lock()
	s.turnStop = nil
	s.mu.Unlock()
	return nil
}

// runTurn is the tool loop of one prompt. It returns the end reason of the
// turn, or an error.
func (s *session) runTurn(ctx context.Context, turn int) (string, error) {
	for i := 0; i < s.opts.maxTurns; i++ {
		if err := ctx.Err(); err != nil {
			return "cancelled", err
		}
		if s.opts.beforeCall != nil {
			if err := s.opts.beforeCall(ctx); err != nil {
				return "", err
			}
		}
		if err := s.maybeCompact(ctx); err != nil {
			return "", err
		}

		resp, usage, calls, err := s.streamCall(ctx, turn)
		if err != nil {
			return "", err
		}
		s.emitUsage(usage, turn)
		if s.opts.afterCall != nil {
			s.opts.afterCall(ctx, usage)
		}

		assistantMsg := assistantMessage(resp, calls)
		s.mu.Lock()
		s.msgs = append(s.msgs, assistantMsg)
		s.appendLocked(turn, assistantMsg)
		s.mu.Unlock()

		if len(calls) == 0 {
			return "end_turn", nil
		}
		for _, call := range calls {
			if err := ctx.Err(); err != nil {
				return "cancelled", err
			}
			s.runTool(ctx, turn, call)
		}
	}
	return "max_turn_requests", nil
}

// streamCall runs one model call: it folds the stream into the final text,
// the reasoning text, and the complete tool calls.
func (s *session) streamCall(ctx context.Context, turn int) (string, Usage, []ToolCall, error) {
	var text strings.Builder
	var thought strings.Builder
	var calls []ToolCall
	var usage Usage

	req := Request{
		System:          s.opts.system,
		Messages:        s.snapshot(),
		Tools:           s.specs(),
		MaxOutputTokens: s.opts.maxOutputTokens,
	}
	var finish *Finish
	for part, err := range s.model.Stream(ctx, req) {
		if err != nil {
			return "", usage, nil, err
		}
		switch part.Kind {
		case StreamTextDelta:
			text.WriteString(part.Delta)
			s.emit(agentwire.Event{Type: agentwire.EventAssistant, Text: part.Delta, Delta: true, Turn: turn, At: time.Now().UTC()})
		case StreamReasoningDelta:
			thought.WriteString(part.Delta)
			s.emit(agentwire.Event{Type: agentwire.EventThought, Text: part.Delta, Delta: true, Turn: turn, At: time.Now().UTC()})
		case StreamToolInputStart, StreamToolInputDelta:
			// The complete call arrives on StreamToolCallDone.
		case StreamToolCallDone:
			if part.Call != nil {
				calls = append(calls, *part.Call)
			}
		case StreamFinish:
			finish = part.Finish
			if part.Finish != nil {
				usage = part.Finish.Usage
			}
		}
	}
	if finish == nil {
		return "", usage, nil, fmt.Errorf("native: model stream ended without a finish part")
	}
	if err := finishReasonError(finish.Reason); err != nil {
		return "", usage, nil, err
	}
	final := text.String()
	if final != "" {
		s.emit(agentwire.Event{Type: agentwire.EventAssistant, Text: final, Turn: turn, At: time.Now().UTC()})
	}
	return final, usage, calls, nil
}

// assistantMessage builds the persisted assistant entry.
func assistantMessage(text string, calls []ToolCall) Message {
	m := Message{Role: RoleAssistant}
	if text != "" {
		m.Parts = append(m.Parts, Part{Text: &TextPart{Text: text}})
	}
	for i := range calls {
		m.Parts = append(m.Parts, Part{ToolCall: &ToolCall{ID: calls[i].ID, Name: calls[i].Name, Input: calls[i].Input}})
	}
	return m
}

// runTool gates one call behind the approver, executes it, and records both
// the wire events and the tool message.
func (s *session) runTool(ctx context.Context, turn int, call ToolCall) {
	nt, known := s.tools[call.Name]
	spec := ToolSpec{Name: call.Name}
	if known {
		spec = nt.spec
	} else {
		spec.InputSchema = json.RawMessage(`{}`)
		spec.Annotations.Kind = agentwire.ToolMCP
	}

	s.emit(agentwire.Event{Type: agentwire.EventTool, Turn: turn, At: time.Now().UTC(),
		Tool: &agentwire.ToolEvent{ID: call.ID, Name: spec.Name, Kind: spec.Annotations.Kind,
			Input: truncateJSON(call.Input), Status: "started"}})

	result := s.executeTool(ctx, spec, call)

	status := "completed"
	if result.IsError {
		status = "failed"
	}
	out := flatText(Message{Parts: result.Content})
	s.emit(agentwire.Event{Type: agentwire.EventTool, Turn: turn, At: time.Now().UTC(),
		Tool: &agentwire.ToolEvent{ID: call.ID, Name: spec.Name, Kind: spec.Annotations.Kind,
			Output: out, Status: status}})

	msg := Message{Role: RoleTool, Parts: []Part{{ToolResult: &ToolResult{
		CallID: call.ID, Name: spec.Name, Content: result.Content, IsError: result.IsError,
	}}}}
	s.mu.Lock()
	s.msgs = append(s.msgs, msg)
	s.appendLocked(turn, msg)
	s.mu.Unlock()
}

// executeTool approves and runs one call.
func (s *session) executeTool(ctx context.Context, spec ToolSpec, call ToolCall) ToolResult {
	nt, known := s.tools[call.Name]
	if !known {
		return ToolResult{IsError: true, Content: []Part{{Text: &TextPart{Text: fmt.Sprintf("unknown tool %q", call.Name)}}}}
	}
	if needsApproval(spec.Annotations) {
		d, err := s.opts.approver.Approve(ctx, ApprovalRequest{CallID: call.ID, Spec: spec, Input: call.Input})
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return ToolResult{IsError: true, Content: []Part{{Text: &TextPart{Text: "cancelled"}}}}
			}
			return ToolResult{IsError: true, Content: []Part{{Text: &TextPart{Text: fmt.Sprintf("approval failed: %v", err)}}}}
		}
		if !d.Allow {
			note := d.Message
			if note != "" {
				note = "denied by the operator: " + note
			} else {
				note = "denied by the operator"
			}
			return ToolResult{IsError: true, Content: []Part{{Text: &TextPart{Text: note}}}}
		}
	}
	result, err := nt.tool.Call(ctx, call.Input)
	if err != nil {
		return ToolResult{CallID: call.ID, Name: spec.Name, IsError: true,
			Content: []Part{{Text: &TextPart{Text: fmt.Sprintf("tool infrastructure failure: %v", err)}}}}
	}
	result.CallID = call.ID
	if result.Name == "" {
		result.Name = spec.Name
	}
	return result
}

// needsApproval reports whether the default flow asks before a call: any
// mutating tool, and every MCP tool.
func needsApproval(a Annotations) bool {
	if a.Kind == agentwire.ToolMCP || a.Kind == agentwire.ToolExec {
		return true
	}
	return !a.ReadOnly
}

// maybeCompact runs the compactor when the message list has grown past 60%
// of the context budget (plan 2026-09-26 I.5).
func (s *session) maybeCompact(ctx context.Context) error {
	if s.drv == nil || s.drv.cfg.Compactor == nil || s.opts.contextTokens <= 0 {
		return nil
	}
	s.mu.Lock()
	msgs := s.msgs
	s.mu.Unlock()
	if estimateTokens(msgs) < s.opts.contextTokens*60/100 {
		return nil
	}
	folded, err := s.drv.cfg.Compactor.Compact(ctx, s.model, msgs)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.msgs = folded
	s.mu.Unlock()
	if s.store != nil {
		return s.store.Replace(ctx, s.ID(), folded)
	}
	return nil
}

// estimateTokens is the coarse length heuristic: about four bytes per token.
func estimateTokens(msgs []Message) int {
	n := 0
	for _, m := range msgs {
		for _, p := range m.Parts {
			switch {
			case p.Text != nil:
				n += len(p.Text.Text)
			case p.ToolCall != nil:
				n += len(p.ToolCall.Input)
			case p.ToolResult != nil:
				n += len(flatText(Message{Parts: p.ToolResult.Content}))
			case p.Reasoning != nil:
				n += len(p.Reasoning.Text)
			}
		}
	}
	return n / 4
}

// specs returns the model-visible tool surface.
func (s *session) specs() []ToolSpec {
	out := make([]ToolSpec, 0, len(s.tools))
	for _, t := range s.tools {
		out = append(out, t.spec)
	}
	return out
}

func (s *session) snapshot() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Message(nil), s.msgs...)
}

// appendLocked persists one message. Callers hold s.mu.
func (s *session) appendLocked(turn int, m Message) {
	if s.store == nil {
		return
	}
	_ = s.store.Append(context.Background(), s.ID(), m)
}

func (s *session) emitUsage(u Usage, turn int) {
	s.mu.Lock()
	s.usage.Input += u.Input
	s.usage.Output += u.Output
	s.usage.CacheRead += u.CacheRead
	s.usage.CacheWrite += u.CacheWrite
	s.usage.Reasoning += u.Reasoning
	s.mu.Unlock()
	s.emit(agentwire.Event{Type: agentwire.EventUsage, Turn: turn, At: time.Now().UTC(),
		Usage: &agentwire.Usage{Input: u.Input, Output: u.Output, CacheRead: u.CacheRead, CacheCreation: u.CacheWrite}})
}

func (s *session) emit(e agentwire.Event) {
	if e.SessionID == "" {
		e.SessionID = s.ID()
	}
	select {
	case s.events <- e:
	case <-s.done:
	}
}

// --- agentwire.Session ---

func (s *session) Provider() string { return string(agentwire.Native) }

func (s *session) ID() string { return s.sessionID }

func (s *session) PID() int { return 0 }

// Events is the live event stream. It closes on Close.
func (s *session) Events() <-chan agentwire.Event { return s.events }

// Interrupt stops the current turn; the loop answers with a cancelled
// result.
func (s *session) Interrupt(_ context.Context) error {
	s.mu.Lock()
	stop := s.turnStop
	s.mu.Unlock()
	if stop != nil {
		stop()
	}
	return nil
}

func (s *session) InterruptTurn(ctx context.Context) error { return s.Interrupt(ctx) }

// AnswerPermission routes a decision to the waiting approver.
func (s *session) AnswerPermission(ctx context.Context, permissionID string, d agentwire.Decision) error {
	return s.approvals.decide(permissionID, d)
}

// Done closes when the session has ended. A native session ends on Close.
func (s *session) Done() <-chan struct{} { return s.done }

func (s *session) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *session) setErr(err error) {
	s.mu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.mu.Unlock()
}

// Close ends the session and drains Events.
func (s *session) Close(context.Context) error {
	s.closed.Do(func() {
		if s.turnStop != nil {
			s.turnStop()
		}
		close(s.done)
		close(s.events)
	})
	return nil
}

// currentTurn is the store turn of the conversation.
func (s *session) currentTurn() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turnNo
}

func finishReasonError(r FinishReason) error {
	switch r {
	case FinishRefusal:
		return fmt.Errorf("model refused the request")
	case FinishContentFilter:
		return fmt.Errorf("model response was blocked by a content filter")
	}
	return nil
}

func truncateJSON(b []byte) string {
	const max = 8 << 10
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max])
}
