package agentwire

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/trebi-ai/agent-wire/internal/proc"
	"github.com/trebi-ai/agent-wire/internal/wire"
)

// openCodeWire is one version of the OpenCode HTTP surface. The session
// plumbing is shared between versions; the wire is not.
type openCodeWire interface {
	// harness is the harness this wire serves.
	harness() Harness
	// prefix is the API root path: "" on V1, "/api" on V2.
	prefix() string
	// healthPath is the readiness endpoint.
	healthPath() string
	// eventPath is the SSE endpoint.
	eventPath() string
	// eventScoped reports whether the event stream is scoped by session
	// directory. V1 publishes a directory's events only to a stream opened for
	// it; V2 publishes every location on one stream.
	eventScoped() bool
	// frame splits one SSE payload into the event type, the payload and the
	// session it belongs to.
	frame(raw []byte) (typ string, payload map[string]any, sessionID string, ok bool)
	// decode unwraps one JSON response body into out.
	decode(body []byte, out any) error
	// errorText reads the message out of a non-2xx response body.
	errorText(body []byte, status int) string
	// models lists the models the server offers for a directory.
	models(ctx context.Context, srv *openCodeServer, dir string) ([]ModelInfo, error)
}

// openCodeRoute is one session attached to a shared server.
type openCodeRoute interface {
	sessionID() string
	workDir() string
	deliver([]Event)
	parse(typ string, payload map[string]any) []Event
	serverGone(reason string)
}

// openCodeBase is the transport-agnostic half of an OpenCode session: the event
// queue, the pump, the terminal bookkeeping and the shared-server refcount. The
// version-specific half lives in the concrete session type.
type openCodeBase struct {
	rt      *Runtime
	srv     *openCodeServer
	id      string
	dir     string
	harness Harness
	// interrupt aborts the current turn on this wire.
	interrupt func(context.Context) error

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
	// busy is true from a prompt until the turn ends on the event stream.
	busy      bool
	live      bool
	lastError string
	onClose   []func()

	out wire.Outbox
}

func newOpenCodeBase(rt *Runtime, srv *openCodeServer, id, dir string, w openCodeWire) *openCodeBase {
	return &openCodeBase{
		rt: rt, srv: srv, id: id, dir: dir, harness: w.harness(),
		events: make(chan Event, 256), done: make(chan struct{}),
		notify: make(chan struct{}, 1), pumpStop: make(chan struct{}),
	}
}

func (b *openCodeBase) sessionID() string { return b.id }
func (b *openCodeBase) workDir() string   { return b.dir }

// markLive queues the init event for a session the server now knows about. It
// fires once per session.
//
// A wire whose bus does not repeat the session event cannot be the source of
// init: the event is published while the create is in flight, before the
// session is routable, so it is dropped. The driver reports the session it just
// opened instead.
func (b *openCodeBase) markLive() {
	b.queueMu.Lock()
	first := !b.live
	b.live = true
	b.queueMu.Unlock()
	if first {
		b.deliver([]Event{{Type: EventInit, SessionID: b.id}})
	}
}

func (b *openCodeBase) Provider() string { return string(b.harness) }
func (b *openCodeBase) ID() string       { return b.id }
func (b *openCodeBase) PID() int         { return b.srv.proc.PID() }
func (b *openCodeBase) Pgid() int        { return b.srv.proc.Pgid() }

// Argv is the serve command of the owning server (ledger evidence).
func (b *openCodeBase) Argv() []string { return b.srv.argv }

func (b *openCodeBase) Events() <-chan Event  { return b.events }
func (b *openCodeBase) Done() <-chan struct{} { return b.done }

func (b *openCodeBase) Err() error {
	b.queueMu.Lock()
	defer b.queueMu.Unlock()
	if b.lastError == "" {
		return nil
	}
	return errors.New(b.lastError)
}

// deliver queues events for the session pump. It never blocks the shared event
// stream, whatever the consumer is doing.
func (b *openCodeBase) deliver(evs []Event) {
	if len(evs) == 0 {
		return
	}
	b.queueMu.Lock()
	if b.queueClosed {
		b.queueMu.Unlock()
		return
	}
	b.queue = append(b.queue, evs...)
	b.queueMu.Unlock()
	select {
	case b.notify <- struct{}{}:
	default:
	}
}

// pump moves queued events to the consumer channel.
func (b *openCodeBase) pump() {
	defer close(b.done)
	defer close(b.events)
	for {
		b.queueMu.Lock()
		batch := b.queue
		b.queue = nil
		b.queueMu.Unlock()
		for _, ev := range batch {
			if b.send(ev) {
				return
			}
		}
		select {
		case <-b.notify:
		case <-b.pumpStop:
			b.flush()
			return
		}
	}
}

