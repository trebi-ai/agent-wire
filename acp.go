package agentwire

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/trebi-ai/agent-wire/internal/jsonrpc"
	"github.com/trebi-ai/agent-wire/internal/proc"
	"github.com/trebi-ai/agent-wire/internal/wire"
)

// acpKind distinguishes the ACP agents that share this driver.
//
// Copilot runs `copilot --acp`. Gemini runs `gemini --experimental-acp`, or
// `--acp` in newer releases; the launch builder picks the flag the installed
// binary advertises. Cursor runs `cursor-agent acp`, a hidden subcommand that
// `--help` does not list, and a new session mints a chat id with
// `cursor-agent create-chat` first, because the agent has no method that opens
// a chat the caller does not already know.
type acpKind string

const (
	acpCopilot acpKind = "copilot"
	acpCursor  acpKind = "cursor"
	acpGemini  acpKind = "gemini"
)

// harness is the Agentwire harness name of the kind.
func (k acpKind) harness() Harness {
	switch k {
	case acpCopilot:
		return Copilot
	case acpCursor:
		return Cursor
	case acpGemini:
		return Gemini
	}
	return Harness(k)
}

const acpHandshakeTimeout = 45 * time.Second

// acpDriver is the Agent Client Protocol driver shared by Copilot, Cursor and
// Gemini.
type acpDriver struct {
	rt   *Runtime
	kind acpKind
}

func (d acpDriver) Name() string { return string(d.kind.harness()) }

func (d acpDriver) Start(ctx context.Context, req StartRequest) (Session, error) {
	return d.start(ctx, req, false)
}

func (d acpDriver) Resume(ctx context.Context, req StartRequest) (Session, error) {
	return d.start(ctx, req, true)
}

func (d acpDriver) start(ctx context.Context, req StartRequest, resume bool) (Session, error) {
	fresh := false
	if d.kind == acpCursor && !resume {
		// A Cursor session starts as a fresh chat: the id must exist before the
		// agent can load it, and it is never a resume of an older chat.
		id, err := d.createCursorChat(ctx, req.Binary)
		if err != nil {
			return nil, err
		}
		req.SessionID = id
		fresh = true
	}
	l, err := d.rt.prepare(ctx, &req, resume)
	if err != nil {
		return nil, err
	}
	args, err := d.acpArgs(ctx, l, req, resume)
	if err != nil {
		return nil, err
	}
	proto := newACPProtocol(d.rt, d.kind, req, l, fresh)
	p, err := proc.Start(ctx, proc.Opts{
		Path: l.bin, Args: args, Dir: req.WorkingDir,
		Env: proc.ChildEnv(l.env), OnStderr: d.rt.stderrHook(d.kind.harness(), req.OnStderr),
	})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", d.kind, err)
	}
	s, err := wire.Start(ctx, string(d.kind.harness()), p, proto, d.rt.wireConfig(req.LogPath))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", d.kind, err)
	}
	d.rt.bindSession(s, l)
	if id := proto.currentSession(); id != "" {
		s.SetID(id)
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

// acpArgs adds the subcommand or flag that selects the ACP transport.
func (d acpDriver) acpArgs(ctx context.Context, l *launch, req StartRequest, resume bool) ([]string, error) {
	switch d.kind {
	case acpCopilot:
		args := append([]string{}, l.args...)
		if !hasFlag(args, "--acp") {
			args = append(args, "--acp")
		}
		return args, nil
	case acpCursor:
		// The acp subcommand replaces the run-mode flags, and the session id
		// travels on the wire (create-chat plus session/load).
		args := append([]string{}, stripCursorArgs(l.args)...)
		return append(args, "acp"), nil
	case acpGemini:
		args := append([]string{}, l.args...)
		help := d.rt.HelpText(ctx, l.bin)
		switch {
		case strings.Contains(help, "--experimental-acp"):
			args = append(args, "--experimental-acp")
		case strings.Contains(help, "--acp"):
			args = append(args, "--acp")
		default:
			// The documented form. A binary without it fails loudly in the
			// handshake, which is better than silently opening a TUI.
			args = append(args, "--experimental-acp")
		}
		return args, nil
	}
	return append([]string{}, l.args...), nil
}

// createCursorChat mints a chat id with `cursor-agent create-chat`.
func (d acpDriver) createCursorChat(ctx context.Context, binary string) (string, error) {
	bin := binary
	if bin == "" {
		resolved, err := resolveBinary(&StartRequest{Harness: Cursor})
		if err != nil {
			return "", err
		}
		bin = resolved
	}
	runCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(runCtx, bin, "create-chat").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("cursor create-chat: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	id := lastIdentifier(string(out))
	if id == "" {
		return "", errors.New("cursor create-chat returned no chat id")
	}
	return id, nil
}

// lastIdentifier picks the last uuid-shaped token in text, else the last
// non-empty line.
func lastIdentifier(text string) string {
	lines := strings.Split(text, "\n")
	last := ""
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		last = line
		for _, field := range strings.Fields(line) {
			field = strings.Trim(field, `"'.,`)
			if looksLikeUUID(field) {
				return field
			}
		}
	}
	return strings.Trim(last, `"' `)
}

func looksLikeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// stripCursorArgs removes run-mode flags that conflict with the acp
// subcommand.
func stripCursorArgs(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--resume" || a == "--continue" || a == "-c":
			i++
		case strings.HasPrefix(a, "--resume=") || strings.HasPrefix(a, "--continue="):
		default:
			out = append(out, a)
		}
	}
	return out
}

