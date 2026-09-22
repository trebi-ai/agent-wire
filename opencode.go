package agentwire

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// openCodeDriver manages sessions on a runtime-owned `opencode serve`.
type openCodeDriver struct{ rt *Runtime }

func (openCodeDriver) Name() string { return string(OpenCode) }

func (d openCodeDriver) Start(ctx context.Context, req StartRequest) (Session, error) {
	return d.start(ctx, req, false)
}

func (d openCodeDriver) Resume(ctx context.Context, req StartRequest) (Session, error) {
	return d.start(ctx, req, true)
}

func (d openCodeDriver) start(ctx context.Context, req StartRequest, resume bool) (Session, error) {
	l, err := d.rt.prepare(ctx, &req, resume)
	if err != nil {
		return nil, err
	}
	srv, err := d.rt.oc.acquireServer(ctx, l.bin, l.env, nil)
	if err != nil {
		return nil, err
	}
	dir := req.WorkingDir
	sessionID := req.SessionID
	if sessionID != "" {
		var info map[string]any
		if err := srv.call(ctx, "GET", "/session/"+sessionID, dir, nil, &info); err != nil {
			d.rt.oc.release(srv)
			return nil, fmt.Errorf("%w: opencode session %s: %v", ErrResumeUnsupported, sessionID, err)
		}
	} else {
		var res struct {
			ID string `json:"id"`
		}
		if err := srv.call(ctx, "POST", "/session", dir, map[string]any{}, &res); err != nil {
			d.rt.oc.release(srv)
			return nil, fmt.Errorf("opencode session create: %w", err)
		}
		sessionID = res.ID
	}
	if sessionID == "" {
		d.rt.oc.release(srv)
		return nil, errors.New("opencode: session has no id")
	}
	s := &openCodeSession{
		rt: d.rt, srv: srv, id: sessionID, directory: dir,
		instructions: l.instructions, model: req.Model, variant: req.Effort,
		policy: req.Permissions.Normalized(), first: true,
		events: make(chan Event, 256), done: make(chan struct{}),
		notify: make(chan struct{}, 1), pumpStop: make(chan struct{}),
		parts: map[string]int{}, roles: map[string]string{},
		partOwner: map[string]string{},
	}
	srv.subscribe(s)
	srv.ensureStream()
	go s.pump()
	d.rt.bindSession(s, l)
	return s, nil
}

// openCodeSession is one OpenCode conversation over HTTP plus the shared event
// stream of its server.
type openCodeSession struct {
	rt        *Runtime
	srv       *openCodeServer
	id        string
	directory string

	instructions string
	model        string
	variant      string
	policy       PermissionPolicy
	first        bool

	events   chan Event
	done     chan struct{}
	notify   chan struct{}
	pumpStop chan struct{}

	queueMu     sync.Mutex
	queue       []Event
	queueClosed bool
	finishOnce  sync.Once
	releaseOnce sync.Once
	sawResult   bool
	lastError   string
	parts       map[string]int
	roles       map[string]string
	partOwner   map[string]string
	onClose     []func()
}

func (s *openCodeSession) Provider() string { return string(OpenCode) }
func (s *openCodeSession) ID() string       { return s.id }
func (s *openCodeSession) PID() int         { return s.srv.proc.PID() }
func (s *openCodeSession) Pgid() int        { return s.srv.proc.Pgid() }

// Argv is the serve command of the owning server (ledger evidence).
func (s *openCodeSession) Argv() []string { return s.srv.argv }

func (s *openCodeSession) Events() <-chan Event  { return s.events }
func (s *openCodeSession) Done() <-chan struct{} { return s.done }

func (s *openCodeSession) Err() error {
	s.queueMu.Lock()
	defer s.queueMu.Unlock()
	if s.lastError == "" {
		return nil
	}
	return errors.New(s.lastError)
}

