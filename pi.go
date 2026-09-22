package agentwire

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/trebi-ai/agent-wire/internal/proc"
	"github.com/trebi-ai/agent-wire/internal/wire"
)

// piDriver speaks `pi --mode rpc`.
type piDriver struct{ rt *Runtime }

func (piDriver) Name() string { return string(Pi) }

func (d piDriver) Start(ctx context.Context, req StartRequest) (Session, error) {
	return d.start(ctx, req, false)
}

func (d piDriver) Resume(ctx context.Context, req StartRequest) (Session, error) {
	return d.start(ctx, req, true)
}

func (d piDriver) start(ctx context.Context, req StartRequest, resume bool) (Session, error) {
	l, err := d.rt.prepare(ctx, &req, resume)
	if err != nil {
		return nil, err
	}
	args := append([]string{}, l.args...)
	if !hasFlag(args, "--mode") && !hasFlag(args, "--rpc") {
		args = append(args, "--mode", "rpc")
	}
	p, err := proc.Start(ctx, proc.Opts{
		Path: l.bin, Args: args, Dir: req.WorkingDir,
		Env: proc.ChildEnv(l.env), OnStderr: d.rt.stderrHook(Pi, req.OnStderr),
	})
	if err != nil {
		return nil, fmt.Errorf("pi: %w", err)
	}
	proto := newPiProtocol(req.Permissions, l.instructions)
	s, err := wire.Start(ctx, string(Pi), p, proto, d.rt.wireConfig(req.LogPath))
	if err != nil {
		return nil, fmt.Errorf("pi: %w", err)
	}
	d.rt.bindSession(s, l)
	if req.SessionID != "" {
		s.SetID(req.SessionID)
	}
	if req.OneShot && req.OneShotPrompt != "" {
		if err := s.Prompt(ctx, Prompt{Text: req.OneShotPrompt, Kind: "job"}); err != nil {
			_ = s.KillTree()
			return nil, err
		}
		_ = p.CloseStdin()
	}
	return s, nil
}

// piProtocol maps pi's RPC vocabulary. Captured against pi 0.84.1:
// agent_start, turn_start, message_start and message_end with role and content
// blocks, turn_end, agent_end. Deltas arrive as message_update where the pin
// supports them.
type piProtocol struct {
	mu sync.Mutex
	w  *wire.Writer

	policy       PermissionPolicy
	instructions string
	firstPrompt  bool

	sawEnd    bool
	sawError  string
	sawResult bool
}

func newPiProtocol(policy PermissionPolicy, instructions string) *piProtocol {
	return &piProtocol{
		policy:       policy.Normalized(),
		instructions: instructions,
		firstPrompt:  strings.TrimSpace(instructions) != "",
	}
}

func (p *piProtocol) SetWriter(w *wire.Writer) {
	p.mu.Lock()
	p.w = w
	p.mu.Unlock()
}

func (p *piProtocol) Handshake(context.Context, *wire.Writer) ([]Event, error) { return nil, nil }

// EncodePrompt sends the prompt command. The instructions ride along on the
// first turn when the binary has no flag for them.
func (p *piProtocol) EncodePrompt(pr Prompt) ([]byte, error) {
	if len(pr.Attachments) > 0 {
		return nil, fmt.Errorf("%w: pi does not accept prompt attachments", ErrUnsupported)
	}
	p.mu.Lock()
	first := p.firstPrompt
	p.firstPrompt = false
	p.sawEnd = false
	p.sawResult = false
	p.mu.Unlock()
	return wire.JSONLine(map[string]any{
		"type": "prompt", "message": withInstructions(p.instructions, pr.Text, first),
	})
}

func (p *piProtocol) EncodeInterrupt() ([]byte, error) {
	return wire.JSONLine(map[string]any{"type": "abort"})
}

func (p *piProtocol) EncodeDecision(id string, d Decision) ([]byte, error) {
	return wire.JSONLine(map[string]any{
		"type": "permission_response", "id": id, "approved": d.Allow, "message": d.Message,
	})
}

