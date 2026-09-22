package agentwire

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/trebi-ai/agent-wire/internal/jsonrpc"
	"github.com/trebi-ai/agent-wire/internal/proc"
	"github.com/trebi-ai/agent-wire/internal/wire"
)

// Codex app-server timings.
const (
	codexHandshakeTimeout = 30 * time.Second
	// codexInterruptWait bounds the interrupt to new-turn fallback. A steer is
	// preferred; an older pin answers method-not-found and lands here.
	codexInterruptWait = 5 * time.Second
	// codexPromptAckWait bounds the turn/start ack wait: the turn is live even
	// if the pin streams notifications without an RPC response.
	codexPromptAckWait = 2 * time.Second
	// codexSteerAckWait bounds the steer ack wait before falling back.
	codexSteerAckWait = 5 * time.Second
)

// codexDriver speaks the `codex app-server` JSON-RPC protocol.
type codexDriver struct{ rt *Runtime }

func (codexDriver) Name() string { return string(Codex) }

// Start opens a new thread. A pre-assigned session id is never sent: the
// handshake adopts the thread id the server reports.
func (d codexDriver) Start(ctx context.Context, req StartRequest) (Session, error) {
	req.SessionID = ""
	return d.start(ctx, req, false)
}

// Resume reopens a known thread. The session id is the stored vendor thread id.
func (d codexDriver) Resume(ctx context.Context, req StartRequest) (Session, error) {
	return d.start(ctx, req, true)
}

func (d codexDriver) start(ctx context.Context, req StartRequest, resume bool) (Session, error) {
	l, err := d.rt.prepare(ctx, &req, resume)
	if err != nil {
		return nil, err
	}
	proto := newCodexProtocol(d.rt, req, l)
	args := append(append([]string{}, l.args...), "app-server")
	p, err := proc.Start(ctx, proc.Opts{
		Path: l.bin, Args: args, Dir: req.WorkingDir,
		Env: proc.ChildEnv(l.env), OnStderr: d.rt.stderrHook(Codex, req.OnStderr),
	})
	if err != nil {
		return nil, fmt.Errorf("codex: %w", err)
	}
	s, err := wire.Start(ctx, string(Codex), p, proto, d.rt.wireConfig(req.LogPath))
	if err != nil {
		return nil, fmt.Errorf("codex: %w", err)
	}
	d.rt.bindSession(s, l)
	s.OnClose(proto.Cleanup)
	if proto.threadID != "" {
		s.SetID(proto.threadID)
	}
	if req.OneShot && req.OneShotPrompt != "" {
		if err := s.Prompt(ctx, Prompt{Text: req.OneShotPrompt, Kind: "job"}); err != nil {
			_ = s.KillTree()
			return nil, fmt.Errorf("codex: write prompt: %w", err)
		}
		_ = p.CloseStdin()
	}
	return s, nil
}

// codexProtocol ports the app-server JSON-RPC shapes. Requests it sends get
// waiters through the shared client; approval requests from the server become
// permission events, unless the policy answers them.
type codexProtocol struct {
	rt     *Runtime
	client *jsonrpc.Client
	policy PermissionPolicy

	mu sync.Mutex
	w  *wire.Writer

	handshakeTimeout time.Duration
	model            string
	effort           string
	cwd              string
	approvalPolicy   string
	sandbox          string
	resumeID         string
	instructions     string

	// sendEffort reports that this pin accepts effort on turn/start. It is
	// cleared when the server rejects the field.
	sendEffort bool

	threadID   string
	turnID     string
	turnActive bool
	sawResult  bool
	// approvals maps a server request id to its method, so a later decision
	// knows which reply body to render.
	approvals map[string]string

	tempDir string
}

func newCodexProtocol(rt *Runtime, req StartRequest, l *launch) *codexProtocol {
	sandbox := req.RawString("sandbox")
	if sandbox == "" {
		sandbox = "workspace-write"
	}
	approval := req.RawString("approvalPolicy")
	if approval == "" {
		approval = codexApprovalPolicy(req.Permissions)
	}
	timeout := req.HandshakeTimeout
	if timeout <= 0 {
		timeout = codexHandshakeTimeout
	}
	p := &codexProtocol{
		rt:               rt,
		policy:           req.Permissions.Normalized(),
		handshakeTimeout: timeout,
		model:            req.Model,
		effort:           req.Effort,
		cwd:              req.WorkingDir,
		approvalPolicy:   approval,
		sandbox:          sandbox,
		resumeID:         req.SessionID,
		instructions:     l.instructions,
		sendEffort:       req.Effort != "",
		approvals:        map[string]string{},
	}
	p.client = jsonrpc.New(nil, timeout)
	return p
}