// acpOption is one permission option the agent offers.
type acpOption struct {
	ID   string
	Kind string
}

// acpProtocol maps Agent Client Protocol JSON-RPC to events.
type acpProtocol struct {
	client *jsonrpc.Client
	kind   acpKind
	policy PermissionPolicy
	fs     FileSystem

	mu sync.Mutex
	w  *wire.Writer

	handshakeTimeout time.Duration
	cwd              string
	model            string
	clientName       string
	clientVersion    string
	mcpServers       []MCPServer
	instructions     string
	authMethod       string
	trustHTTPMCP     bool

	resumeID     string
	freshSession bool
	sessionID    string
	firstPrompt  bool

	promptID    string
	sawResult   bool
	approvals   map[string][]acpOption
	authMethods []string
}

func newACPProtocol(rt *Runtime, kind acpKind, req StartRequest, l *launch, fresh bool) *acpProtocol {
	timeout := req.HandshakeTimeout
	if timeout <= 0 {
		timeout = acpHandshakeTimeout
	}
	name := rt.opts.ClientName
	if name == "" {
		name = "agentwire"
	}
	p := &acpProtocol{
		kind:             kind,
		policy:           req.Permissions.Normalized(),
		fs:               req.FS,
		handshakeTimeout: timeout,
		cwd:              req.WorkingDir,
		model:            req.Model,
		clientName:       name,
		clientVersion:    rt.opts.ClientVersion,
		mcpServers:       req.MCPServers,
		instructions:     l.instructions,
		authMethod:       req.RawString("acp_auth_method"),
		resumeID:         req.SessionID,
		freshSession:     fresh,
		firstPrompt:      strings.TrimSpace(l.instructions) != "",
		approvals:        map[string][]acpOption{},
	}
	p.client = jsonrpc.New(nil, timeout)
	return p
}

func (p *acpProtocol) SetWriter(w *wire.Writer) {
	p.mu.Lock()
	p.w = w
	p.mu.Unlock()
	p.client.SetWrite(w.Write)
}

