package agentwire

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// openCode2 is the OpenCode 2.x HTTP surface: every route lives under /api, the
// event envelope carries its payload under `data`, one stream serves every
// location, and a turn ends with session.execution.succeeded instead of
// session.idle.
type openCode2 struct{}

func (openCode2) harness() Harness   { return OpenCode2 }
func (openCode2) prefix() string     { return "/api" }
func (openCode2) healthPath() string { return "/api/health" }
func (openCode2) eventPath() string  { return "/api/event" }

// eventScoped is false: V2 streams every location on one connection.
func (openCode2) eventScoped() bool { return false }

// frame reads one V2 event: {id, type, data}.
func (openCode2) frame(raw []byte) (string, map[string]any, string, bool) {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return "", nil, "", false
	}
	typ := str(m["type"])
	if typ == "" {
		return "", nil, "", false
	}
	data, _ := m["data"].(map[string]any)
	if data == nil {
		data = map[string]any{}
	}
	return typ, data, openCode2SessionID(data), true
}

// decode unwraps the V2 response envelope {data: …}.
func (openCode2) decode(body []byte, out any) error {
	if out == nil {
		return nil
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return err
	}
	if len(env.Data) == 0 {
		// A route without an envelope answers with the payload itself.
		return json.Unmarshal(body, out)
	}
	return json.Unmarshal(env.Data, out)
}

