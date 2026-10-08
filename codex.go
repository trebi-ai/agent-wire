package agentwire

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
	// codexPromptAckWait bounds the turn/start ack wait: the turn is live even
	// if the pin streams notifications without an RPC response.
	codexPromptAckWait = 2 * time.Second
	// codexSteerAckWait bounds the turn/steer ack wait.
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
	return &codexSession{Session: s, p: proto}, nil
}

// codexSession adds the model and steer calls to the wire session.
type codexSession struct {
	*wire.Session
	p *codexProtocol
}

// SetModel changes the model of the next turn/start. The running turn keeps
// its model.
func (s *codexSession) SetModel(_ context.Context, model string) error {
	s.p.mu.Lock()
	s.p.model = model
	s.p.mu.Unlock()
	return nil
}

// Steer sends turn/steer for the running turn.
func (s *codexSession) Steer(ctx context.Context, pr Prompt) error { return s.p.Steer(ctx, pr) }

// Models implements ModelLister. It starts `codex app-server`, reads every
// page of model/list, and kills the child. Hidden models are left out.
func (d codexDriver) Models(ctx context.Context, q ModelQuery) ([]ModelInfo, error) {
	var models []ModelInfo
	err := d.probe(ctx, q, modelProbeTimeout, func(ctx context.Context, client *jsonrpc.Client) error {
		cursor := ""
		for range 50 {
			params := map[string]any{}
			if cursor != "" {
				params["cursor"] = cursor
			}
			raw, err := client.Call(ctx, "model/list", params, modelProbeTimeout)
			if err != nil {
				return err
			}
			page, next, err := codexModelPage(raw)
			if err != nil {
				return err
			}
			models = append(models, page...)
			if next == "" {
				return nil
			}
			cursor = next
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return models, nil
}

// ReadLimits implements LimitsReader. It starts `codex app-server`, reads
// account/rateLimits/read, and kills the child.
func (d codexDriver) ReadLimits(ctx context.Context, q ModelQuery) (Limits, error) {
	var out Limits
	err := d.probe(ctx, q, limitsProbeTimeout, func(ctx context.Context, client *jsonrpc.Client) error {
		raw, err := client.Call(ctx, "account/rateLimits/read", map[string]any{"excludeResetCreditDetails": true}, limitsProbeTimeout)
		if err != nil {
			return err
		}
		if l := codexLimits(raw); l != nil {
			out = *l
		}
		return nil
	})
	if err != nil {
		return Limits{}, err
	}
	return out, nil
}

// probe starts a short-lived `codex app-server`, runs the handshake, then fn.
func (d codexDriver) probe(ctx context.Context, q ModelQuery, timeout time.Duration, fn func(context.Context, *jsonrpc.Client) error) error {
	dir, done, err := probeDir(q)
	if err != nil {
		return err
	}
	defer done()
	req := probeRequest(Codex, q, dir)
	l, err := d.rt.prepare(ctx, &req, false)
	if err != nil {
		return err
	}
	defer l.cleanup()
	client := jsonrpc.New(nil, timeout)
	a := probeAdapter{
		run: func(ctx context.Context, w *wire.Writer) error {
			client.SetWrite(w.Write)
			if _, err := client.Call(ctx, "initialize", map[string]any{
				"clientInfo": map[string]any{"name": d.rt.clientName(), "title": d.rt.clientName(), "version": d.rt.opts.ClientVersion},
			}, timeout); err != nil {
				return err
			}
			if err := client.Notify("initialized", map[string]any{}); err != nil {
				return err
			}
			return fn(ctx, client)
		},
		parse: func(line []byte) { client.Handle(line) },
	}
	args := append(append([]string{}, l.args...), "app-server")
	return d.rt.runProbe(ctx, Codex, l.bin, args, dir, l.env, a)
}

// codexModelPage maps one model/list response and returns the next cursor.
func codexModelPage(raw json.RawMessage) ([]ModelInfo, string, error) {
	var res struct {
		Data []struct {
			ID          string `json:"id"`
			Model       string `json:"model"`
			DisplayName string `json:"displayName"`
			Description string `json:"description"`
			Hidden      bool   `json:"hidden"`
			IsDefault   bool   `json:"isDefault"`
		} `json:"data"`
		NextCursor *string `json:"nextCursor"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, "", fmt.Errorf("codex model/list: decode response: %w", err)
	}
	out := make([]ModelInfo, 0, len(res.Data))
	for _, m := range res.Data {
		id := firstNonEmpty(m.Model, m.ID)
		if id == "" || m.Hidden {
			continue
		}
		out = append(out, ModelInfo{ID: id, Name: m.DisplayName, Description: m.Description, Default: m.IsDefault})
	}
	next := ""
	if res.NextCursor != nil {
		next = *res.NextCursor
	}
	return out, next, nil
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
	// turnUsage is the usage of the open turn. turnBase is the thread total
	// before the first model call of the turn.
	turnUsage Usage
	turnBase  *Usage
	// approvals maps a server request id to its method, so a later decision
	// knows which reply body to render.
	approvals map[string]string
	// limitsReq is the id of the account/rateLimits/read request. Its error
	// reply is dropped: an API-key login has no plan limits.
	limitsReq string

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
	clientName := p.rt.clientName()
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
	p.requestLimits()
	if id := codexThreadID(payload); id != "" {
		p.mu.Lock()
		p.threadID = id
		p.mu.Unlock()
		return []Event{{Type: EventInit, SessionID: id}}, nil
	}
	return nil, nil
}

// requestLimits sends account/rateLimits/read once. Parse maps the reply.
func (p *codexProtocol) requestLimits() {
	id, err := p.client.Request("account/rateLimits/read", map[string]any{"excludeResetCreditDetails": true})
	if err != nil {
		return
	}
	p.mu.Lock()
	p.limitsReq = id.String()
	p.mu.Unlock()
}

// limitsReply reports whether msg answers the limits request, and maps it.
func (p *codexProtocol) limitsReply(msg jsonrpc.Message) ([]Event, bool) {
	p.mu.Lock()
	match := p.limitsReq != "" && msg.ID.String() == p.limitsReq
	if match {
		p.limitsReq = ""
	}
	p.mu.Unlock()
	if !match {
		return nil, false
	}
	if msg.Err != nil {
		return nil, true
	}
	if l := codexLimits(msg.Result); l != nil {
		return []Event{{Type: EventLimits, Limits: l}}, true
	}
	return nil, true
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

// PromptSession starts a turn with turn/start. It returns ErrTurnActive while
// a turn runs; Send steers or holds the prompt instead.
func (p *codexProtocol) PromptSession(ctx context.Context, pr Prompt) error {
	p.mu.Lock()
	threadID, active := p.threadID, p.turnActive
	p.mu.Unlock()
	if threadID == "" {
		return errors.New("codex: thread not started")
	}
	if active {
		return ErrTurnActive
	}
	input, err := p.inputItems(pr)
	if err != nil {
		return err
	}
	return p.startTurn(ctx, threadID, input)
}

// TurnActive implements wire.TurnStater.
func (p *codexProtocol) TurnActive() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.turnActive
}

// Steer adds input to the running turn with turn/steer. It never interrupts.
func (p *codexProtocol) Steer(ctx context.Context, pr Prompt) error {
	p.mu.Lock()
	threadID, turnID, active := p.threadID, p.turnID, p.turnActive
	p.mu.Unlock()
	if threadID == "" {
		return errors.New("codex: thread not started")
	}
	if !active {
		return ErrNoActiveTurn
	}
	input, err := p.inputItems(pr)
	if err != nil {
		return err
	}
	return p.steer(ctx, threadID, turnID, input)
}

// steer sends turn/steer. A refused or unanswered steer wraps ErrUnsupported.
func (p *codexProtocol) steer(ctx context.Context, threadID, turnID string, input []map[string]any) error {
	params := map[string]any{"threadId": threadID, "input": input}
	if turnID != "" {
		params["expectedTurnId"] = turnID
	}
	_, err := p.client.Call(ctx, "turn/steer", params, codexSteerAckWait)
	if err == nil || ctx.Err() != nil {
		return err
	}
	return fmt.Errorf("%w: codex turn/steer: %v", ErrUnsupported, err)
}

// startTurn sends turn/start. The turn is live even when the pin streams
// notifications without an RPC ack, so an ack timeout is not an error.
func (p *codexProtocol) startTurn(ctx context.Context, threadID string, input []map[string]any) error {
	params := map[string]any{"threadId": threadID, "input": input}
	p.mu.Lock()
	if p.model != "" {
		params["model"] = p.model
	}
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
		if evs, ok := p.limitsReply(msg); ok {
			return evs
		}
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
			p.turnUsage = Usage{}
			p.turnBase = nil
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
		last, total := codexTokenUsage(params)
		p.noteTurnUsage(str(params["turnId"]), last, total)
		if total != nil {
			return []Event{{Type: EventUsage, Usage: total}}
		}
	case "account/rateLimits/updated":
		if l := codexLimits(msg.Params); l != nil {
			return []Event{{Type: EventLimits, Limits: l}}
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
	usage := p.turnUsage
	p.turnUsage = Usage{}
	p.turnBase = nil
	p.mu.Unlock()
	switch status {
	case "", "completed", "success":
		return []Event{{Type: EventResult, Result: &Result{Subtype: "success", Usage: usage}}}
	case "interrupted", "cancelled", "canceled":
		return []Event{{Type: EventResult, Result: &Result{Subtype: "interrupted", IsError: true, Text: "turn interrupted", Usage: usage}}}
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
		EndReason: string(c.Class), Code: c.Code, Usage: usage,
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

// noteTurnUsage adds one token usage notification to the open turn. Codex
// sends "last" for one model call and "total" for the thread, and a turn can
// make many model calls. The turn usage is the total now minus the total
// before the first call of the turn. A repeated notification adds nothing.
func (p *codexProtocol) noteTurnUsage(turnID string, last, total *Usage) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.turnActive || (turnID != "" && p.turnID != "" && turnID != p.turnID) {
		return
	}
	switch {
	case total != nil:
		if p.turnBase == nil {
			base := *total
			if last != nil {
				base = codexUsageSub(base, *last)
			}
			p.turnBase = &base
		}
		p.turnUsage = codexUsageSub(*total, *p.turnBase)
	case last != nil:
		p.turnUsage.Input += last.Input
		p.turnUsage.Output += last.Output
		p.turnUsage.CacheRead += last.CacheRead
		p.turnUsage.CacheCreation += last.CacheCreation
	}
}

// codexTokenUsage reads the ThreadTokenUsage of a thread/tokenUsage/updated
// notification: "last" is one model call, "total" is the thread.
func codexTokenUsage(params map[string]any) (last, total *Usage) {
	tu, _ := params["tokenUsage"].(map[string]any)
	if tu == nil {
		tu = params
	}
	lm, _ := tu["last"].(map[string]any)
	tm, _ := tu["total"].(map[string]any)
	if lm == nil && tm == nil {
		return nil, codexBreakdown(tu)
	}
	return codexBreakdown(lm), codexBreakdown(tm)
}

// codexBreakdown reads one TokenUsageBreakdown. It returns nil when every
// counter is 0.
func codexBreakdown(m map[string]any) *Usage {
	if m == nil {
		return nil
	}
	u := &Usage{
		Input:         int64(num(m["inputTokens"]) + num(m["input_tokens"])),
		Output:        int64(num(m["outputTokens"]) + num(m["output_tokens"])),
		CacheRead:     int64(num(m["cachedInputTokens"]) + num(m["cached_input_tokens"])),
		CacheCreation: int64(num(m["cacheWriteInputTokens"]) + num(m["cache_write_input_tokens"])),
	}
	if u.Input == 0 && u.Output == 0 && u.CacheRead == 0 && u.CacheCreation == 0 {
		return nil
	}
	return u
}

// codexUsageSub returns a minus b. A counter never goes below 0.
func codexUsageSub(a, b Usage) Usage {
	sub := func(x, y int64) int64 { return max(x-y, 0) }
	return Usage{
		Input:         sub(a.Input, b.Input),
		Output:        sub(a.Output, b.Output),
		CacheRead:     sub(a.CacheRead, b.CacheRead),
		CacheCreation: sub(a.CacheCreation, b.CacheCreation),
	}
}

func codexExitCode(v any) *int {
	if v == nil {
		return nil
	}
	n := int(num(v))
	return &n
}

// ensure the Codex session has the optional capabilities.
var (
	_ ModelLister = codexDriver{}
	_ ModelSetter = (*codexSession)(nil)
	_ Steerer     = (*codexSession)(nil)
)

// codexRateLimitSnapshot is the app-server RateLimitSnapshot.
type codexRateLimitSnapshot struct {
	LimitID              string                `json:"limitId"`
	Primary              *codexRateLimitWindow `json:"primary"`
	Secondary            *codexRateLimitWindow `json:"secondary"`
	PlanType             string                `json:"planType"`
	RateLimitReachedType string                `json:"rateLimitReachedType"`
}

// codexRateLimitWindow is the app-server RateLimitWindow. ResetsAt is epoch
// seconds.
type codexRateLimitWindow struct {
	UsedPercent        float64 `json:"usedPercent"`
	WindowDurationMins *int    `json:"windowDurationMins"`
	ResetsAt           *int64  `json:"resetsAt"`
}

// codexLimits maps the body of account/rateLimits/read or of the
// account/rateLimits/updated notification. Windows are keyed by length,
// because primary is not always the 5-hour window.
func codexLimits(raw json.RawMessage) *Limits {
	var body struct {
		RateLimits          *codexRateLimitSnapshot           `json:"rateLimits"`
		RateLimitsByLimitID map[string]codexRateLimitSnapshot `json:"rateLimitsByLimitId"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil
	}
	snaps := []codexRateLimitSnapshot{}
	if body.RateLimits != nil {
		snaps = append(snaps, *body.RateLimits)
	}
	ids := make([]string, 0, len(body.RateLimitsByLimitID))
	for id := range body.RateLimitsByLimitID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		snap := body.RateLimitsByLimitID[id]
		if snap.LimitID == "" {
			snap.LimitID = id
		}
		snaps = append(snaps, snap)
	}
	l := &Limits{}
	seen := map[string]bool{}
	for _, snap := range snaps {
		if l.Plan == "" {
			l.Plan = snap.PlanType
		}
		if snap.RateLimitReachedType != "" {
			l.Rejected = true
		}
		scope := snap.LimitID
		if scope == "codex" {
			scope = ""
		}
		for _, w := range []*codexRateLimitWindow{snap.Primary, snap.Secondary} {
			if w == nil {
				continue
			}
			lw := LimitWindow{Scope: scope, UsedPercent: w.UsedPercent}
			if w.WindowDurationMins != nil {
				lw.Minutes = *w.WindowDurationMins
			}
			lw.Key = WindowKey(lw.Minutes)
			if w.ResetsAt != nil && *w.ResetsAt > 0 {
				lw.ResetsAt = time.Unix(*w.ResetsAt, 0).UTC()
			}
			k := fmt.Sprintf("%s|%s|%d", lw.Key, lw.Scope, lw.Minutes)
			if seen[k] {
				continue
			}
			seen[k] = true
			l.Windows = append(l.Windows, lw)
		}
	}
	if len(l.Windows) == 0 && !l.Rejected && l.Plan == "" {
		return nil
	}
	return l
}