// Handshake negotiates capabilities and opens or loads a session.
func (p *acpProtocol) Handshake(ctx context.Context, _ *wire.Writer) ([]Event, error) {
	caps := map[string]any{
		"terminal": false,
		"auth":     map[string]any{"terminal": false},
	}
	if p.fs != nil {
		// Advertise the file system so every agent write can be observed or
		// approved by the consumer.
		caps["fs"] = map[string]any{"readTextFile": true, "writeTextFile": true}
	} else {
		caps["fs"] = map[string]any{"readTextFile": false, "writeTextFile": false}
	}
	raw, err := p.client.Call(ctx, "initialize", map[string]any{
		"protocolVersion":    1,
		"clientCapabilities": caps,
		"clientInfo":         map[string]any{"name": p.clientName, "version": p.clientVersion},
	}, p.handshakeTimeout)
	if err != nil {
		return nil, err
	}
	var init struct {
		AuthMethods       []map[string]any `json:"authMethods"`
		AgentCapabilities struct {
			MCPCapabilities struct {
				HTTP bool `json:"http"`
			} `json:"mcpCapabilities"`
		} `json:"agentCapabilities"`
	}
	_ = json.Unmarshal(raw, &init)
	p.mu.Lock()
	for _, m := range init.AuthMethods {
		if id := str(m["id"]); id != "" {
			p.authMethods = append(p.authMethods, id)
		}
	}
	p.trustHTTPMCP = init.AgentCapabilities.MCPCapabilities.HTTP
	p.mu.Unlock()

	events := p.openOrLoad(ctx)
	if len(events) == 0 {
		return nil, errors.New(string(p.kind) + ": handshake produced no session")
	}
	if events[0].Type == EventError {
		return nil, errors.New(events[0].Error)
	}
	if p.model != "" {
		events = append(events, p.applyModel(ctx)...)
	}
	return events, nil
}

// openOrLoad resumes a known session or opens a new one, authenticating once
// when the agent demands it.
func (p *acpProtocol) openOrLoad(ctx context.Context) []Event {
	if p.resumeID != "" {
		p.mu.Lock()
		p.sessionID = p.resumeID
		p.mu.Unlock()
		events, err := p.loadSession(ctx)
		if err == nil {
			return events
		}
		if !errors.Is(err, ErrResumeUnsupported) {
			return []Event{{Type: EventError, Error: err.Error(), Code: "resume_failed", EndReason: string(FailProtocol)}}
		}
	}
	events, err := p.newSession(ctx)
	if err != nil {
		if authErr := p.authenticate(ctx); authErr == nil {
			events, err = p.newSession(ctx)
		} else if p.resumeID != "" {
			// The session may exist but need the login first.
			if retry, retryErr := p.loadSession(ctx); retryErr == nil {
				return retry
			}
		}
	}
	if err != nil {
		return []Event{{Type: EventError, Error: err.Error(), Code: "session_open_failed", EndReason: string(FailProtocol)}}
	}
	return events
}

// sessionParams renders the session-open parameters, including the injected
// MCP servers.
func (p *acpProtocol) sessionParams() map[string]any {
	params := map[string]any{"cwd": p.cwd}
	p.mu.Lock()
	httpOK := p.trustHTTPMCP
	p.mu.Unlock()
	servers := acpMCPServers(p.mcpServers, httpOK)
	if servers == nil {
		servers = []map[string]any{}
	}
	params["mcpServers"] = servers
	return params
}

func (p *acpProtocol) newSession(ctx context.Context) ([]Event, error) {
	raw, err := p.client.Call(ctx, "session/new", p.sessionParams(), p.handshakeTimeout)
	if err != nil {
		return nil, err
	}
	id := acpSessionID(raw)
	if id == "" {
		return nil, fmt.Errorf("%s: session/new returned no sessionId", p.kind)
	}
	p.mu.Lock()
	p.sessionID = id
	p.mu.Unlock()
	return []Event{{Type: EventInit, SessionID: id}}, nil
}

func (p *acpProtocol) loadSession(ctx context.Context) ([]Event, error) {
	p.mu.Lock()
	sessionID := p.sessionID
	fresh := p.freshSession
	p.mu.Unlock()
	params := p.sessionParams()
	params["sessionId"] = sessionID
	if !fresh {
		raw, err := p.client.Call(ctx, "session/resume", params, p.handshakeTimeout)
		if err == nil {
			return p.sessionOpened(raw, sessionID), nil
		}
	}
	raw, err := p.client.Call(ctx, "session/load", params, p.handshakeTimeout)
	if err != nil {
		return nil, fmt.Errorf("%w: %s session %s: %v", ErrResumeUnsupported, p.kind, sessionID, err)
	}
	return p.sessionOpened(raw, sessionID), nil
}

func (p *acpProtocol) sessionOpened(raw json.RawMessage, fallback string) []Event {
	id := acpSessionID(raw)
	if id == "" {
		id = fallback
	}
	p.mu.Lock()
	p.sessionID = id
	p.mu.Unlock()
	return []Event{{Type: EventInit, SessionID: id}}
}