// send writes one event and reports whether the pump must stop.
func (b *openCodeBase) send(ev Event) bool {
	select {
	case b.events <- ev:
		return false
	default:
	}
	select {
	case b.events <- ev:
		return false
	case <-b.pumpStop:
		return true
	}
}

// flush drops any remaining queue on a forced stop.
func (b *openCodeBase) flush() {
	b.queueMu.Lock()
	batch := b.queue
	b.queue = nil
	b.queueClosed = true
	b.queueMu.Unlock()
	for _, ev := range batch {
		select {
		case b.events <- ev:
		default:
			return
		}
	}
}

// finish queues the terminal exit event, ends the pump, drops the server
// refcount and runs the close hooks exactly once.
func (b *openCodeBase) finish(code *int, reason string) {
	b.finishOnce.Do(func() {
		ev := Event{Type: EventExit, Code: reason}
		if code != nil {
			ev.ExitCode = code
		}
		if dropped := b.out.Drop(); len(dropped) > 0 {
			b.deliver([]Event{wire.DroppedEvent(dropped)})
		}
		b.deliver([]Event{ev})
		close(b.pumpStop)
		b.releaseServer()
		b.runOnClose()
	})
}

// OnClose registers a cleanup hook that runs once when the session ends.
func (b *openCodeBase) OnClose(fn func()) {
	if fn == nil {
		return
	}
	b.queueMu.Lock()
	b.onClose = append(b.onClose, fn)
	b.queueMu.Unlock()
}

func (b *openCodeBase) runOnClose() {
	b.queueMu.Lock()
	hooks := b.onClose
	b.onClose = nil
	b.queueMu.Unlock()
	for _, fn := range hooks {
		fn()
	}
}

// releaseServer drops the shared server refcount exactly once.
func (b *openCodeBase) releaseServer() {
	b.releaseOnce.Do(func() {
		b.srv.unsubscribe(b.id)
		b.rt.oc.release(b.srv)
	})
}

// beginTurn opens a turn. It returns ErrTurnActive while a turn runs, so a
// second prompt never resets the state of the running turn.
func (b *openCodeBase) beginTurn() error {
	b.queueMu.Lock()
	defer b.queueMu.Unlock()
	if b.busy {
		return ErrTurnActive
	}
	b.busy = true
	b.sawResult = false
	b.lastError = ""
	return nil
}

// turnBusy reports whether a turn runs.
func (b *openCodeBase) turnBusy() bool {
	b.queueMu.Lock()
	defer b.queueMu.Unlock()
	return b.busy
}

// sendWith delivers p with the shared Send rules. OpenCode has no steer, so a
// prompt during a turn waits for the turn to end.
func (b *openCodeBase) sendWith(ctx context.Context, prompt func(context.Context, Prompt) error, p Prompt) (Delivery, error) {
	return b.out.Send(ctx, wire.SendOps{Active: b.turnBusy, Prompt: prompt}, p)
}

// afterTurn sends the held prompts once the turn ended. It runs off the
// shared event stream, because a prompt is an HTTP call.
func (b *openCodeBase) afterTurn(prompt func(context.Context, Prompt) error) {
	if !b.out.Pending() {
		return
	}
	go func() {
		if dropped := b.out.Flush(context.Background(), prompt); len(dropped) > 0 {
			b.deliver([]Event{wire.DroppedEvent(dropped)})
		}
	}()
}

// endTurn closes the turn, on the terminal event or on a failed prompt call.
func (b *openCodeBase) endTurn() {
	b.queueMu.Lock()
	b.busy = false
	b.queueMu.Unlock()
}

// serverGone ends the session because its server disappeared.
func (b *openCodeBase) serverGone(reason string) {
	b.queueMu.Lock()
	saw := b.sawResult
	b.lastError = firstNonEmpty(b.lastError, reason)
	b.queueMu.Unlock()
	if !saw {
		c := ClassifyFor(b.harness, reason, 0)
		b.deliver([]Event{{Type: EventError, Error: reason, Code: c.Code, EndReason: string(c.Class)}})
	}
	code := -1
	b.finish(&code, "server_exited")
}

// Close aborts, unsubscribes and releases the shared server refcount.
func (b *openCodeBase) Close(ctx context.Context) error {
	if b.interrupt != nil {
		abortCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = b.interrupt(abortCtx)
		cancel()
	}
	b.finish(nil, "closed")
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-b.done:
	case <-ctx.Done():
	}
	return nil
}

// openCodeV1 is the OpenCode 1.x HTTP surface: no path prefix, one event stream
// per session directory, and an event envelope that carries its payload under
// `properties`.
type openCodeV1 struct{}

