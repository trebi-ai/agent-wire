package agentwire

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/trebi-ai/agent-wire/internal/proc"
	"github.com/trebi-ai/agent-wire/internal/wire"
)

// claudeMinVersion is the documented CLI floor.
const claudeMinVersion = "2.0.0"

// claudeDriver speaks Claude Code's stream-json wire over a child process.
type claudeDriver struct{ rt *Runtime }

func (claudeDriver) Name() string { return string(Claude) }

func (d claudeDriver) Start(ctx context.Context, req StartRequest) (Session, error) {
	return d.start(ctx, req, false)
}

func (d claudeDriver) Resume(ctx context.Context, req StartRequest) (Session, error) {
	return d.start(ctx, req, true)
}

func (d claudeDriver) start(ctx context.Context, req StartRequest, resume bool) (Session, error) {
	l, err := d.rt.prepare(ctx, &req, resume)
	if err != nil {
		return nil, err
	}
	if err := d.rt.checkClaudeVersion(ctx, l.bin); err != nil {
		return nil, err
	}
	// The protocol half: stream-json in and out, with partial messages so text
	// arrives as deltas.
	args := []string{"--output-format", "stream-json", "--verbose", "--include-partial-messages"}
	args = append(args, "--input-format", "stream-json")
	if req.OneShot {
		// Narrow one-shot path: -p plus stream-json input. Stdin stays open
		// until the result so permission control frames are still answered;
		// the caller closes it on terminate.
		args = append(args, "--print")
	}
	args = append(args, l.args...)
	// The host permission callback needs the stdio prompt tool. Without it the
	// CLI denies prompts itself and never sends can_use_tool.
	if !hasFlag(args, "--permission-prompt-tool") {
		args = append(args, "--permission-prompt-tool", "stdio")
	}
	p, err := proc.Start(ctx, proc.Opts{
		Path: l.bin, Args: args, Dir: req.WorkingDir,
		Env: proc.ChildEnv(l.env), OnStderr: d.rt.stderrHook(Claude, req.OnStderr),
	})
	if err != nil {
		return nil, fmt.Errorf("claude: %w", err)
	}
	proto := newClaudeProtocol(d.rt, req.Permissions)
	s, err := wire.Start(ctx, string(Claude), p, proto, d.rt.wireConfig(req.LogPath))
	if err != nil {
		return nil, fmt.Errorf("claude: %w", err)
	}
	d.rt.bindSession(s, l)
	if req.SessionID != "" {
		s.SetID(req.SessionID)
	}
	if req.OneShot && req.OneShotPrompt != "" {
		if err := s.Prompt(ctx, Prompt{Text: req.OneShotPrompt, Kind: "job"}); err != nil {
			_ = s.KillTree()
			return nil, fmt.Errorf("claude: write prompt: %w", err)
		}
	}
	return s, nil
}

// checkClaudeVersion fails closed below the CLI floor: an old binary may not
// speak stream-json or the control channel.
func (rt *Runtime) checkClaudeVersion(ctx context.Context, bin string) error {
	ver := rt.Version(ctx, bin)
	if strings.TrimSpace(ver) == "" {
		return fmt.Errorf("claude: cannot determine --version for %s; pin a CLI >= %s or use another runtime", bin, claudeMinVersion)
	}
	if !semverAtLeast(ver, claudeMinVersion) {
		return fmt.Errorf("claude %s is below the supported floor %s: update the binary pin", firstToken(ver), claudeMinVersion)
	}
	return nil
}

// hasFlag reports an exact flag token or its equals form.
func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag || strings.HasPrefix(a, flag+"=") {
			return true
		}
	}
	return false
}

// claudeProtocol ports the SDK's stream-json message and control shapes.
type claudeProtocol struct {
	rt     *Runtime
	policy PermissionPolicy

	mu      sync.Mutex
	w       *wire.Writer
	pending map[string]json.RawMessage
	// tools maps a tool_use id to its name: tool_result frames carry only the
	// id, so the completion event would otherwise lose the tool name.
	tools     map[string]string
	sawResult bool
}