// errorText reads a V2 error body: {_tag, message, kind}.
func (openCode2) errorText(body []byte, status int) string {
	var e struct {
		Tag     string `json:"_tag"`
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	return firstNonEmpty(e.Message, e.Error, e.Tag, fmt.Sprintf("HTTP %d", status))
}

// openCode2SessionID finds the session a V2 event belongs to. V2 names it in
// the payload; the shell events carry it one level deeper.
func openCode2SessionID(data map[string]any) string {
	if id := str(data["sessionID"]); id != "" {
		return id
	}
	if info, _ := data["info"].(map[string]any); info != nil {
		if meta, _ := info["metadata"].(map[string]any); meta != nil {
			return str(meta["sessionID"])
		}
	}
	return ""
}

// openCode2Driver manages sessions on a runtime-owned `opencode2 serve`.
type openCode2Driver struct{ rt *Runtime }

func (openCode2Driver) Name() string { return string(OpenCode2) }

func (d openCode2Driver) Start(ctx context.Context, req StartRequest) (Session, error) {
	return d.start(ctx, req, false)
}

func (d openCode2Driver) Resume(ctx context.Context, req StartRequest) (Session, error) {
	return d.start(ctx, req, true)
}

func (d openCode2Driver) start(ctx context.Context, req StartRequest, resume bool) (Session, error) {
	l, err := d.rt.prepare(ctx, &req, resume)
	if err != nil {
		return nil, err
	}
	srv, err := d.rt.oc.acquireServer(ctx, openCode2{}, l.bin, l.env, nil)
	if err != nil {
		return nil, err
	}
	dir := req.WorkingDir
	// V2 streams every location on one connection, so the stream is opened once
	// per server, before the session exists. Events published before a stream
	// is connected are not replayed.
	srv.ensureStreamReady(ctx, dir)
	sessionID := req.SessionID
	if sessionID != "" {
		var info map[string]any
		if err := srv.call(ctx, "GET", "/session/"+sessionID, dir, nil, &info); err != nil {
			d.rt.oc.release(srv)
			return nil, fmt.Errorf("%w: opencode2 session %s: %v", ErrResumeUnsupported, sessionID, err)
		}
	} else {
		body := map[string]any{"location": map[string]any{"directory": dir}}
		model := req.Model
		if model == "" {
			model = openCode2DefaultModel()
		}
		if ref := openCode2ModelRef(model, req.Effort); ref != nil {
			body["model"] = ref
		}
		var res struct {
			ID string `json:"id"`
		}
		if err := srv.call(ctx, "POST", "/session", dir, body, &res); err != nil {
			d.rt.oc.release(srv)
			return nil, fmt.Errorf("opencode2 session create: %w", err)
		}
		sessionID = res.ID
	}
	if sessionID == "" {
		d.rt.oc.release(srv)
		return nil, errors.New("opencode2: session has no id")
	}
	s := &openCode2Session{
		openCodeBase: newOpenCodeBase(d.rt, srv, sessionID, dir, openCode2{}),
		instructions: l.instructions,
		policy:       req.Permissions.Normalized(),
		text:         map[string]int{},
		tools:        map[string]string{},
	}
	s.interrupt = s.Interrupt
	srv.subscribe(s)
	s.markLive()
	go s.pump()
	d.rt.bindSession(s, l)
	// The session instructions are a per-session entry on V2, so they are sent
	// once the session exists. A resume re-sends them, which is idempotent.
	if err := s.setInstructions(ctx); err != nil {
		_ = s.Close(ctx)
		return nil, err
	}
	if resume && req.Model != "" {
		if err := s.setModel(ctx, req.Model, req.Effort); err != nil {
			_ = s.Close(ctx)
			return nil, err
		}
	}
	return s, nil
}

// openCode2Session is one OpenCode 2.x conversation over HTTP plus the shared
// event stream of its server.
type openCode2Session struct {
	*openCodeBase

	instructions string
	policy       PermissionPolicy

	// text records how much of each (assistant message, block) pair the caller
	// has already received, so the ended event can be reconciled against it.
	text map[string]int
	// tools remembers the name of each tool call. V2 sends the name once, when
	// the call starts.
	tools map[string]string
}

// Prompt starts a turn without waiting: the prompt route answers with the
// queued message and the turn arrives on the event stream.
func (s *openCode2Session) Prompt(ctx context.Context, p Prompt) error {
	s.queueMu.Lock()
	s.sawResult = false
	s.lastError = ""
	s.text = map[string]int{}
	s.tools = map[string]string{}
	s.queueMu.Unlock()
	body := map[string]any{
		"text":     p.Text,
		"location": map[string]any{"directory": s.dir},
	}
	files, err := s.promptFiles(p)
	if err != nil {
		return err
	}
	if len(files) > 0 {
		body["files"] = files
	}
	return s.srv.call(ctx, "POST", "/session/"+s.id+"/prompt", s.dir, body, nil)
}

// promptFiles renders the attachments of one prompt as V2 file inputs.
func (s *openCode2Session) promptFiles(p Prompt) ([]map[string]any, error) {
	files := make([]map[string]any, 0, len(p.Attachments))
	for _, a := range sortedAttachments(p.Attachments) {
		data, media, err := loadAttachment(a)
		if err != nil {
			return nil, err
		}
		file := map[string]any{}
		if a.Path != "" {
			file["uri"] = "file://" + a.Path
			file["name"] = filepath.Base(a.Path)
		} else {
			file["uri"] = "data:" + media + ";base64," + encodeBase64(data)
		}
		files = append(files, file)
	}
	return files, nil
}

// setInstructions writes the session instructions as a per-session entry.
func (s *openCode2Session) setInstructions(ctx context.Context) error {
	text := strings.TrimSpace(s.instructions)
	if text == "" {
		return nil
	}
	return s.srv.call(ctx, "PUT", "/session/"+s.id+"/instructions/entries/"+openCode2InstructionKey, s.dir,
		map[string]any{"value": text}, nil)
}

// setModel pins the model on an existing session.
func (s *openCode2Session) setModel(ctx context.Context, model, variant string) error {
	ref := openCode2ModelRef(model, variant)
	if ref == nil {
		return nil
	}
	return s.srv.call(ctx, "POST", "/session/"+s.id+"/model", s.dir, map[string]any{"model": ref}, nil)
}

// AnswerPermission posts the decision (allow to once, deny to reject).
func (s *openCode2Session) AnswerPermission(ctx context.Context, permissionID string, d Decision) error {
	reply := "once"
	if !d.Allow {
		reply = "reject"
	}
	return s.srv.call(ctx, "POST", "/session/"+s.id+"/permission/"+permissionID+"/reply", s.dir,
		map[string]any{"reply": reply}, nil)
}

// Interrupt ends the current turn.
func (s *openCode2Session) Interrupt(ctx context.Context) error {
	return s.srv.call(ctx, "POST", "/session/"+s.id+"/interrupt", s.dir, nil, nil)
}

// InterruptTurn is the protocol interrupt.
func (s *openCode2Session) InterruptTurn(ctx context.Context) error { return s.Interrupt(ctx) }

// openCode2InstructionKey is the per-session instruction entry this library
// owns. V2 keys are lowercase alphanumerics plus . _ -.
const openCode2InstructionKey = "agentwire.instructions"

// openCode2DefaultModel is the model the operator's own CLI would use when the
// caller pins none. V2 needs it on session create: a session created without a
// model falls back to a provider default that can point at an unusable
// credential (observed 2026-09-22: an expired Anthropic OAuth token answered
// every prompt with "OAuth access token is invalid" while the CLI, which
// resolves this config, ran the same prompt). The file is the operator's
// global config, not the trebi home: V2 reads the standard directories.
func openCode2DefaultModel() string {
	data, err := os.ReadFile(openCode2ConfigPath())
	if err != nil {
		return ""
	}
	var cfg struct {
		Model json.RawMessage `json:"model"`
	}
	if json.Unmarshal(data, &cfg) != nil || len(cfg.Model) == 0 {
		return ""
	}
	// 1.x stores a "provider/model" string, 2.x stores {providerID, model}.
	var pinned string
	if json.Unmarshal(cfg.Model, &pinned) == nil {
		return strings.TrimSpace(pinned)
	}
	var ref struct {
		ProviderID string `json:"providerID"`
		Model      string `json:"model"`
	}
	if json.Unmarshal(cfg.Model, &ref) != nil || ref.Model == "" {
		return ""
	}
	if ref.ProviderID == "" {
		return ref.Model
	}
	return ref.ProviderID + "/" + ref.Model
}

// openCode2ConfigPath is the global config file V2 reads: $XDG_CONFIG_HOME
// first, then ~/.config/opencode/opencode.json.
func openCode2ConfigPath() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "opencode", "opencode.json")
}