// applyModel asks the agent to switch model, when it offered a model list.
func (p *acpProtocol) applyModel(ctx context.Context) []Event {
	p.mu.Lock()
	sessionID := p.sessionID
	p.mu.Unlock()
	_, err := p.client.Call(ctx, "session/set_model", map[string]any{
		"sessionId": sessionID, "modelId": p.model,
	}, 10*time.Second)
	if err != nil {
		return []Event{{Type: EventStatus, Status: StatusRunning, Text: "model not applied: " + err.Error()}}
	}
	return []Event{{Type: EventStatus, Status: StatusRunning, Text: "model " + p.model}}
}

// authenticate picks an auth method and reports the choice, so a consumer can
// tell which login the session used.
func (p *acpProtocol) authenticate(ctx context.Context) error {
	p.mu.Lock()
	methods := append([]string{}, p.authMethods...)
	preferred := p.authMethod
	p.mu.Unlock()
	if len(methods) == 0 {
		return errors.New("no auth methods")
	}
	pick := preferred
	if pick == "" {
		pick = methods[0]
		for _, m := range methods {
			if p.kind == acpCursor && strings.Contains(m, "cursor_login") {
				pick = m
			}
			if p.kind == acpCopilot && (strings.Contains(m, "github") || strings.Contains(m, "device")) {
				pick = m
			}
		}
	}
	_, err := p.client.Call(ctx, "authenticate", map[string]any{"methodId": pick}, p.handshakeTimeout)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.authMethod = pick
	p.mu.Unlock()
	return nil
}

// EncodePrompt sends session/prompt. Its response is the turn completion.
func (p *acpProtocol) EncodePrompt(pr Prompt) ([]byte, error) {
	p.mu.Lock()
	p.sawResult = false
	id := p.client.NextID()
	p.promptID = string(id.Raw())
	sessionID := p.sessionID
	first := p.firstPrompt
	p.firstPrompt = false
	instructions := p.instructions
	p.mu.Unlock()
	content, err := p.promptContent(pr, first, instructions)
	if err != nil {
		return nil, err
	}
	return wire.JSONLine(map[string]any{
		"jsonrpc": jsonrpc.Version, "id": id, "method": "session/prompt",
		"params": map[string]any{"sessionId": sessionID, "prompt": content},
	})
}

// promptContent renders the ACP prompt blocks: images first, then the text
// with the session instructions in a fenced block on the first turn.
func (p *acpProtocol) promptContent(pr Prompt, first bool, instructions string) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(pr.Attachments)+1)
	for _, a := range sortedAttachments(pr.Attachments) {
		data, media, err := loadAttachment(a)
		if err != nil {
			return nil, err
		}
		if imageAttachment(media) {
			out = append(out, map[string]any{"type": "image", "mimeType": media, "data": encodeBase64(data)})
			continue
		}
		out = append(out, map[string]any{"type": "text", "text": string(data)})
	}
	out = append(out, map[string]any{"type": "text", "text": withInstructions(instructions, pr.Text, first)})
	return out, nil
}

// EncodeInterrupt sends the session/cancel notification.
func (p *acpProtocol) EncodeInterrupt() ([]byte, error) {
	p.mu.Lock()
	sessionID := p.sessionID
	p.mu.Unlock()
	return wire.JSONLine(map[string]any{
		"jsonrpc": jsonrpc.Version, "method": "session/cancel",
		"params": map[string]any{"sessionId": sessionID},
	})
}

// EncodeDecision answers session/request_permission.
func (p *acpProtocol) EncodeDecision(id string, d Decision) ([]byte, error) {
	p.mu.Lock()
	options := p.approvals[id]
	delete(p.approvals, id)
	p.mu.Unlock()
	rid, err := jsonrpc.ParseID([]byte(id))
	if err != nil {
		return nil, err
	}
	outcome := map[string]any{"outcome": "cancelled"}
	if d.Allow {
		if option := pickACPOption(options, true); option != "" {
			outcome = map[string]any{"outcome": "selected", "optionId": option}
		}
	} else if option := pickACPOption(options, false); option != "" {
		outcome = map[string]any{"outcome": "selected", "optionId": option}
	}
	return wire.JSONLine(map[string]any{
		"jsonrpc": jsonrpc.Version, "id": rid, "result": map[string]any{"outcome": outcome},
	})
}