func newClaudeProtocol(rt *Runtime, policy PermissionPolicy) *claudeProtocol {
	return &claudeProtocol{
		rt:      rt,
		policy:  policy.Normalized(),
		pending: map[string]json.RawMessage{},
		tools:   map[string]string{},
	}
}

func (p *claudeProtocol) SetWriter(w *wire.Writer) {
	p.mu.Lock()
	p.w = w
	p.mu.Unlock()
}

func (p *claudeProtocol) writer() *wire.Writer {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.w
}

// Handshake sends the control initialize request without waiting for the
// reply: the first prompt goes out immediately.
func (p *claudeProtocol) Handshake(_ context.Context, w *wire.Writer) ([]Event, error) {
	frame, err := wire.JSONLine(map[string]any{
		"type":       "control_request",
		"request_id": p.rt.requestID(),
		"request":    map[string]any{"subtype": "initialize", "hooks": nil},
	})
	if err != nil {
		return nil, err
	}
	return nil, w.Write(frame)
}

// EncodePrompt ports the SDK user-message shape. Attachments become content
// blocks, images first.
func (p *claudeProtocol) EncodePrompt(pr Prompt) ([]byte, error) {
	content, err := claudeContent(pr)
	if err != nil {
		return nil, err
	}
	return wire.JSONLine(map[string]any{
		"type":               "user",
		"session_id":         "",
		"message":            map[string]any{"role": "user", "content": content},
		"parent_tool_use_id": nil,
	})
}

// claudeContent renders the prompt body: a plain string when there is nothing
// but text, a content-block list otherwise.
func claudeContent(pr Prompt) (any, error) {
	if len(pr.Attachments) == 0 {
		return pr.Text, nil
	}
	blocks := make([]map[string]any, 0, len(pr.Attachments)+1)
	for _, a := range sortedAttachments(pr.Attachments) {
		data, media, err := loadAttachment(a)
		if err != nil {
			return nil, err
		}
		if imageAttachment(media) {
			blocks = append(blocks, map[string]any{
				"type": "image",
				"source": map[string]any{
					"type": "base64", "media_type": media, "data": encodeBase64(data),
				},
			})
			continue
		}
		blocks = append(blocks, map[string]any{"type": "text", "text": string(data)})
	}
	blocks = append(blocks, map[string]any{"type": "text", "text": pr.Text})
	return blocks, nil
}

func (p *claudeProtocol) EncodeInterrupt() ([]byte, error) {
	return wire.JSONLine(map[string]any{
		"type":       "control_request",
		"request_id": p.rt.requestID(),
		"request":    map[string]any{"subtype": "interrupt"},
	})
}

// EncodeDecision answers a can_use_tool control request.
func (p *claudeProtocol) EncodeDecision(id string, d Decision) ([]byte, error) {
	p.mu.Lock()
	orig := p.pending[id]
	delete(p.pending, id)
	p.mu.Unlock()

	response := map[string]any{}
	if d.Allow {
		response["behavior"] = "allow"
		switch {
		case len(d.UpdatedInput) > 0:
			response["updatedInput"] = json.RawMessage(d.UpdatedInput)
		case len(orig) > 0:
			response["updatedInput"] = json.RawMessage(orig)
		default:
			response["updatedInput"] = map[string]any{}
		}
	} else {
		response["behavior"] = "deny"
		if d.Message != "" {
			response["message"] = d.Message
		}
	}
	return wire.JSONLine(map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": id,
			"response":   response,
		},
	})
}

