package agentwire

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// coaTestSSE serves the OpenCode /event stream. A test pushes bus frames into
// it and the handler writes them as server-sent events.
type coaTestSSE struct {
	frames chan string
	ready  chan struct{}
	stop   chan struct{}

	readyOnce sync.Once
	stopOnce  sync.Once
}

// coaTestNewSSE builds an event hub with no subscriber.
func coaTestNewSSE() *coaTestSSE {
	return &coaTestSSE{
		frames: make(chan string),
		ready:  make(chan struct{}),
		stop:   make(chan struct{}),
	}
}

// shutdown releases every streaming handler, so the test server can close.
func (h *coaTestSSE) shutdown() {
	h.stopOnce.Do(func() { close(h.stop) })
}

// wait blocks until the first subscriber connected.
func (h *coaTestSSE) wait(t *testing.T) {
	t.Helper()
	select {
	case <-h.ready:
	case <-time.After(3 * time.Second):
		t.Fatal("coaTestSSE: no subscriber connected")
	}
}

// push sends one bus frame and waits for the subscriber to take it.
func (h *coaTestSSE) push(t *testing.T, typ string, props map[string]any) {
	t.Helper()
	frame, err := json.Marshal(map[string]any{"type": typ, "properties": props})
	if err != nil {
		t.Fatalf("coaTestSSE: encode: %v", err)
	}
	select {
	case h.frames <- string(frame):
	case <-time.After(3 * time.Second):
		t.Fatal("coaTestSSE: no subscriber took the frame")
	}
}

// serve streams the pushed frames until the client goes away.
func (h *coaTestSSE) serve(w http.ResponseWriter, r *http.Request) {
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	if flusher != nil {
		flusher.Flush()
	}
	h.readyOnce.Do(func() { close(h.ready) })
	for {
		select {
		case <-r.Context().Done():
			return
		case <-h.stop:
			return
		case frame := <-h.frames:
			if _, err := io.WriteString(w, "data: "+frame+"\n\n"); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}

// coaTestOpenCode is one httptest server that speaks the OpenCode HTTP and
// event surface, plus the router the sessions attach to.
type coaTestOpenCode struct {
	route *openCodeServer
	sse   *coaTestSSE

	mu      sync.Mutex
	prompts []map[string]any
}

// coaTestNewOpenCode starts the test server and the router.
func coaTestNewOpenCode(t *testing.T, rt *Runtime) *coaTestOpenCode {
	t.Helper()
	oc := &coaTestOpenCode{sse: coaTestNewSSE()}
	mux := http.NewServeMux()
	mux.HandleFunc("/event", oc.sse.serve)
	mux.HandleFunc("/session", func(w http.ResponseWriter, r *http.Request) {
		coaTestWriteJSON(t, w, map[string]any{"id": "s1"})
	})
	mux.HandleFunc("/session/s1", func(w http.ResponseWriter, r *http.Request) {
		coaTestWriteJSON(t, w, map[string]any{"id": "s1"})
	})
	mux.HandleFunc("/session/s1/prompt_async", func(w http.ResponseWriter, r *http.Request) {
		body := coaTestReadBody(t, r)
		oc.mu.Lock()
		oc.prompts = append(oc.prompts, body)
		oc.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/session/s1/permissions/p1", func(w http.ResponseWriter, r *http.Request) {
		coaTestReadBody(t, r)
		coaTestWriteJSON(t, w, map[string]any{})
	})
	mux.HandleFunc("/session/s1/abort", func(w http.ResponseWriter, r *http.Request) {
		coaTestWriteJSON(t, w, map[string]any{})
	})
	mux.HandleFunc("/session/s1/message", func(w http.ResponseWriter, r *http.Request) {
		coaTestWriteJSON(t, w, []any{})
	})
	ts := httptest.NewServer(mux)
	u, err := url.Parse(ts.URL)
	if err != nil {
		ts.Close()
		t.Fatalf("coaTestOpenCode: server url: %v", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		ts.Close()
		t.Fatalf("coaTestOpenCode: server port: %v", err)
	}
	oc.route = &openCodeServer{
		port: port, username: "u", password: "p", rt: rt,
		sessions: map[string]*openCodeSession{},
	}
	// The streaming handler must stop before the server can close, so the
	// release cleanup runs first (cleanups run last in, first out).
	t.Cleanup(ts.Close)
	t.Cleanup(func() {
		oc.sse.shutdown()
		oc.route.stop()
	})
	return oc
}

// session builds one OpenCode session over the shared route with the fields the
// driver sets, subscribes it, and starts its pump.
func (oc *coaTestOpenCode) session(id, directory, instructions, model, variant string, policy PermissionPolicy) *openCodeSession {
	s := &openCodeSession{
		rt: oc.route.rt, srv: oc.route, id: id, directory: directory,
		instructions: instructions, model: model, variant: variant,
		policy: policy.Normalized(), first: true,
		events: make(chan Event, 256), done: make(chan struct{}),
		notify: make(chan struct{}, 1), pumpStop: make(chan struct{}),
		parts: map[string]int{}, roles: map[string]string{}, partOwner: map[string]string{},
	}
	oc.route.subscribe(s)
	oc.route.ensureStream()
	go s.pump()
	return s
}

// promptBodies returns the recorded prompt_async request bodies.
func (oc *coaTestOpenCode) promptBodies() []map[string]any {
	oc.mu.Lock()
	defer oc.mu.Unlock()
	return append([]map[string]any(nil), oc.prompts...)
}

// coaTestWriteJSON writes one JSON response from a server handler.
func coaTestWriteJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("coaTestOpenCode: write response: %v", err)
	}
}

// coaTestReadBody decodes one JSON request body from a server handler.
func coaTestReadBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Errorf("coaTestOpenCode: decode body: %v", err)
		return map[string]any{}
	}
	return body
}