func pickACPOption(options []acpOption, allow bool) string {
	for _, o := range options {
		if allow && (o.Kind == "allow_once" || o.Kind == "allow_always") {
			return o.ID
		}
		if !allow && (o.Kind == "reject_once" || o.Kind == "reject_always") {
			return o.ID
		}
	}
	if len(options) == 0 {
		return ""
	}
	if allow {
		return options[0].ID
	}
	return options[len(options)-1].ID
}

// Parse routes responses, maps session/update notifications, answers the file
// system methods when a FileSystem is set, and refuses the rest so the agent
// never hangs.
func (p *acpProtocol) Parse(line []byte) []Event {
	kind, msg := p.client.Handle(line)
	switch kind {
	case jsonrpc.KindRequest:
		return p.handleAgentRequest(msg)
	case jsonrpc.KindResponse:
		return p.parseResponse(msg)
	case jsonrpc.KindUnknown:
		return nil
	}
	if msg.Method != "session/update" {
		return nil
	}
	var params struct {
		SessionID string         `json:"sessionId"`
		Update    map[string]any `json:"update"`
	}
	if len(msg.Params) > 0 {
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			return nil
		}
	}
	if params.SessionID != "" && params.SessionID != p.currentSession() {
		return nil
	}
	if params.Update == nil {
		return nil
	}
	return p.parseUpdate(params.Update)
}

func (p *acpProtocol) parseUpdate(update map[string]any) []Event {
	switch str(update["sessionUpdate"]) {
	case "agent_message_chunk":
		content, _ := update["content"].(map[string]any)
		if content != nil && str(content["type"]) == "text" {
			if text := str(content["text"]); text != "" {
				return []Event{{Type: EventAssistant, Text: text}}
			}
		}
	case "agent_thought_chunk", "plan", "plan_update", "available_commands_update", "current_mode_update":
		return []Event{{Type: EventStatus, Status: StatusRunning}}
	case "usage_update":
		if u := acpUsage(update); u != nil {
			return []Event{{Type: EventUsage, Usage: u}}
		}
	case "tool_call":
		return []Event{{Type: EventTool, Tool: p.toolEvent(update, "started")}}
	case "tool_call_update":
		status := str(update["status"])
		switch status {
		case "completed", "failed":
		default:
			status = "started"
		}
		return []Event{{Type: EventTool, Tool: p.toolEvent(update, status)}}
	case "user_message_chunk":
		return nil
	}
	return nil
}

// toolEvent renders one tool call or tool call update.
func (p *acpProtocol) toolEvent(update map[string]any, status string) *ToolEvent {
	name := firstNonEmpty(str(update["title"]), str(update["kind"]), "tool")
	kind := classifyToolName(firstNonEmpty(str(update["kind"]), name))
	input := ""
	if raw, ok := update["rawInput"]; ok {
		if b, err := json.Marshal(raw); err == nil {
			input = string(b)
		}
	}
	return &ToolEvent{
		ID: str(update["toolCallId"]), Name: name, Kind: kind,
		Input: input, Output: acpToolOutput(update["content"]), Status: status,
		Paths: toolPaths(kind, input),
	}
}

// acpToolOutput renders ACP tool call content blocks: nested text, plain text
// and diff paths.
func acpToolOutput(v any) string {
	list, _ := v.([]any)
	var parts []string
	for _, item := range list {
		m, _ := item.(map[string]any)
		if m == nil {
			continue
		}
		if inner, _ := m["content"].(map[string]any); inner != nil {
			if t := str(inner["text"]); t != "" {
				parts = append(parts, t)
				continue
			}
		}
		if t := str(m["text"]); t != "" {
			parts = append(parts, t)
		}
		if p := str(m["path"]); p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, "\n")
}