// Parse maps one stdout frame to normalized events.
func (p *claudeProtocol) Parse(line []byte) []Event {
	trimmed := strings.TrimSpace(string(line))
	if trimmed == "" {
		return nil
	}
	if !strings.HasPrefix(trimmed, "{") {
		// The CLI writes non-JSON diagnostics to stdout; skip them without
		// failing the wire, but keep the line visible as a status.
		return []Event{{Type: EventStatus, Text: trimmed, Status: StatusRunning}}
	}
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		return []Event{{Type: EventError, Error: "claude: malformed JSON frame: " + err.Error(), Code: "malformed_frame", EndReason: string(FailProtocol)}}
	}
	switch str(m["type"]) {
	case "system":
		return p.parseSystem(m)
	case "assistant":
		return p.parseAssistant(m)
	case "user":
		return p.parseUser(m)
	case "stream_event":
		return p.parseStreamEvent(m)
	case "result":
		return p.parseResult(m)
	case "control_request":
		return p.parseControlRequest(m)
	case "rate_limit_event":
		return p.parseRateLimit(m)
	}
	return nil
}

func (p *claudeProtocol) parseSystem(m map[string]any) []Event {
	switch str(m["subtype"]) {
	case "init":
		ev := Event{Type: EventInit, SessionID: str(m["session_id"])}
		if model := str(m["model"]); model != "" {
			ev.Text = model
		}
		return []Event{ev}
	case "session_state_changed":
		switch str(m["state"]) {
		case "idle":
			return []Event{{Type: EventStatus, Status: StatusIdle}}
		case "running":
			return []Event{{Type: EventStatus, Status: StatusRunning}}
		}
	case "status":
		// For example compacting; the turn is still active.
		return []Event{{Type: EventStatus, Status: StatusRunning, Text: str(m["status"])}}
	case "compact_boundary":
		return []Event{{Type: EventStatus, Status: StatusRunning, Text: "compacting"}}
	}
	return nil
}

func (p *claudeProtocol) parseAssistant(m map[string]any) []Event {
	msg, _ := m["message"].(map[string]any)
	if msg == nil {
		return nil
	}
	var out []Event
	for _, block := range contentBlocks(msg["content"]) {
		switch str(block["type"]) {
		case "text":
			if text := str(block["text"]); text != "" {
				out = append(out, Event{Type: EventAssistant, Text: text})
			}
		case "tool_use":
			out = append(out, p.toolStarted(block)...)
		}
	}
	if u := messageUsage(msg["usage"]); u != nil {
		out = append(out, Event{Type: EventUsage, Usage: u})
	}
	return out
}

// toolStarted records the tool name and emits the start event.
func (p *claudeProtocol) toolStarted(block map[string]any) []Event {
	input := "{}"
	if raw, ok := block["input"]; ok {
		if b, err := json.Marshal(raw); err == nil {
			input = string(b)
		}
	}
	id, name := str(block["id"]), str(block["name"])
	kind := classifyToolName(name)
	if id != "" && name != "" {
		p.mu.Lock()
		p.tools[id] = name
		p.mu.Unlock()
	}
	return []Event{{Type: EventTool, Tool: &ToolEvent{
		ID: id, Name: name, Kind: kind, Input: input,
		Paths: toolPaths(kind, input), Status: "started",
	}}}
}

func (p *claudeProtocol) parseUser(m map[string]any) []Event {
	msg, _ := m["message"].(map[string]any)
	if msg == nil {
		return nil
	}
	var out []Event
	for _, block := range contentBlocks(msg["content"]) {
		switch str(block["type"]) {
		case "tool_result":
			status := "completed"
			if isErr, _ := block["is_error"].(bool); isErr {
				status = "failed"
			}
			id := str(block["tool_use_id"])
			p.mu.Lock()
			name := p.tools[id]
			delete(p.tools, id)
			p.mu.Unlock()
			out = append(out, Event{Type: EventTool, Tool: &ToolEvent{
				ID: id, Name: name, Kind: classifyToolName(name),
				Output: flattenContent(block["content"]), Status: status,
			}})
		case "text":
			if text := str(block["text"]); text != "" {
				out = append(out, Event{Type: EventUser, Text: text})
			}
		}
	}
	if len(out) == 0 {
		if text, ok := msg["content"].(string); ok && text != "" {
			out = append(out, Event{Type: EventUser, Text: text})
		}
	}
	return out
}