func (p *codexProtocol) SetWriter(w *wire.Writer) {
	p.mu.Lock()
	p.w = w
	p.mu.Unlock()
	p.client.SetWrite(w.Write)
}

// Cleanup removes the temp files this session generated.
func (p *codexProtocol) Cleanup() {
	p.mu.Lock()
	dir := p.tempDir
	p.tempDir = ""
	p.mu.Unlock()
	if dir != "" {
		_ = os.RemoveAll(dir)
	}
}

// Handshake runs initialize, initialized, then thread/start or thread/resume.
func (p *codexProtocol) Handshake(ctx context.Context, _ *wire.Writer) ([]Event, error) {
	clientName := p.rt.opts.ClientName
	if clientName == "" {
		clientName = "agentwire"
	}
	_, err := p.client.Call(ctx, "initialize", map[string]any{
		"clientInfo": map[string]any{
			"name": clientName, "title": clientName, "version": p.rt.opts.ClientVersion,
		},
	}, p.handshakeTimeout)
	if err != nil {
		return nil, err
	}
	if err := p.client.Notify("initialized", map[string]any{}); err != nil {
		return nil, err
	}
	params := map[string]any{
		"cwd": p.cwd, "approvalPolicy": p.approvalPolicy, "sandbox": p.sandbox,
	}
	if p.model != "" {
		params["model"] = p.model
	}
	if p.instructions != "" {
		params["developerInstructions"] = p.instructions
	}
	method := "thread/start"
	if p.resumeID != "" {
		method = "thread/resume"
		params["threadId"] = p.resumeID
	}
	res, err := p.client.Call(ctx, method, params, p.handshakeTimeout)
	if err != nil {
		return nil, err
	}
	var payload map[string]any
	if err := json.Unmarshal(res, &payload); err != nil {
		return nil, fmt.Errorf("codex %s: decode response: %w", method, err)
	}
	if id := codexThreadID(payload); id != "" {
		p.mu.Lock()
		p.threadID = id
		p.mu.Unlock()
		return []Event{{Type: EventInit, SessionID: id}}, nil
	}
	return nil, nil
}

// EncodePrompt is the fire-and-forget shape. The live path goes through
// PromptSession so the ack is awaited and a busy turn steers instead of
// erroring.
func (p *codexProtocol) EncodePrompt(pr Prompt) ([]byte, error) {
	p.mu.Lock()
	threadID := p.threadID
	p.mu.Unlock()
	if threadID == "" {
		return nil, errors.New("codex: thread not started")
	}
	input, err := p.inputItems(pr)
	if err != nil {
		return nil, err
	}
	return wire.JSONLine(map[string]any{
		"jsonrpc": jsonrpc.Version,
		"id":      p.client.NextID(),
		"method":  "turn/start",
		"params":  map[string]any{"threadId": threadID, "input": input},
	})
}

// PromptSession is the live prompt path: turn/steer while a turn is active,
// falling back to interrupt plus a new turn when the pin rejects steer.
func (p *codexProtocol) PromptSession(ctx context.Context, pr Prompt) error {
	p.mu.Lock()
	threadID, turnID, active := p.threadID, p.turnID, p.turnActive
	p.mu.Unlock()
	if threadID == "" {
		return errors.New("codex: thread not started")
	}
	input, err := p.inputItems(pr)
	if err != nil {
		return err
	}
	if active {
		params := map[string]any{"threadId": threadID, "input": input}
		if turnID != "" {
			params["expectedTurnId"] = turnID
		}
		if _, err := p.client.Call(ctx, "turn/steer", params, codexSteerAckWait); err == nil {
			return nil
		} else if ctx.Err() != nil {
			return err
		}
		if err := p.interruptTurn(); err != nil {
			return err
		}
		p.awaitTurnEnd(ctx, turnID)
	}
	return p.startTurn(ctx, threadID, input)
}