// coaTestMessageUpdated renders one message.updated frame for a role.
func coaTestMessageUpdated(sessionID, messageID, role string) (string, map[string]any) {
	return "message.updated", map[string]any{
		"info": map[string]any{"id": messageID, "role": role, "sessionID": sessionID},
	}
}

// coaTestTextPart renders one text part of a message.
func coaTestTextPart(sessionID, messageID, partID, text string) (string, map[string]any) {
	return "message.part.updated", map[string]any{"part": map[string]any{
		"id": partID, "messageID": messageID, "sessionID": sessionID, "type": "text", "text": text,
	}}
}

// TestOpenCodeSessionEvents maps the OpenCode bus events to normalized events.
func TestOpenCodeSessionEvents(t *testing.T) {
	oc := coaTestNewOpenCode(t, coaTestRuntime())
	sess := oc.session("s1", "", "", "", "", PermissionPolicy{})
	events := coaTestWatch(t, sess.Events())
	oc.sse.wait(t)

	// A part of a user message is dropped: the bus echoes the prompt back.
	typ, props := coaTestMessageUpdated("s1", "m1", "user")
	oc.sse.push(t, typ, props)
	typ, props = coaTestTextPart("s1", "m1", "p1", "my own words")
	oc.sse.push(t, typ, props)

	typ, props = coaTestMessageUpdated("s1", "m2", "assistant")
	oc.sse.push(t, typ, props)
	typ, props = coaTestTextPart("s1", "m2", "p2", "pong")
	oc.sse.push(t, typ, props)

	first := events.next(EventAssistant)
	if first.Text != "pong" || first.Delta {
		t.Fatalf("first assistant event: %+v", first)
	}
	for _, ev := range events.buf {
		if strings.Contains(ev.Text, "my own words") {
			t.Fatalf("user text leaked into the stream: %+v", ev)
		}
	}

	// One part streamed three times becomes a block and then two deltas.
	typ, props = coaTestTextPart("s1", "m2", "p3", "po")
	oc.sse.push(t, typ, props)
	typ, props = coaTestTextPart("s1", "m2", "p3", "pon")
	oc.sse.push(t, typ, props)
	typ, props = coaTestTextPart("s1", "m2", "p3", "pong")
	oc.sse.push(t, typ, props)

	block := events.next(EventAssistant)
	if block.Text != "po" || block.Delta {
		t.Fatalf("block event: %+v", block)
	}
	for _, want := range []string{"n", "g"} {
		delta := events.next(EventAssistant)
		if delta.Text != want || !delta.Delta {
			t.Fatalf("delta event: %+v", delta)
		}
	}

	oc.sse.push(t, "message.part.updated", map[string]any{"part": map[string]any{
		"id": "p4", "messageID": "m2", "sessionID": "s1", "type": "tool", "tool": "bash",
		"state": map[string]any{
			"status": "completed",
			"input":  map[string]any{"command": "ls"},
			"output": "total 0",
		},
	}})
	tool := events.next(EventTool)
	if tool.Tool == nil || tool.Tool.Name != "bash" || tool.Tool.Kind != ToolExec {
		t.Fatalf("tool event: %+v", tool.Tool)
	}
	if tool.Tool.Status != "completed" {
		t.Fatalf("tool status: %+v", tool.Tool)
	}

	oc.sse.push(t, "session.idle", map[string]any{"sessionID": "s1"})
	idle := events.next(EventResult)
	if idle.Result == nil || idle.Result.IsError || idle.Result.Subtype != "success" {
		t.Fatalf("idle result: %+v", idle.Result)
	}

	oc.sse.push(t, "session.error", map[string]any{
		"sessionID": "s1",
		"error":     map[string]any{"message": "provider failed with 503"},
	})
	failure := events.next(EventError)
	if !strings.HasPrefix(failure.Code, "opencode.") {
		t.Fatalf("error code: %q", failure.Code)
	}

	// The error is remembered, so the next idle ends the turn as an error.
	oc.sse.push(t, "session.idle", map[string]any{"sessionID": "s1"})
	after := events.next(EventResult)
	if after.Result == nil || !after.Result.IsError {
		t.Fatalf("idle after an error: %+v", after.Result)
	}
	if !strings.HasPrefix(after.Result.Code, "opencode.") {
		t.Fatalf("idle after an error: code %q", after.Result.Code)
	}
}