// parseResponse turns a session/prompt response into the terminal turn event.
func (p *acpProtocol) parseResponse(msg jsonrpc.Message) []Event {
	p.mu.Lock()
	promptID := p.promptID
	p.mu.Unlock()
	if string(msg.ID.Raw()) != promptID {
		return nil
	}
	// A new turn resets the latch, so a crash after turn one still classifies.
	p.mu.Lock()
	p.sawResult = true
	p.promptID = ""
	p.mu.Unlock()
	if msg.Err != nil {
		text := msg.Err.Error()
		c := ClassifyFor(p.kind.harness(), text, 0)
		return []Event{{Type: EventError, Error: text, Code: c.Code, EndReason: string(c.Class)}}
	}
	var res struct {
		StopReason string          `json:"stopReason"`
		Usage      json.RawMessage `json:"usage"`
	}
	_ = json.Unmarshal(msg.Result, &res)
	var out []Event
	if u := acpUsageRaw(res.Usage); u != nil {
		out = append(out, Event{Type: EventUsage, Usage: u})
	}
	switch res.StopReason {
	case "", "end_turn":
		out = append(out, Event{Type: EventResult, Result: &Result{Subtype: "success"}})
	case "cancelled":
		out = append(out, Event{Type: EventResult, Result: &Result{Subtype: "cancelled", IsError: true, Text: "turn cancelled"}})
	default:
		c := ClassifyFor(p.kind.harness(), res.StopReason, 0)
		out = append(out, Event{Type: EventResult, Result: &Result{
			Subtype: res.StopReason, IsError: true, Text: "stop: " + res.StopReason,
			EndReason: string(c.Class), Code: c.Code,
		}})
	}
	return out
}

// handleAgentRequest answers a peer request: a permission prompt, a file
// system call when the consumer supplied a FileSystem, or an explicit refusal.
func (p *acpProtocol) handleAgentRequest(msg jsonrpc.Message) []Event {
	switch msg.Method {
	case "session/request_permission":
		return p.permissionRequest(msg)
	case "fs/read_text_file":
		return p.handleReadTextFile(msg)
	case "fs/write_text_file":
		return p.handleWriteTextFile(msg)
	}
	if err := p.client.ReplyError(msg.ID, -32601, "agentwire: client method unsupported: "+msg.Method); err != nil {
		return []Event{{Type: EventError, Error: err.Error(), Code: "rpc_error", EndReason: string(FailProtocol)}}
	}
	return nil
}

// permissionRequest maps session/request_permission, unless the policy answers
// it here.
func (p *acpProtocol) permissionRequest(msg jsonrpc.Message) []Event {
	var params struct {
		ToolCall map[string]any   `json:"toolCall"`
		Options  []map[string]any `json:"options"`
	}
	_ = json.Unmarshal(msg.Params, &params)
	var options []acpOption
	for _, o := range params.Options {
		options = append(options, acpOption{ID: str(o["optionId"]), Kind: str(o["kind"])})
	}
	id := string(msg.ID.Raw())
	kind := classifyToolName(firstNonEmpty(str(params.ToolCall["kind"]), str(params.ToolCall["title"])))
	if d, ok := p.policy.Answer(kind, firstNonEmpty(str(params.ToolCall["title"]), str(params.ToolCall["kind"]))); ok {
		outcome := map[string]any{"outcome": "cancelled"}
		if option := pickACPOption(options, d.Allow); option != "" {
			outcome = map[string]any{"outcome": "selected", "optionId": option}
		}
		if err := p.client.Reply(msg.ID, map[string]any{"outcome": outcome}); err != nil {
			return []Event{{Type: EventError, Error: err.Error(), Code: "rpc_error", EndReason: string(FailProtocol)}}
		}
		return nil
	}
	p.mu.Lock()
	p.approvals[id] = options
	p.mu.Unlock()
	question := "Agent requests permission"
	if title := str(params.ToolCall["title"]); title != "" {
		question = "Allow " + title + "?"
	}
	return []Event{{Type: EventPermission, Permission: &Permission{
		ID: id, Tool: firstNonEmpty(str(params.ToolCall["title"]), str(params.ToolCall["kind"])),
		Kind: kind, Question: question,
	}}}
}