func (p *claudeProtocol) parseStreamEvent(m map[string]any) []Event {
	ev, _ := m["event"].(map[string]any)
	if ev == nil {
		return nil
	}
	switch str(ev["type"]) {
	case "content_block_delta":
		delta, _ := ev["delta"].(map[string]any)
		if delta == nil {
			return nil
		}
		if str(delta["type"]) == "text_delta" {
			if text := str(delta["text"]); text != "" {
				return []Event{{Type: EventAssistant, Text: text, Delta: true}}
			}
		}
	case "content_block_start":
		block, _ := ev["content_block"].(map[string]any)
		if block != nil && str(block["type"]) == "tool_use" {
			return p.toolStarted(block)
		}
	case "message_start":
		msg, _ := ev["message"].(map[string]any)
		if msg != nil {
			if u := messageUsage(msg["usage"]); u != nil {
				return []Event{{Type: EventUsage, Usage: u}}
			}
		}
	case "message_delta":
		if u := messageUsage(ev["usage"]); u != nil {
			return []Event{{Type: EventUsage, Usage: u}}
		}
	}
	return nil
}

func (p *claudeProtocol) parseResult(m map[string]any) []Event {
	p.mu.Lock()
	p.sawResult = true
	p.mu.Unlock()
	// Every tool still open when the turn ends is over; the vendor stream
	// lost its completion (plan 2026-09-26 B). Close them as cancelled.
	out := p.cancelOpenTools()
	subtype := str(m["subtype"])
	isErr, _ := m["is_error"].(bool)
	res := &Result{
		Subtype:   subtype,
		IsError:   isErr,
		SessionID: str(m["session_id"]),
	}
	res.Text = resultFrameText(m)
	if u := messageUsage(m["usage"]); u != nil {
		res.Usage = *u
	}
	if cost, ok := m["total_cost_usd"].(float64); ok {
		res.Usage.CostUSD = cost
	}
	if isErr {
		c := ClassifyResultFor(Claude, res)
		res.EndReason = string(c.Class)
		res.Code = c.Code
	}
	return append(out, Event{Type: EventResult, Result: res, SessionID: res.SessionID})
}

// cancelOpenTools emits one cancelled tool event per open tool_use id and
// clears the table (plan 2026-09-26 B).
func (p *claudeProtocol) cancelOpenTools() []Event {
	p.mu.Lock()
	ids := make([]string, 0, len(p.tools))
	for id := range p.tools {
		ids = append(ids, id)
	}
	clear(p.tools)
	p.mu.Unlock()
	if len(ids) == 0 {
		return nil
	}
	sort.Strings(ids)
	out := make([]Event, 0, len(ids))
	for _, id := range ids {
		out = append(out, Event{Type: EventTool, Tool: &ToolEvent{ID: id, Status: "cancelled"}})
	}
	return out
}