// deliver queues events for the session pump. It never blocks the shared event
// stream, whatever the consumer is doing.
func (s *openCodeSession) deliver(evs []Event) {
	if len(evs) == 0 {
		return
	}
	s.queueMu.Lock()
	if s.queueClosed {
		s.queueMu.Unlock()
		return
	}
	s.queue = append(s.queue, evs...)
	s.queueMu.Unlock()
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

// pump moves queued events to the consumer channel.
func (s *openCodeSession) pump() {
	defer close(s.done)
	defer close(s.events)
	for {
		s.queueMu.Lock()
		batch := s.queue
		s.queue = nil
		s.queueMu.Unlock()
		for _, ev := range batch {
			if s.send(ev) {
				return
			}
		}
		select {
		case <-s.notify:
		case <-s.pumpStop:
			s.flush()
			return
		}
	}
}

// send writes one event and reports whether the pump must stop.
func (s *openCodeSession) send(ev Event) bool {
	select {
	case s.events <- ev:
		return false
	default:
	}
	select {
	case s.events <- ev:
		return false
	case <-s.pumpStop:
		return true
	}
}

// flush drops any remaining queue on a forced stop.
func (s *openCodeSession) flush() {
	s.queueMu.Lock()
	batch := s.queue
	s.queue = nil
	s.queueClosed = true
	s.queueMu.Unlock()
	for _, ev := range batch {
		select {
		case s.events <- ev:
		default:
			return
		}
	}
}

// finish queues the terminal exit event, ends the pump, drops the server
// refcount and runs the close hooks exactly once.
func (s *openCodeSession) finish(code *int, reason string) {
	s.finishOnce.Do(func() {
		ev := Event{Type: EventExit, Code: reason}
		if code != nil {
			ev.ExitCode = code
		}
		s.deliver([]Event{ev})
		close(s.pumpStop)
		s.releaseServer()
		s.runOnClose()
	})
}

// OnClose registers a cleanup hook that runs once when the session ends.
func (s *openCodeSession) OnClose(fn func()) {
	if fn == nil {
		return
	}
	s.queueMu.Lock()
	s.onClose = append(s.onClose, fn)
	s.queueMu.Unlock()
}

func (s *openCodeSession) runOnClose() {
	s.queueMu.Lock()
	hooks := s.onClose
	s.onClose = nil
	s.queueMu.Unlock()
	for _, fn := range hooks {
		fn()
	}
}

// releaseServer drops the shared server refcount exactly once.
func (s *openCodeSession) releaseServer() {
	s.releaseOnce.Do(func() {
		s.srv.unsubscribe(s.id)
		s.rt.oc.release(s.srv)
	})
}

// serverGone ends the session because its server disappeared.
func (s *openCodeSession) serverGone(reason string) {
	s.queueMu.Lock()
	saw := s.sawResult
	s.lastError = firstNonEmpty(s.lastError, reason)
	s.queueMu.Unlock()
	if !saw {
		c := ClassifyFor(OpenCode, reason, 0)
		s.deliver([]Event{{Type: EventError, Error: reason, Code: c.Code, EndReason: string(c.Class)}})
	}
	code := -1
	s.finish(&code, "server_exited")
}

// Prompt starts a turn without waiting: prompt_async answers 204 and the turn
// arrives on the event stream.
func (s *openCodeSession) Prompt(ctx context.Context, p Prompt) error {
	s.queueMu.Lock()
	s.sawResult = false
	s.lastError = ""
	s.parts = map[string]int{}
	s.queueMu.Unlock()
	body := map[string]any{}
	parts, err := s.promptParts(p)
	if err != nil {
		return err
	}
	body["parts"] = parts
	if s.model != "" {
		body["model"] = openCodeModel(s.model)
	}
	if s.variant != "" {
		body["variant"] = s.variant
	}
	s.queueMu.Lock()
	first := s.first
	s.first = false
	instructions := s.instructions
	s.queueMu.Unlock()
	if first && strings.TrimSpace(instructions) != "" {
		body["system"] = instructions
	}
	return s.srv.call(ctx, "POST", "/session/"+s.id+"/prompt_async", s.directory, body, nil)
}

// promptParts renders the prompt body: text plus file parts for attachments.
func (s *openCodeSession) promptParts(p Prompt) ([]map[string]any, error) {
	parts := make([]map[string]any, 0, len(p.Attachments)+1)
	for _, a := range sortedAttachments(p.Attachments) {
		data, media, err := loadAttachment(a)
		if err != nil {
			return nil, err
		}
		part := map[string]any{"type": "file", "mime": media}
		if a.Path != "" {
			part["filename"] = a.Path
			part["url"] = "file://" + a.Path
		} else {
			part["url"] = "data:" + media + ";base64," + encodeBase64(data)
		}
		parts = append(parts, part)
	}
	parts = append(parts, map[string]any{"type": "text", "text": p.Text})
	return parts, nil
}

// openCodeModel splits a "provider/model" pin into the shape prompt_async
// takes.
func openCodeModel(model string) map[string]any {
	if provider, id, ok := strings.Cut(model, "/"); ok {
		return map[string]any{"providerID": provider, "modelID": id}
	}
	return map[string]any{"modelID": model}
}

// AnswerPermission posts the decision (allow to once, deny to reject).
func (s *openCodeSession) AnswerPermission(ctx context.Context, permissionID string, d Decision) error {
	response := "once"
	if !d.Allow {
		response = "reject"
	}
	return s.srv.call(ctx, "POST", "/session/"+s.id+"/permissions/"+permissionID, s.directory,
		map[string]any{"response": response}, nil)
}

// Interrupt aborts the current turn.
func (s *openCodeSession) Interrupt(ctx context.Context) error {
	return s.srv.call(ctx, "POST", "/session/"+s.id+"/abort", s.directory, map[string]any{}, nil)
}

// InterruptTurn is the protocol interrupt.
func (s *openCodeSession) InterruptTurn(ctx context.Context) error { return s.Interrupt(ctx) }

// Close aborts, unsubscribes and releases the shared server refcount.
func (s *openCodeSession) Close(ctx context.Context) error {
	abortCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = s.Interrupt(abortCtx)
	cancel()
	s.finish(nil, "closed")
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-s.done:
	case <-ctx.Done():
	}
	return nil
}