// openCode2ModelRef splits a "provider/model" pin into the shape V2 takes.
func openCode2ModelRef(model, variant string) map[string]any {
	if strings.TrimSpace(model) == "" {
		return nil
	}
	ref := map[string]any{}
	if provider, id, ok := strings.Cut(model, "/"); ok {
		ref["providerID"], ref["id"] = provider, id
	} else {
		ref["id"] = model
	}
	if variant != "" {
		ref["variant"] = variant
	}
	return ref
}

// parse maps one V2 event to normalized events.
func (s *openCode2Session) parse(typ string, data map[string]any) []Event {
	switch typ {
	case "session.execution.started", "session.step.started":
		return []Event{{Type: EventStatus, Status: StatusRunning}}
	case "session.text.delta":
		delta := str(data["delta"])
		if delta == "" {
			return nil
		}
		s.noteText(data, len(delta))
		return []Event{{Type: EventAssistant, Text: delta, Delta: true}}
	case "session.text.ended":
		return s.parseTextEnded(data)
	case "session.tool.input.started":
		id, name := str(data["id"]), str(data["name"])
		if id == "" {
			return nil
		}
		s.queueMu.Lock()
		s.tools[id] = name
		s.queueMu.Unlock()
		return []Event{{Type: EventTool, Tool: s.toolEvent(id, name, "", "", "started")}}
	case "session.tool.input.ended":
		id := str(data["id"])
		if id == "" {
			return nil
		}
		name := s.toolName(id)
		return []Event{{Type: EventTool, Tool: s.toolEvent(id, name, str(data["text"]), "", "started")}}
	case "session.tool.called":
		id := str(data["id"])
		if id == "" {
			return nil
		}
		name := s.toolName(id)
		return []Event{{Type: EventTool, Tool: s.toolEvent(id, name, openCode2JSON(data["input"]), "", "started")}}
	case "session.tool.success":
		id := str(data["id"])
		if id == "" {
			return nil
		}
		name := s.toolName(id)
		return []Event{{Type: EventTool, Tool: s.toolEvent(id, name, "", openCode2ToolOutput(data), "completed")}}
	case "session.tool.failed":
		id := str(data["id"])
		if id == "" {
			return nil
		}
		name := s.toolName(id)
		text, _, _ := openCode2Error(data)
		return []Event{{Type: EventTool, Tool: s.toolEvent(id, name, "", firstNonEmpty(text, openCode2ToolOutput(data)), "failed")}}
	case "session.step.ended", "session.usage.updated":
		if u := openCode2Usage(data); u != nil {
			return []Event{{Type: EventUsage, Usage: u}}
		}
	case "session.execution.succeeded":
		s.queueMu.Lock()
		s.sawResult = true
		lastErr := s.lastError
		s.queueMu.Unlock()
		if lastErr != "" {
			c := ClassifyFor(OpenCode2, lastErr, 0)
			return []Event{{Type: EventResult, Result: &Result{
				Subtype: "error_during_execution", IsError: true, Text: lastErr,
				EndReason: string(c.Class), Code: c.Code,
			}}}
		}
		return []Event{{Type: EventResult, Result: &Result{Subtype: "success", SessionID: s.id}}}
	case "session.step.failed", "session.execution.failed":
		text, status, vendor := openCode2Error(data)
		if text == "" {
			text = "opencode2 session error"
		}
		c := openCode2Classify(text, status, vendor)
		s.queueMu.Lock()
		s.sawResult = true
		s.lastError = text
		s.queueMu.Unlock()
		if typ == "session.execution.failed" {
			return []Event{
				{Type: EventError, Error: text, Code: c.Code, EndReason: string(c.Class)},
				{Type: EventResult, Result: &Result{
					Subtype: "error_during_execution", IsError: true, Text: text,
					EndReason: string(c.Class), Code: c.Code,
				}},
			}
		}
		return []Event{{Type: EventError, Error: text, Code: c.Code, EndReason: string(c.Class)}}
	case "permission.asked":
		return s.parsePermission(data)
	}
	return nil
}