// TestOpenCodeSessionDeleted proves a deleted session ends with a code and an
// exit code instead of a bare channel close.
func TestOpenCodeSessionDeleted(t *testing.T) {
	oc := coaTestNewOpenCode(t, coaTestRuntime())
	sess := oc.session("s1", "", "", "", "", PermissionPolicy{})
	events := coaTestWatch(t, sess.Events())
	oc.sse.wait(t)

	oc.sse.push(t, "session.deleted", map[string]any{"sessionID": "s1"})
	exit := events.next(EventExit)
	if exit.Code != "session_deleted" {
		t.Fatalf("exit event: %+v", exit)
	}
	if exit.ExitCode == nil {
		t.Fatalf("exit event has no exit code: %+v", exit)
	}
}

// TestOpenCodeSessionRouting proves two sessions on one server each receive
// only their own events.
func TestOpenCodeSessionRouting(t *testing.T) {
	oc := coaTestNewOpenCode(t, coaTestRuntime())
	first := oc.session("s1", "", "", "", "", PermissionPolicy{})
	second := oc.session("s2", "", "", "", "", PermissionPolicy{})
	firstEvents := coaTestWatch(t, first.Events())
	secondEvents := coaTestWatch(t, second.Events())
	oc.sse.wait(t)

	typ, props := coaTestMessageUpdated("s1", "a1", "assistant")
	oc.sse.push(t, typ, props)
	typ, props = coaTestTextPart("s1", "a1", "q1", "one")
	oc.sse.push(t, typ, props)
	typ, props = coaTestMessageUpdated("s2", "b1", "assistant")
	oc.sse.push(t, typ, props)
	typ, props = coaTestTextPart("s2", "b1", "q2", "two")
	oc.sse.push(t, typ, props)

	if ev := firstEvents.next(EventAssistant); ev.Text != "one" {
		t.Fatalf("first session event: %+v", ev)
	}
	if ev := secondEvents.next(EventAssistant); ev.Text != "two" {
		t.Fatalf("second session event: %+v", ev)
	}
}

// TestOpenCodePromptBody pins the prompt_async body: model and variant every
// turn, system only on the first turn of the session.
func TestOpenCodePromptBody(t *testing.T) {
	oc := coaTestNewOpenCode(t, coaTestRuntime())
	sess := oc.session("s1", t.TempDir(), "be brief", "anthropic/claude-sonnet-4", "high", PermissionPolicy{})
	oc.sse.wait(t)

	if err := sess.Prompt(context.Background(), Prompt{Text: "hello"}); err != nil {
		t.Fatalf("first prompt: %v", err)
	}
	if err := sess.Prompt(context.Background(), Prompt{Text: "again"}); err != nil {
		t.Fatalf("second prompt: %v", err)
	}

	bodies := oc.promptBodies()
	if len(bodies) != 2 {
		t.Fatalf("prompt_async bodies: %d", len(bodies))
	}
	model, _ := bodies[0]["model"].(map[string]any)
	if model == nil || model["providerID"] != "anthropic" || model["modelID"] != "claude-sonnet-4" {
		t.Fatalf("first prompt model: %+v", bodies[0]["model"])
	}
	if bodies[0]["variant"] != "high" {
		t.Fatalf("first prompt variant: %+v", bodies[0]["variant"])
	}
	if bodies[0]["system"] != "be brief" {
		t.Fatalf("first prompt system: %+v", bodies[0]["system"])
	}
	if _, ok := bodies[1]["system"]; ok {
		t.Fatalf("second prompt carried system: %+v", bodies[1])
	}
	if bodies[1]["variant"] != "high" {
		t.Fatalf("second prompt variant: %+v", bodies[1]["variant"])
	}
	parts, _ := bodies[0]["parts"].([]any)
	if len(parts) == 0 {
		t.Fatalf("first prompt parts: %+v", bodies[0]["parts"])
	}
	last, _ := parts[len(parts)-1].(map[string]any)
	if last["type"] != "text" || last["text"] != "hello" {
		t.Fatalf("first prompt text part: %+v", last)
	}
}