func (openCodeV1) harness() Harness   { return OpenCode }
func (openCodeV1) prefix() string     { return "" }
func (openCodeV1) healthPath() string { return "/global/health" }
func (openCodeV1) eventPath() string  { return "/event" }
func (openCodeV1) eventScoped() bool  { return true }

// frame reads one V1 bus frame: {type, properties}.
func (openCodeV1) frame(raw []byte) (string, map[string]any, string, bool) {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return "", nil, "", false
	}
	typ := str(m["type"])
	props, _ := m["properties"].(map[string]any)
	if typ == "" || props == nil {
		return "", nil, "", false
	}
	return typ, props, openCodeSessionID(props), true
}

// decode reads a V1 response body, which is the payload itself.
func (openCodeV1) decode(body []byte, out any) error {
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}

// errorText reads a V1 error body: {error, data:{message}}.
func (openCodeV1) errorText(body []byte, status int) string {
	var e struct {
		Error string `json:"error"`
		Data  struct {
			Message string `json:"message"`
		} `json:"data"`
	}
	_ = json.Unmarshal(body, &e)
	return firstNonEmpty(e.Error, e.Data.Message, fmt.Sprintf("HTTP %d", status))
}

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
	srv, err := d.rt.oc.acquireServer(ctx, openCodeV1{}, l.bin, l.env, nil)
	if err != nil {
		return nil, err
	}
	dir := req.WorkingDir
	// Subscribe to the directory's event stream before the session exists:
	// OpenCode drops every event published before a stream is connected, and
	// the caller cannot prompt until this call returns.
	srv.ensureStreamReady(ctx, dir)
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
		openCodeBase: newOpenCodeBase(d.rt, srv, sessionID, dir, openCodeV1{}),
		instructions: l.instructions, model: req.Model, variant: req.Effort,
		policy: req.Permissions.Normalized(), first: true,
		parts: map[string]int{}, roles: map[string]string{},
		partOwner: map[string]string{},
	}
	s.interrupt = s.Interrupt
	srv.subscribe(s)
	go s.pump()
	d.rt.bindSession(s, l)
	return s, nil
}

// openCodeSession is one OpenCode 1.x conversation over HTTP plus the shared
// event stream of its server.
type openCodeSession struct {
	*openCodeBase

	instructions string
	model        string
	variant      string
	policy       PermissionPolicy
	first        bool

	parts     map[string]int
	roles     map[string]string
	partOwner map[string]string
}

// Send prompts, or holds p until the running turn ends.
func (s *openCodeSession) Send(ctx context.Context, p Prompt) (Delivery, error) {
	return s.sendWith(ctx, s.Prompt, p)
}

// Prompt starts a turn without waiting: prompt_async answers 204 and the turn
// arrives on the event stream.
// It returns ErrTurnActive while a turn runs.
func (s *openCodeSession) Prompt(ctx context.Context, p Prompt) error {
	parts, err := s.promptParts(p)
	if err != nil {
		return err
	}
	if err := s.beginTurn(); err != nil {
		return err
	}
	body := map[string]any{"parts": parts}
	s.queueMu.Lock()
	s.parts = map[string]int{}
	if s.model != "" {
		body["model"] = openCodeModel(s.model)
	}
	if s.variant != "" {
		body["variant"] = s.variant
	}
	first := s.first
	s.first = false
	instructions := s.instructions
	s.queueMu.Unlock()
	if first && strings.TrimSpace(instructions) != "" {
		body["system"] = instructions
	}
	if err := s.srv.call(ctx, "POST", "/session/"+s.id+"/prompt_async", s.dir, body, nil); err != nil {
		s.endTurn()
		return err
	}
	return nil
}

// SetModel changes the model of the next prompt. The running turn keeps its
// model.
func (s *openCodeSession) SetModel(_ context.Context, model string) error {
	s.queueMu.Lock()
	s.model = model
	s.queueMu.Unlock()
	return nil
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
	return s.srv.call(ctx, "POST", "/session/"+s.id+"/permissions/"+permissionID, s.dir,
		map[string]any{"response": response}, nil)
}

// Interrupt aborts the current turn.
func (s *openCodeSession) Interrupt(ctx context.Context) error {
	return s.srv.call(ctx, "POST", "/session/"+s.id+"/abort", s.dir, map[string]any{}, nil)
}

// InterruptTurn is the protocol interrupt.
func (s *openCodeSession) InterruptTurn(ctx context.Context) error { return s.Interrupt(ctx) }

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
		s.busy = false
		lastErr := s.lastError
		s.queueMu.Unlock()
		s.afterTurn(s.Prompt)
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
		if err := srv.call(ctx, "GET", "/session/"+sessionID+"/message", sess.workDir(), nil, &raw); err != nil {
			return nil, false
		}
		return raw, len(raw) > 0
	}
	return nil, false
}