func (p *piProtocol) Parse(line []byte) []Event {
	var m map[string]any
	if json.Unmarshal(line, &m) != nil {
		return nil
	}
	switch str(m["type"]) {
	case "ready", "session_start", "session_started":
		return []Event{{Type: EventInit, SessionID: str(m["session_id"])}}
	case "agent_start", "turn_start":
		return []Event{{Type: EventStatus, Status: StatusRunning}}
	case "message_start", "message_end":
		return p.parseMessage(str(m["type"]), m)
	case "turn_end":
		p.noteError(m)
		return nil
	case "message_update":
		return p.parseUpdate(m)
	case "message":
		if text := str(m["text"]); text != "" {
			return []Event{{Type: EventAssistant, Text: text}}
		}
	case "tool_execution_start", "tool_execution_end":
		name := str(m["tool"])
		kind := classifyToolName(name)
		input := ""
		if raw, ok := m["input"]; ok {
			if b, err := json.Marshal(raw); err == nil {
				input = string(b)
			}
		}
		status := "started"
		if str(m["type"]) == "tool_execution_end" {
			status = "completed"
		}
		return []Event{{Type: EventTool, Tool: &ToolEvent{
			ID: str(m["id"]), Name: name, Kind: kind, Input: input,
			Output: str(m["output"]), Paths: toolPaths(kind, input), Status: status,
		}}}
	case "permission_request", "permission":
		return p.parsePermission(m)
	case "agent_end":
		return p.parseAgentEnd()
	case "error":
		msg := firstNonEmpty(str(m["error"]), str(m["message"]))
		if msg == "" {
			msg = fmt.Sprint(m["error"])
		}
		p.mu.Lock()
		p.sawError = msg
		p.mu.Unlock()
		c := ClassifyFor(Pi, msg, 0)
		return []Event{{Type: EventError, Error: msg, Code: c.Code, EndReason: string(c.Class)}}
	}
	return nil
}

// parseUpdate maps a streaming message update.
func (p *piProtocol) parseUpdate(m map[string]any) []Event {
	ev, _ := m["assistantMessageEvent"].(map[string]any)
	if ev == nil {
		ev, _ = m["event"].(map[string]any)
	}
	if ev == nil {
		return nil
	}
	switch str(ev["type"]) {
	case "text_delta", "text":
		text := firstNonEmpty(str(ev["delta"]), str(ev["text"]))
		if text != "" {
			return []Event{{Type: EventAssistant, Text: text, Delta: str(ev["type"]) == "text_delta"}}
		}
	case "tool_use", "tool_call":
		name := str(ev["name"])
		return []Event{{Type: EventTool, Tool: &ToolEvent{
			ID: str(ev["id"]), Name: name, Kind: classifyToolName(name), Status: "started",
		}}}
	case "tool_result":
		name := str(ev["name"])
		return []Event{{Type: EventTool, Tool: &ToolEvent{
			ID: str(ev["id"]), Name: name, Kind: classifyToolName(name), Status: "completed",
		}}}
	}
	return nil
}

// parsePermission maps a permission request, unless the policy answers it.
func (p *piProtocol) parsePermission(m map[string]any) []Event {
	id := str(m["id"])
	tool := str(m["tool"])
	kind := classifyToolName(tool)
	if d, ok := p.policy.Answer(kind, tool); ok {
		if frame, err := p.EncodeDecision(id, d); err == nil {
			p.mu.Lock()
			w := p.w
			p.mu.Unlock()
			if w != nil {
				_ = w.Write(frame)
			}
		}
		return nil
	}
	return []Event{{Type: EventPermission, Permission: &Permission{
		ID: id, Tool: tool, Kind: kind,
		Question: firstNonEmpty(str(m["message"]), str(m["question"]), "pi requests permission"),
	}}}
}

// parseAgentEnd emits the terminal turn event.
func (p *piProtocol) parseAgentEnd() []Event {
	p.mu.Lock()
	p.sawEnd = true
	p.sawResult = true
	lastErr := p.sawError
	p.mu.Unlock()
	if lastErr != "" {
		c := ClassifyFor(Pi, lastErr, 0)
		return []Event{{Type: EventResult, Result: &Result{
			Subtype: "error_during_execution", IsError: true, Text: lastErr,
			EndReason: string(c.Class), Code: c.Code,
		}}}
	}
	return []Event{{Type: EventResult, Result: &Result{Subtype: "success"}}}
}