// parse maps one OpenCode bus event to normalized events.
func (s *openCodeSession) parse(typ string, props map[string]any) []Event {
	switch typ {
	case "session.created", "session.updated":
		if info, _ := props["info"].(map[string]any); info != nil && str(info["id"]) == s.id {
			return []Event{{Type: EventInit, SessionID: s.id}}
		}
	case "session.idle":
		s.queueMu.Lock()
		s.sawResult = true
		lastErr := s.lastError
		s.queueMu.Unlock()
		if lastErr != "" {
			c := ClassifyFor(OpenCode, lastErr, 0)
			return []Event{{Type: EventResult, Result: &Result{
				Subtype: "error_during_execution", IsError: true, Text: lastErr,
				EndReason: string(c.Class), Code: c.Code,
			}}}
		}
		return []Event{{Type: EventResult, Result: &Result{Subtype: "success", SessionID: s.id}}}
	case "session.error":
		text := openCodeErrorText(props)
		if text == "" {
			text = "opencode session error"
		}
		s.queueMu.Lock()
		s.sawResult = true
		s.lastError = text
		s.queueMu.Unlock()
		c := ClassifyFor(OpenCode, text, 0)
		return []Event{{Type: EventError, Error: text, Code: c.Code, EndReason: string(c.Class)}}
	case "session.deleted":
		// The session is gone on the server. The consumer still gets a code
		// and a reason instead of a bare channel close.
		code := 0
		s.finish(&code, "session_deleted")
		return nil
	case "message.updated":
		return s.parseMessageUpdated(props)
	case "message.part.updated":
		part, _ := props["part"].(map[string]any)
		if part == nil {
			return nil
		}
		return s.parsePart(part)
	case "permission.updated", "permission.asked", "permission.v2.asked":
		id := str(props["id"])
		kind := classifyPermissionKind(str(props["type"]), "")
		if d, ok := s.policy.Answer(kind, str(props["type"])); ok {
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_ = s.AnswerPermission(ctx, id, d)
			}()
			return nil
		}
		q := firstNonEmpty(str(props["title"]), str(props["message"]), "OpenCode requests permission")
		return []Event{{Type: EventPermission, Permission: &Permission{
			ID: id, Tool: str(props["type"]), Kind: kind, Question: q,
		}}}
	}
	return nil
}

// parseMessageUpdated records the role of a message and reports usage or a
// message-level error.
func (s *openCodeSession) parseMessageUpdated(props map[string]any) []Event {
	info, _ := props["info"].(map[string]any)
	if info == nil {
		return nil
	}
	if id := str(info["id"]); id != "" {
		s.queueMu.Lock()
		s.roles[id] = str(info["role"])
		s.queueMu.Unlock()
	}
	if errText := openCodeErrorText(info); errText != "" {
		s.queueMu.Lock()
		s.lastError = errText
		s.queueMu.Unlock()
		return []Event{{Type: EventError, Error: errText}}
	}
	if num(info["cost"]) > 0 {
		if u := openCodeUsage(info); u != nil {
			return []Event{{Type: EventUsage, Usage: u}}
		}
	}
	return nil
}

// parsePart maps one message part. A part of a user message is dropped: the
// bus echoes the prompt back, and a caller must not read its own text as
// assistant output.
func (s *openCodeSession) parsePart(part map[string]any) []Event {
	partID := str(part["id"])
	messageID := str(part["messageID"])
	s.queueMu.Lock()
	if messageID != "" {
		if owner, ok := s.partOwner[partID]; !ok {
			s.partOwner[partID] = messageID
		} else {
			messageID = owner
		}
	}
	role := s.roles[messageID]
	s.queueMu.Unlock()
	if role == "user" {
		return nil
	}
	switch str(part["type"]) {
	case "text":
		return s.parseTextPart(part, partID)
	case "tool":
		return s.parseToolPart(part, partID)
	case "reasoning":
		return []Event{{Type: EventStatus, Status: StatusRunning}}
	}
	return nil
}