// openCodeModels lists models through a shared server of the wire. The probe
// shares a running server when the fingerprint matches. When no server
// starts, V1 falls back to `opencode models`.
func (rt *Runtime) openCodeModels(ctx context.Context, w openCodeWire, q ModelQuery) ([]ModelInfo, error) {
	req := probeRequest(w.harness(), q, q.WorkingDir)
	l, err := rt.prepare(ctx, &req, false)
	if err != nil {
		return nil, err
	}
	defer l.cleanup()
	srv, err := rt.oc.acquireServer(ctx, w, l.bin, l.env, nil)
	if err != nil {
		if w.harness() == OpenCode {
			if models, cliErr := openCodeCLIModels(ctx, l, q.WorkingDir); cliErr == nil {
				return models, nil
			}
		}
		return nil, err
	}
	defer rt.oc.release(srv)
	return w.models(ctx, srv, q.WorkingDir)
}

// openCodeCLIModels runs `opencode models`, which prints one provider/model
// per line.
func openCodeCLIModels(ctx context.Context, l *launch, dir string) ([]ModelInfo, error) {
	cmd := exec.CommandContext(ctx, l.bin, "models")
	cmd.Dir = dir
	cmd.Env = proc.ChildEnv(l.env)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("opencode models: %w", err)
	}
	var models []ModelInfo
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if provider, id, ok := strings.Cut(line, "/"); ok && provider != "" && id != "" && !strings.ContainsAny(line, " \t") {
			models = append(models, ModelInfo{ID: line})
		}
	}
	return models, nil
}

// sortModels orders a list by id, so a list from a JSON map is stable.
func sortModels(models []ModelInfo) {
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
}

// contextText renders a context window size as a short description.
func contextText(tokens int) string {
	switch {
	case tokens <= 0:
		return ""
	case tokens >= 1_000_000 && tokens%1_000_000 == 0:
		return fmt.Sprintf("%dM context", tokens/1_000_000)
	case tokens >= 1000:
		return fmt.Sprintf("%dK context", tokens/1000)
	}
	return fmt.Sprintf("%d context", tokens)
}

// joinNonEmpty joins the non-empty values with " · ".
func joinNonEmpty(values ...string) string {
	var parts []string
	for _, v := range values {
		if v != "" {
			parts = append(parts, v)
		}
	}
	return strings.Join(parts, " · ")
}

// Models implements ModelLister.
func (d openCodeDriver) Models(ctx context.Context, q ModelQuery) ([]ModelInfo, error) {
	return d.rt.openCodeModels(ctx, openCodeV1{}, q)
}

// models reads the V1 provider list (GET /config/providers) and marks the
// model of the merged config (GET /config) as the default.
func (openCodeV1) models(ctx context.Context, srv *openCodeServer, dir string) ([]ModelInfo, error) {
	var res struct {
		Providers []struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Models map[string]struct {
				ID    string `json:"id"`
				Name  string `json:"name"`
				Limit struct {
					Context int `json:"context"`
				} `json:"limit"`
			} `json:"models"`
		} `json:"providers"`
	}
	if err := srv.call(ctx, "GET", "/config/providers", dir, nil, &res); err != nil {
		return nil, err
	}
	var cfg struct {
		Model json.RawMessage `json:"model"`
	}
	_ = srv.call(ctx, "GET", "/config", dir, nil, &cfg)
	def := openCodeConfigModel(cfg.Model)
	var out []ModelInfo
	for _, p := range res.Providers {
		for key, m := range p.Models {
			id := p.ID + "/" + firstNonEmpty(m.ID, key)
			out = append(out, ModelInfo{
				ID: id, Name: firstNonEmpty(m.Name, m.ID, key),
				Description: joinNonEmpty(p.Name, contextText(m.Limit.Context)),
				Default:     id == def,
			})
		}
	}
	sortModels(out)
	return out, nil
}

// ensure the openCodeSession satisfies the Session interface.
var _ Session = (*openCodeSession)(nil)

// ensure the OpenCode session has the optional capabilities.
var (
	_ ModelLister = openCodeDriver{}
	_ ModelSetter = (*openCodeSession)(nil)
)

// ensure the driver satisfies the Driver interface.
var _ Driver = openCodeDriver{}

// ensure the V1 wire satisfies the wire interface.
var _ openCodeWire = openCodeV1{}

// ensure the session is routable on its server.
var _ openCodeRoute = (*openCodeSession)(nil)