// parsePermission answers a permission request from the policy, or forwards it
// to the caller.
func (s *openCode2Session) parsePermission(data map[string]any) []Event {
	id := str(data["id"])
	if id == "" {
		return nil
	}
	action := str(data["action"])
	kind := classifyPermissionKind(action, "")
	if d, ok := s.policy.Answer(kind, action); ok {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = s.AnswerPermission(ctx, id, d)
		}()
		return nil
	}
	q := firstNonEmpty(str(data["message"]), openCode2PermissionQuestion(action, data))
	return []Event{{Type: EventPermission, Permission: &Permission{
		ID: id, Tool: action, Kind: kind, Question: q,
	}}}
}

// parseTextEnded reconciles the full text of a finished block against what the
// caller already received as deltas. V2 appends text at the end of a block that
// never streamed, for example a shell exit code. The block is recorded as fully
// delivered, so a repeated ended event sends nothing twice.
func (s *openCode2Session) parseTextEnded(data map[string]any) []Event {
	text := str(data["text"])
	if text == "" {
		return nil
	}
	key := openCode2TextKey(data)
	s.queueMu.Lock()
	seen := s.text[key]
	s.text[key] = len(text)
	s.queueMu.Unlock()
	switch {
	case seen == len(text):
		return nil
	case seen == 0 || seen > len(text):
		return []Event{{Type: EventAssistant, Text: text}}
	default:
		return []Event{{Type: EventAssistant, Text: text[seen:], Delta: true}}
	}
}

// noteText records streamed text against the block it belongs to.
func (s *openCode2Session) noteText(data map[string]any, n int) {
	key := openCode2TextKey(data)
	s.queueMu.Lock()
	s.text[key] += n
	s.queueMu.Unlock()
}

// toolName is the name of an in-flight tool call.
func (s *openCode2Session) toolName(id string) string {
	s.queueMu.Lock()
	defer s.queueMu.Unlock()
	return s.tools[id]
}

// toolEvent renders one normalized tool event for a V2 tool call.
func (s *openCode2Session) toolEvent(id, name, input, output, status string) *ToolEvent {
	kind := classifyToolName(name)
	return &ToolEvent{
		ID: id, Name: name, Kind: kind, Input: input, Output: output,
		Paths: toolPaths(kind, input), Status: status,
	}
}

// openCode2TextKey identifies one text block of one assistant message.
func openCode2TextKey(data map[string]any) string {
	return str(data["assistantMessageID"]) + "/" + str(data["ordinal"])
}