func (p *acpProtocol) handleReadTextFile(msg jsonrpc.Message) []Event {
	if p.fs == nil {
		if err := p.client.ReplyError(msg.ID, -32601, "agentwire: file system access is not enabled"); err != nil {
			return []Event{{Type: EventError, Error: err.Error(), Code: "rpc_error", EndReason: string(FailProtocol)}}
		}
		return nil
	}
	var params struct {
		Path string `json:"path"`
	}
	_ = json.Unmarshal(msg.Params, &params)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	content, err := p.fs.ReadTextFile(ctx, params.Path)
	if err != nil {
		if replyErr := p.client.ReplyError(msg.ID, -32603, err.Error()); replyErr != nil {
			return []Event{{Type: EventError, Error: replyErr.Error(), Code: "rpc_error", EndReason: string(FailProtocol)}}
		}
		return nil
	}
	if err := p.client.Reply(msg.ID, map[string]any{"content": content}); err != nil {
		return []Event{{Type: EventError, Error: err.Error(), Code: "rpc_error", EndReason: string(FailProtocol)}}
	}
	return nil
}

func (p *acpProtocol) handleWriteTextFile(msg jsonrpc.Message) []Event {
	if p.fs == nil {
		if err := p.client.ReplyError(msg.ID, -32601, "agentwire: file system access is not enabled"); err != nil {
			return []Event{{Type: EventError, Error: err.Error(), Code: "rpc_error", EndReason: string(FailProtocol)}}
		}
		return nil
	}
	var params struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	_ = json.Unmarshal(msg.Params, &params)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := p.fs.WriteTextFile(ctx, params.Path, params.Content); err != nil {
		if replyErr := p.client.ReplyError(msg.ID, -32603, err.Error()); replyErr != nil {
			return []Event{{Type: EventError, Error: replyErr.Error(), Code: "rpc_error", EndReason: string(FailProtocol)}}
		}
		return nil
	}
	if err := p.client.Reply(msg.ID, map[string]any{}); err != nil {
		return []Event{{Type: EventError, Error: err.Error(), Code: "rpc_error", EndReason: string(FailProtocol)}}
	}
	return nil
}

func (p *acpProtocol) currentSession() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sessionID
}

// Exit classifies a process exit without a prompt response.
func (p *acpProtocol) Exit(code int, err error, stderr string) []Event {
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
			text = fmt.Sprintf("%s exited with code %d before the turn ended", p.kind, code)
		}
	}
	c := ClassifyFor(p.kind.harness(), text, 0)
	return []Event{{Type: EventError, Error: text, Code: c.Code, EndReason: string(c.Class)}}
}

// acpSessionID reads the sessionId out of a session-open response.
func acpSessionID(raw json.RawMessage) string {
	var res struct {
		SessionID string `json:"sessionId"`
	}
	if json.Unmarshal(raw, &res) != nil {
		return ""
	}
	return res.SessionID
}

// acpUsage maps a usage_update notification.
func acpUsage(update map[string]any) *Usage {
	u := &Usage{
		Input:   int64(num(update["used"])),
		Output:  int64(num(update["size"])),
		CostUSD: acpCost(update["cost"]),
	}
	if u.Input == 0 && u.Output == 0 && u.CostUSD == 0 {
		return nil
	}
	return u
}

// acpUsageRaw maps the usage object of a prompt response.
func acpUsageRaw(raw json.RawMessage) *Usage {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	u := &Usage{
		Input:         int64(num(m["inputTokens"]) + num(m["input_tokens"])),
		Output:        int64(num(m["outputTokens"]) + num(m["output_tokens"])),
		CacheRead:     int64(num(m["cachedReadTokens"]) + num(m["cached_read_tokens"])),
		CacheCreation: int64(num(m["cachedWriteTokens"]) + num(m["cached_write_tokens"])),
		CostUSD:       acpCost(m["cost"]),
	}
	if u.Input == 0 && u.Output == 0 && u.CacheRead == 0 && u.CacheCreation == 0 && u.CostUSD == 0 {
		return nil
	}
	return u
}

func acpCost(v any) float64 {
	switch t := v.(type) {
	case map[string]any:
		return num(t["amount"])
	default:
		return num(v)
	}
}

// ensure the driver and the protocol satisfy their interfaces.
var (
	_ Driver       = acpDriver{}
	_ wire.Adapter = (*acpProtocol)(nil)
)