// startTurn sends turn/start. The turn is live even when the pin streams
// notifications without an RPC ack, so an ack timeout is not an error.
func (p *codexProtocol) startTurn(ctx context.Context, threadID string, input []map[string]any) error {
	params := map[string]any{"threadId": threadID, "input": input}
	if p.model != "" {
		params["model"] = p.model
	}
	p.mu.Lock()
	effort := p.effort
	sendEffort := p.sendEffort
	p.mu.Unlock()
	if effort != "" && sendEffort {
		params["effort"] = effort
	}
	_, err := p.client.Call(ctx, "turn/start", params, codexPromptAckWait)
	if err != nil && effort != "" && sendEffort && ctx.Err() == nil && codexRejectsField(err) {
		// This pin does not take effort on turn/start. Drop it for the rest of
		// the session; -c model_reasoning_effort still carries the value.
		p.mu.Lock()
		p.sendEffort = false
		p.mu.Unlock()
		delete(params, "effort")
		_, err = p.client.Call(ctx, "turn/start", params, codexPromptAckWait)
	}
	if err != nil && !isTimeout(err) && ctx.Err() == nil {
		return err
	}
	p.mu.Lock()
	p.turnActive = true
	p.mu.Unlock()
	return nil
}

// codexRejectsField reports a server error that names an unknown parameter.
func codexRejectsField(err error) bool {
	var rpcErr *jsonrpc.Error
	if !errors.As(err, &rpcErr) {
		return false
	}
	low := strings.ToLower(rpcErr.Message)
	for _, word := range []string{"unknown", "unexpected", "invalid", "unrecognized", "unsupported"} {
		if strings.Contains(low, word) {
			return true
		}
	}
	return false
}

func isTimeout(err error) bool {
	return err != nil && strings.Contains(err.Error(), "timeout")
}