// resultFrameText ports the SDK's error result text picker: errors[], then
// result, then subtype, then api_error_status.
func resultFrameText(m map[string]any) string {
	var errs []string
	switch v := m["errors"].(type) {
	case []any:
		for _, e := range v {
			if s, ok := e.(string); ok && s != "" {
				errs = append(errs, s)
			}
		}
	case string:
		if v != "" {
			errs = append(errs, v)
		}
	}
	status := 0
	if f, ok := m["api_error_status"].(float64); ok {
		status = int(f)
	}
	return firstNonEmpty(
		strings.Join(errs, "; "),
		strings.TrimSpace(str(m["result"])),
		subtypeText(str(m["subtype"])),
		statusText(status),
		"unknown error",
	)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func subtypeText(subtype string) string {
	if subtype == "" || subtype == "success" {
		return ""
	}
	return subtype
}

func statusText(status int) string {
	if status <= 0 {
		return ""
	}
	return fmt.Sprintf("API error (HTTP %d)", status)
}

func (p *claudeProtocol) parseRateLimit(m map[string]any) []Event {
	status := str(m["status"])
	if status == "rejected" || status == "blocked" {
		text := str(m["message"])
		if text == "" {
			text = "claude rate limit rejected"
		}
		c := ClassifyFor(Claude, text, 0)
		return []Event{{Type: EventError, Error: text, Code: c.Code, EndReason: string(c.Class)}}
	}
	return []Event{{Type: EventStatus, Status: StatusRunning, Text: "rate_limit:" + status}}
}

// parseControlRequest maps a permission request, or answers an unimplemented
// one so the CLI never waits on it.
func (p *claudeProtocol) parseControlRequest(m map[string]any) []Event {
	request, _ := m["request"].(map[string]any)
	if request == nil {
		return nil
	}
	id := str(m["request_id"])
	if str(request["subtype"]) != "can_use_tool" {
		frame, _ := wire.JSONLine(map[string]any{
			"type": "control_response",
			"response": map[string]any{
				"subtype":    "error",
				"request_id": id,
				"error":      "agentwire: unsupported control request subtype " + str(request["subtype"]),
			},
		})
		if w := p.writer(); w != nil {
			_ = w.Write(frame)
		}
		return nil
	}
	input := "{}"
	var raw json.RawMessage
	if v, ok := request["input"]; ok {
		if b, err := json.Marshal(v); err == nil {
			input = string(b)
			raw = json.RawMessage(b)
		}
	}
	tool := str(request["tool_name"])
	kind := classifyPermissionKind(tool, input)
	if d, ok := p.policy.Answer(kind, tool); ok {
		// The policy answers without the consumer; record the input so the
		// allow response carries it back unchanged. No tool event: the
		// tool_use block already announced the start, and the tool_result
		// carries the same tool_use id (plan 2026-09-26 B). An event keyed
		// by the control request_id would never complete.
		p.mu.Lock()
		p.pending[id] = raw
		p.mu.Unlock()
		frame, err := p.EncodeDecision(id, d)
		if err == nil {
			if w := p.writer(); w != nil {
				_ = w.Write(frame)
			}
		}
		return nil
	}
	question := firstNonEmpty(str(request["title"]), str(request["description"]), "Allow "+tool+"?")
	if len(raw) > 0 {
		p.mu.Lock()
		p.pending[id] = raw
		p.mu.Unlock()
	}
	return []Event{{Type: EventPermission, Permission: &Permission{
		ID: id, Tool: tool, Kind: kind, Question: question, Input: input,
		ToolUseID: str(request["tool_use_id"]),
	}}}
}

// Exit classifies a process exit that never produced a result frame.
func (p *claudeProtocol) Exit(code int, err error, stderr string) []Event {
	// Tools left open die with the process (plan 2026-09-26 B).
	out := p.cancelOpenTools()
	p.mu.Lock()
	saw := p.sawResult
	p.mu.Unlock()
	if saw {
		return out
	}
	text := strings.TrimSpace(stderr)
	if text == "" {
		if err != nil {
			text = err.Error()
		} else {
			text = fmt.Sprintf("claude exited with code %d before a result", code)
		}
	}
	c := ClassifyFor(Claude, text, 0)
	return append(out, Event{Type: EventError, Error: text, Code: c.Code, EndReason: string(c.Class)})
}

func contentBlocks(v any) []map[string]any {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		if b, ok := item.(map[string]any); ok {
			out = append(out, b)
		}
	}
	return out
}

func messageUsage(v any) *Usage {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	u := &Usage{
		Input:         int64(num(m["input_tokens"])),
		Output:        int64(num(m["output_tokens"])),
		CacheRead:     int64(num(m["cache_read_input_tokens"])),
		CacheCreation: int64(num(m["cache_creation_input_tokens"])),
	}
	if u.Input == 0 && u.Output == 0 && u.CacheRead == 0 && u.CacheCreation == 0 {
		return nil
	}
	u.Model = str(m["model"])
	return u
}

func num(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case int:
		return float64(t)
	case json.Number:
		f, _ := t.Float64()
		return f
	}
	return 0
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// flattenContent renders a tool_result content value (a string or a block
// list).
func flattenContent(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var parts []string
		for _, item := range t {
			if b, ok := item.(map[string]any); ok {
				if s := str(b["text"]); s != "" {
					parts = append(parts, s)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}