// openCode2Error reads the message, HTTP status and vendor type out of a V2
// error payload. V2 errors are structured:
// {type: "provider.auth", message: …, status: 401}.
func openCode2Error(data map[string]any) (string, int, string) {
	err, _ := data["error"].(map[string]any)
	if err == nil {
		return openCodeErrorText(data), 0, ""
	}
	text := firstNonEmpty(str(err["message"]), str(err["type"]), str(err["name"]))
	status := int(num(err["status"]))
	return text, status, str(err["type"])
}

// openCode2ErrorClasses maps the structured error types of the V2 wire onto the
// library's failure classes. V2 reports a machine type, so the class of a
// failure does not depend on substring matching. A type that is not listed
// falls back to the shared text and status rules.
var openCode2ErrorClasses = map[string]struct {
	class  FailureClass
	reason string
}{
	"provider.auth":                  {FailAuth, "invalid_credentials"},
	"provider.rate-limit":            {FailLimit, "rate_limited"},
	"provider.quota":                 {FailLimit, "credits_exhausted"},
	"provider.internal":              {FailOverloaded, "capacity"},
	"provider.transport":             {FailOverloaded, "capacity"},
	"provider.invalid-request":       {FailProtocol, "wire_drift"},
	"provider.invalid-output":        {FailProtocol, "wire_drift"},
	"provider.unsupported-operation": {FailProtocol, "wire_drift"},
}

// openCode2Classify prefers the vendor error type and falls back to the shared
// rules.
func openCode2Classify(text string, status int, typ string) Classification {
	if m, ok := openCode2ErrorClasses[typ]; ok {
		return Classification{Class: m.class, Code: codeFor(OpenCode2, m.reason), Message: strings.TrimSpace(text)}
	}
	return ClassifyFor(OpenCode2, text, status)
}

// openCode2ToolOutput joins the text of a tool result.
func openCode2ToolOutput(data map[string]any) string {
	items, _ := data["content"].([]any)
	if len(items) == 0 {
		return str(data["output"])
	}
	var b strings.Builder
	for _, item := range items {
		part, _ := item.(map[string]any)
		if part == nil {
			continue
		}
		if text := str(part["text"]); text != "" {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(text)
		}
	}
	return b.String()
}

// openCode2PermissionQuestion describes a permission request in the vendor's
// own words.
func openCode2PermissionQuestion(action string, data map[string]any) string {
	resources, _ := data["resources"].([]any)
	parts := make([]string, 0, len(resources))
	for _, r := range resources {
		if s := str(r); s != "" {
			parts = append(parts, s)
		}
	}
	if len(parts) == 0 {
		return firstNonEmpty(action, "OpenCode requests permission")
	}
	return firstNonEmpty(action, "OpenCode requests permission") + ": " + strings.Join(parts, ", ")
}

// openCode2JSON renders one decoded JSON value back to a string, for the tool
// event input.
func openCode2JSON(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// openCode2Usage reads usage out of a V2 step or usage payload.
func openCode2Usage(data map[string]any) *Usage {
	tokens, _ := data["tokens"].(map[string]any)
	if tokens == nil {
		return nil
	}
	cache, _ := tokens["cache"].(map[string]any)
	u := &Usage{
		Input:     int64(num(tokens["input"])),
		Output:    int64(num(tokens["output"])),
		CacheRead: int64(num(cache["read"]) + num(tokens["cache_read"])),
		CostUSD:   num(data["cost"]),
	}
	if model, _ := data["model"].(map[string]any); model != nil {
		u.Model = str(model["id"])
	}
	if u.Input == 0 && u.Output == 0 && u.CacheRead == 0 && u.CostUSD == 0 {
		return nil
	}
	return u
}

// ensure the openCode2Session satisfies the Session interface.
var _ Session = (*openCode2Session)(nil)

// ensure the driver satisfies the Driver interface.
var _ Driver = openCode2Driver{}

// ensure the V2 wire satisfies the wire interface.
var _ openCodeWire = openCode2{}

// ensure the session is routable on its server.
var _ openCodeRoute = (*openCode2Session)(nil)