// awaitTurnEnd waits out an interrupted turn so the next turn/start is
// accepted. The interrupt ack is not a turn-end signal, so poll the turn state
// instead of trusting the response.
func (p *codexProtocol) awaitTurnEnd(ctx context.Context, turnID string) {
	deadline := time.Now().Add(codexInterruptWait)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		done := !p.turnActive || (turnID != "" && p.turnID != turnID)
		p.mu.Unlock()
		if done {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// EncodeInterrupt sends turn/interrupt for the live turn.
func (p *codexProtocol) EncodeInterrupt() ([]byte, error) {
	p.mu.Lock()
	threadID, turnID := p.threadID, p.turnID
	p.mu.Unlock()
	if threadID == "" {
		return nil, errors.New("codex: thread not started")
	}
	params := map[string]any{"threadId": threadID}
	if turnID != "" {
		params["turnId"] = turnID
	}
	return wire.JSONLine(map[string]any{
		"jsonrpc": jsonrpc.Version, "id": p.client.NextID(),
		"method": "turn/interrupt", "params": params,
	})
}

// interruptTurn sends the interrupt without waiting for its ack: the reply
// arrives with no waiter and is ignored.
func (p *codexProtocol) interruptTurn() error {
	frame, err := p.EncodeInterrupt()
	if err != nil {
		return err
	}
	p.mu.Lock()
	w := p.w
	p.mu.Unlock()
	if w == nil {
		return errors.New("codex: not connected")
	}
	return w.Write(frame)
}

// EncodeDecision answers an approval request from the server.
func (p *codexProtocol) EncodeDecision(id string, d Decision) ([]byte, error) {
	p.mu.Lock()
	method := p.approvals[id]
	delete(p.approvals, id)
	p.mu.Unlock()
	rid, err := jsonrpc.ParseID([]byte(id))
	if err != nil {
		return nil, err
	}
	return wire.JSONLine(map[string]any{
		"jsonrpc": jsonrpc.Version, "id": rid, "result": codexDecisionResult(method, d),
	})
}

// codexDecisionResult renders the reply body for one approval method.
func codexDecisionResult(method string, d Decision) map[string]any {
	switch {
	case strings.Contains(method, "requestUserInput"):
		return map[string]any{"answers": map[string]any{}}
	case strings.Contains(method, "elicitation"):
		if d.Allow {
			return map[string]any{"action": "accept"}
		}
		return map[string]any{"action": "decline"}
	}
	if d.Allow {
		return map[string]any{"decision": "accept"}
	}
	result := map[string]any{"decision": "decline"}
	if d.Message != "" {
		result["reason"] = d.Message
	}
	return result
}

// Parse routes responses to the client and maps notifications to events.
func (p *codexProtocol) Parse(line []byte) []Event {
	kind, msg := p.client.Handle(line)
	switch kind {
	case jsonrpc.KindResponse:
		if msg.Err != nil {
			return []Event{{Type: EventError, Error: msg.Err.Error(), Code: "rpc_error", EndReason: string(FailProtocol)}}
		}
		return nil
	case jsonrpc.KindRequest:
		return p.parseServerRequest(msg)
	case jsonrpc.KindUnknown:
		return nil
	}
	params := map[string]any{}
	if len(msg.Params) > 0 {
		_ = json.Unmarshal(msg.Params, &params)
	}
	switch msg.Method {
	case "thread/started":
		if thread, _ := params["thread"].(map[string]any); thread != nil {
			id := str(thread["id"])
			p.mu.Lock()
			p.threadID = id
			p.mu.Unlock()
			return []Event{{Type: EventInit, SessionID: id}}
		}
	case "turn/started":
		if turn, _ := params["turn"].(map[string]any); turn != nil {
			p.mu.Lock()
			p.turnID = str(turn["id"])
			p.turnActive = true
			p.mu.Unlock()
		}
		return []Event{{Type: EventStatus, Status: StatusRunning}}
	case "turn/completed":
		return p.parseTurnCompleted(params)
	case "item/started", "item/completed", "item/updated":
		return p.parseItem(msg.Method, params)
	case "item/agentMessage/delta", "item/agent_message/delta":
		if text := str(params["delta"]); text != "" {
			return []Event{{Type: EventAssistant, Text: text, Delta: true}}
		}
	case "thread/tokenUsage/updated", "thread/token_usage/updated":
		if u := codexUsage(params); u != nil {
			return []Event{{Type: EventUsage, Usage: u}}
		}
	case "error":
		text := firstNonEmpty(str(params["message"]), fmt.Sprint(params["error"]))
		if text == "" {
			text = "codex reported an error"
		}
		c := ClassifyFor(Codex, text, 0)
		return []Event{{Type: EventError, Error: text, Code: c.Code, EndReason: string(c.Class)}}
	}
	return nil
}

// parseServerRequest maps a server to client request: the policy may answer it
// here, otherwise it becomes a permission event. A request the library cannot
// render a reply for is refused, so the agent never waits on an answer it
// cannot use.
func (p *codexProtocol) parseServerRequest(msg jsonrpc.Message) []Event {
	var params map[string]any
	if len(msg.Params) > 0 {
		_ = json.Unmarshal(msg.Params, &params)
	}
	method := msg.Method
	if !codexApprovalMethod(method) {
		if err := p.client.ReplyError(msg.ID, -32601, "agentwire: app-server method unsupported: "+method); err != nil {
			return []Event{{Type: EventError, Error: err.Error(), Code: "rpc_error", EndReason: string(FailProtocol)}}
		}
		return nil
	}
	// The permission id is the raw wire id text, so a numeric id and a string
	// id with the same digits stay distinct.
	id := string(msg.ID.Raw())
	p.mu.Lock()
	if p.approvals == nil {
		p.approvals = map[string]string{}
	}
	p.approvals[id] = method
	p.mu.Unlock()
	kind := codexApprovalKind(method)
	if d, ok := p.policy.Answer(kind, method); ok {
		if err := p.client.Reply(msg.ID, codexDecisionResult(method, d)); err != nil {
			p.mu.Lock()
			delete(p.approvals, id)
			p.mu.Unlock()
			return []Event{{Type: EventError, Error: err.Error(), Code: "rpc_error", EndReason: string(FailProtocol)}}
		}
		return nil
	}
	return []Event{{Type: EventPermission, Permission: &Permission{
		ID: id, Tool: method, Kind: kind, Question: codexApprovalQuestion(method, params),
	}}}
}

// codexApprovalMethod reports whether the app-server method is a request the
// library can answer: an approval the consumer decides, or an input request.
// Every other method gets an explicit refusal.
func codexApprovalMethod(method string) bool {
	switch {
	case strings.HasSuffix(method, "Approval"),
		strings.Contains(method, "requestUserInput"),
		strings.Contains(method, "elicitation"):
		return true
	}
	return false
}

// codexApprovalKind maps an approval method to a tool kind.
func codexApprovalKind(method string) ToolKind {
	switch {
	case strings.Contains(method, "fileChange"):
		return ToolEdit
	case strings.Contains(method, "commandExecution"):
		return ToolExec
	case strings.Contains(method, "permissions"):
		return ToolOther
	}
	return ToolOther
}

func codexApprovalQuestion(method string, params map[string]any) string {
	switch {
	case strings.Contains(method, "commandExecution"):
		if cmd := str(params["command"]); cmd != "" {
			return "Allow command: " + cmd
		}
	case strings.Contains(method, "fileChange"):
		return "Allow file changes"
	case strings.Contains(method, "permissions"):
		return "Allow requested permissions"
	case strings.Contains(method, "requestUserInput"):
		if q := str(params["question"]); q != "" {
			return q
		}
		return "Codex needs input"
	case strings.Contains(method, "elicitation"):
		if q := str(params["message"]); q != "" {
			return q
		}
		return "Codex requests input"
	case method == "execCommandApproval":
		return "Allow command execution"
	case method == "applyPatchApproval":
		return "Allow patch application"
	}
	return "Codex requests approval"
}

func (p *codexProtocol) parseTurnCompleted(params map[string]any) []Event {
	turn, _ := params["turn"].(map[string]any)
	status := ""
	if turn != nil {
		status = str(turn["status"])
	}
	p.mu.Lock()
	p.turnActive = false
	p.turnID = ""
	p.sawResult = true
	p.mu.Unlock()
	switch status {
	case "", "completed", "success":
		return []Event{{Type: EventResult, Result: &Result{Subtype: "success"}}}
	case "interrupted", "cancelled", "canceled":
		return []Event{{Type: EventResult, Result: &Result{Subtype: "interrupted", IsError: true, Text: "turn interrupted"}}}
	}
	text := ""
	if turn != nil {
		switch e := turn["error"].(type) {
		case string:
			text = e
		case map[string]any:
			text = firstNonEmpty(str(e["message"]), str(e["error"]))
		}
	}
	if text == "" {
		text = "turn " + status
	}
	c := ClassifyFor(Codex, text, 0)
	return []Event{{Type: EventResult, Result: &Result{
		Subtype: status, IsError: true, Text: text,
		EndReason: string(c.Class), Code: c.Code,
	}}}
}

func (p *codexProtocol) parseItem(method string, params map[string]any) []Event {
	item, _ := params["item"].(map[string]any)
	if item == nil {
		return nil
	}
	typ := str(item["type"])
	id := str(item["id"])
	completed := method == "item/completed"
	status := "started"
	if completed {
		status = "completed"
	}
	switch typ {
	case "agentMessage":
		if text := str(item["text"]); text != "" {
			return []Event{{Type: EventAssistant, Text: text, Delta: !completed}}
		}
	case "reasoning":
		return nil
	case "commandExecution":
		output := firstNonEmpty(str(item["aggregatedOutput"]), str(item["output"]))
		command := str(item["command"])
		if code := codexExitCode(item["exitCode"]); code != nil && *code != 0 {
			status = "failed"
		}
		return []Event{{Type: EventTool, Tool: &ToolEvent{
			ID: id, Name: "command", Kind: ToolExec, Input: command, Output: output, Status: status,
		}}}
	case "fileChange":
		return []Event{{Type: EventTool, Tool: &ToolEvent{
			ID: id, Name: "fileChange", Kind: ToolEdit, Paths: codexChangePaths(item), Status: status,
		}}}
	case "mcpToolCall":
		args := ""
		if raw, ok := item["arguments"]; ok {
			if b, err := json.Marshal(raw); err == nil {
				args = string(b)
			}
		}
		return []Event{{Type: EventTool, Tool: &ToolEvent{
			ID: id, Name: "mcp:" + str(item["tool"]), Kind: ToolMCP, Input: args, Status: status,
		}}}
	case "webSearch":
		return []Event{{Type: EventTool, Tool: &ToolEvent{ID: id, Name: "web_search", Kind: ToolSearch, Status: status}}}
	}
	return nil
}

// codexChangePaths lists the files a fileChange item touches.
func codexChangePaths(item map[string]any) []string {
	changes, _ := item["changes"].([]any)
	if len(changes) == 0 {
		return nil
	}
	out := make([]string, 0, len(changes))
	for _, c := range changes {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if p := firstNonEmpty(str(m["path"]), str(m["file"])); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Exit classifies a process exit that never produced a turn/completed frame.
func (p *codexProtocol) Exit(code int, err error, stderr string) []Event {
	p.mu.Lock()
	saw := p.sawResult
	p.mu.Unlock()
	if saw {
		return nil
	}
	text := strings.TrimSpace(stderr)
	if text == "" {
		if err != nil {
			text = err.Error()
		} else {
			text = fmt.Sprintf("codex exited with code %d before turn/completed", code)
		}
	}
	c := ClassifyFor(Codex, text, 0)
	return []Event{{Type: EventError, Error: text, Code: c.Code, EndReason: string(c.Class)}}
}

// inputItems renders a prompt as app-server input items. Images go first as
// localImage entries.
func (p *codexProtocol) inputItems(pr Prompt) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(pr.Attachments)+1)
	for _, a := range sortedAttachments(pr.Attachments) {
		data, media, err := loadAttachment(a)
		if err != nil {
			return nil, err
		}
		if !imageAttachment(media) {
			out = append(out, map[string]any{"type": "text", "text": string(data)})
			continue
		}
		path := a.Path
		if path == "" {
			path, err = p.writeTempImage(data, media)
			if err != nil {
				return nil, err
			}
		}
		out = append(out, map[string]any{"type": "localImage", "path": path})
	}
	out = append(out, map[string]any{"type": "text", "text": pr.Text})
	return out, nil
}

// writeTempImage stores an inline image so the app-server can read it from
// disk.
func (p *codexProtocol) writeTempImage(data []byte, media string) (string, error) {
	p.mu.Lock()
	dir := p.tempDir
	p.mu.Unlock()
	if dir == "" {
		d, err := os.MkdirTemp("", "agentwire-codex-")
		if err != nil {
			return "", err
		}
		if err := os.Chmod(d, 0o700); err != nil {
			_ = os.RemoveAll(d)
			return "", err
		}
		p.mu.Lock()
		p.tempDir = d
		p.mu.Unlock()
		dir = d
	}
	ext := ".bin"
	switch media {
	case "image/png":
		ext = ".png"
	case "image/jpeg":
		ext = ".jpg"
	case "image/gif":
		ext = ".gif"
	case "image/webp":
		ext = ".webp"
	}
	path := filepath.Join(dir, randomHex(8)+ext)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// codexThreadID reads the thread id from a thread/start or thread/resume
// response.
func codexThreadID(res map[string]any) string {
	if id := str(res["threadId"]); id != "" {
		return id
	}
	if t, _ := res["thread"].(map[string]any); t != nil {
		return str(t["id"])
	}
	return str(res["id"])
}

func codexUsage(params map[string]any) *Usage {
	tu, _ := params["tokenUsage"].(map[string]any)
	if tu == nil {
		tu = params
	}
	total, _ := tu["total"].(map[string]any)
	if total == nil {
		total = tu
	}
	u := &Usage{
		Input:         int64(num(total["inputTokens"]) + num(total["input_tokens"])),
		Output:        int64(num(total["outputTokens"]) + num(total["output_tokens"])),
		CacheRead:     int64(num(total["cachedInputTokens"]) + num(total["cached_input_tokens"])),
		CacheCreation: int64(num(total["cacheCreationInputTokens"]) + num(total["cache_creation_input_tokens"])),
	}
	if u.Input == 0 && u.Output == 0 && u.CacheRead == 0 && u.CacheCreation == 0 {
		return nil
	}
	return u
}

func codexExitCode(v any) *int {
	if v == nil {
		return nil
	}
	n := int(num(v))
	return &n
}