// parseMessage maps pi's message_start and message_end frames: assistant text
// into assistant events, toolCall blocks into tool events, and the final usage
// on message_end into a usage event (message_start usage is all zeros).
func (p *piProtocol) parseMessage(kind string, m map[string]any) []Event {
	msg, _ := m["message"].(map[string]any)
	if msg == nil {
		return nil
	}
	p.noteError(msg)
	if role := str(msg["role"]); role == "user" {
		return nil
	}
	var out []Event
	for _, block := range contentBlocks(msg["content"]) {
		switch str(block["type"]) {
		case "text":
			if text := str(block["text"]); text != "" {
				out = append(out, Event{Type: EventAssistant, Text: text})
			}
		case "toolCall":
			name := str(block["name"])
			input := ""
			if raw, ok := block["arguments"]; ok {
				if b, err := json.Marshal(raw); err == nil {
					input = string(b)
				}
			}
			kind := classifyToolName(name)
			out = append(out, Event{Type: EventTool, Tool: &ToolEvent{
				ID: str(block["id"]), Name: name, Kind: kind, Input: input,
				Paths: toolPaths(kind, input), Status: "started",
			}})
		case "toolResult":
			name := str(block["name"])
			out = append(out, Event{Type: EventTool, Tool: &ToolEvent{
				ID: str(block["id"]), Name: name, Kind: classifyToolName(name),
				Output: flattenContent(block["content"]), Status: "completed",
			}})
		}
	}
	if kind == "message_end" {
		if u := piUsage(msg["usage"]); u != nil {
			out = append(out, Event{Type: EventUsage, Usage: u})
		}
	}
	return out
}

// noteError records a message-level provider failure so agent_end can report a
// failed result instead of a silent success.
func (p *piProtocol) noteError(m map[string]any) {
	key := m
	if msg, _ := m["message"].(map[string]any); msg != nil {
		key = msg
	}
	if text := str(key["errorMessage"]); text != "" {
		p.mu.Lock()
		p.sawError = text
		p.mu.Unlock()
	}
	if stop := str(key["stopReason"]); stop == "error" {
		p.mu.Lock()
		if p.sawError == "" {
			p.sawError = "pi turn ended with stopReason error"
		}
		p.mu.Unlock()
	}
}

func piUsage(v any) *Usage {
	m, _ := v.(map[string]any)
	if m == nil {
		return nil
	}
	u := &Usage{
		Input:         int64(num(m["input"])),
		Output:        int64(num(m["output"])),
		CacheRead:     int64(num(m["cacheRead"])),
		CacheCreation: int64(num(m["cacheWrite"])),
	}
	if u.Input == 0 && u.Output == 0 && u.CacheRead == 0 && u.CacheCreation == 0 {
		return nil
	}
	if cost, _ := m["cost"].(map[string]any); cost != nil {
		u.CostUSD = num(cost["total"])
	}
	return u
}

// Exit classifies a process exit that never reached agent_end.
func (p *piProtocol) Exit(code int, err error, stderr string) []Event {
	p.mu.Lock()
	saw := p.sawEnd
	sawErr := p.sawError
	p.mu.Unlock()
	if saw {
		return nil
	}
	text := strings.TrimSpace(stderr)
	if text == "" {
		text = sawErr
	}
	if text == "" && err != nil {
		text = err.Error()
	}
	if text == "" {
		text = fmt.Sprintf("pi exited with code %d before agent_end", code)
	}
	c := ClassifyFor(Pi, text, 0)
	return []Event{{Type: EventError, Error: text, Code: c.Code, EndReason: string(c.Class)}}
}

// ensure the pi protocol satisfies the adapter interface.
var (
	_ wire.Adapter          = (*piProtocol)(nil)
	_ wire.InterruptEncoder = (*piProtocol)(nil)
)