func (s *openCodeSession) parseTextPart(part map[string]any, id string) []Event {
	text := str(part["text"])
	if text == "" {
		return nil
	}
	s.queueMu.Lock()
	prev := s.parts[id]
	if len(text) > prev {
		s.parts[id] = len(text)
	}
	s.queueMu.Unlock()
	if prev > len(text) {
		// A replaced block: emit the whole text once.
		return []Event{{Type: EventAssistant, Text: text}}
	}
	if prev == 0 {
		return []Event{{Type: EventAssistant, Text: text}}
	}
	return []Event{{Type: EventAssistant, Text: text[prev:], Delta: true}}
}

func (s *openCodeSession) parseToolPart(part map[string]any, id string) []Event {
	state, _ := part["state"].(map[string]any)
	name := str(part["tool"])
	status := ""
	input, output := "", ""
	if state != nil {
		status = str(state["status"])
		if raw, ok := state["input"]; ok {
			if b, err := json.Marshal(raw); err == nil {
				input = string(b)
			}
		}
		output = str(state["output"])
	}
	switch status {
	case "", "pending", "running":
		status = "started"
	case "completed":
		status = "completed"
	case "error":
		status = "failed"
		output = firstNonEmpty(output, openCodeErrorText(state))
	}
	kind := classifyToolName(name)
	return []Event{{Type: EventTool, Tool: &ToolEvent{
		ID: id, Name: name, Kind: kind, Input: input, Output: output,
		Paths: toolPaths(kind, input), Status: status,
	}}}
}

// openCodeSessionID finds the session a bus event belongs to.
func openCodeSessionID(props map[string]any) string {
	if id := str(props["sessionID"]); id != "" {
		return id
	}
	if id := str(props["sessionId"]); id != "" {
		return id
	}
	if info, _ := props["info"].(map[string]any); info != nil {
		if id := str(info["sessionID"]); id != "" {
			return id
		}
	}
	if part, _ := props["part"].(map[string]any); part != nil {
		if id := str(part["sessionID"]); id != "" {
			return id
		}
	}
	return ""
}

// openCodeErrorText reads the message out of an OpenCode error payload.
func openCodeErrorText(v map[string]any) string {
	if err, _ := v["error"].(map[string]any); err != nil {
		if data, _ := err["data"].(map[string]any); data != nil {
			if msg := str(data["message"]); msg != "" {
				return msg
			}
		}
		if msg := firstNonEmpty(str(err["message"]), str(err["name"])); msg != "" {
			return msg
		}
	}
	return str(v["error"])
}

func openCodeUsage(info map[string]any) *Usage {
	tokens, _ := info["tokens"].(map[string]any)
	if tokens == nil {
		return nil
	}
	u := &Usage{
		Input:     int64(num(tokens["input"])),
		Output:    int64(num(tokens["output"])),
		CacheRead: int64(num(tokens["cache"]) + num(tokens["cache_read"])),
		CostUSD:   num(info["cost"]),
		Model:     str(info["modelID"]),
	}
	if u.Input == 0 && u.Output == 0 && u.CacheRead == 0 && u.CostUSD == 0 {
		return nil
	}
	return u
}

// OpenCodeMessages returns the persisted message list for one session from a
// live server. ok=false means no server currently holds the session.
func (rt *Runtime) OpenCodeMessages(ctx context.Context, sessionID string) ([]byte, bool) {
	if sessionID == "" {
		return nil, false
	}
	rt.oc.mu.Lock()
	servers := make([]*openCodeServer, 0, len(rt.oc.servers))
	for _, srv := range rt.oc.servers {
		servers = append(servers, srv)
	}
	rt.oc.mu.Unlock()
	for _, srv := range servers {
		sess := srv.lookup(sessionID)
		if sess == nil {
			continue
		}
		var raw json.RawMessage
		if err := srv.call(ctx, "GET", "/session/"+sessionID+"/message", sess.directory, nil, &raw); err != nil {
			return nil, false
		}
		return raw, len(raw) > 0
	}
	return nil, false
}

// ensure the openCodeSession satisfies the Session interface.
var _ Session = (*openCodeSession)(nil)

// ensure the driver satisfies the Driver interface.
var _ Driver = openCodeDriver{}
