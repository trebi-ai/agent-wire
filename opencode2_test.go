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

// oc2TestSSE serves the OpenCode 2.x /api/event stream. A test pushes frames
// into it and the handler writes them as server-sent events.
type oc2TestSSE struct {
	frames chan string
	ready  chan struct{}
	stop   chan struct{}

	readyOnce sync.Once
	stopOnce  sync.Once

	mu    sync.Mutex
	conns int
}

// connections is the number of /api/event requests seen so far.
func (h *oc2TestSSE) connections() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.conns
}

func oc2TestNewSSE() *oc2TestSSE {
	return &oc2TestSSE{
		frames: make(chan string),
		ready:  make(chan struct{}),
		stop:   make(chan struct{}),
	}
}

func (h *oc2TestSSE) shutdown() {
	h.stopOnce.Do(func() { close(h.stop) })
}

// wait blocks until the first subscriber connected.
func (h *oc2TestSSE) wait(t *testing.T) {
	t.Helper()
	select {
	case <-h.ready:
	case <-time.After(3 * time.Second):
		t.Fatal("oc2TestSSE: no subscriber connected")
	}
}

// push sends one event frame and waits for the subscriber to take it.
func (h *oc2TestSSE) push(t *testing.T, typ string, data map[string]any) {
	t.Helper()
	frame, err := json.Marshal(map[string]any{"id": "evt_1", "type": typ, "data": data})
	if err != nil {
		t.Fatalf("oc2TestSSE: encode: %v", err)
	}
	select {
	case h.frames <- string(frame):
	case <-time.After(3 * time.Second):
		t.Fatal("oc2TestSSE: no subscriber took the frame")
	}
}

func (h *oc2TestSSE) serve(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	h.conns++
	h.mu.Unlock()
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

// oc2TestOpenCode is one httptest server that speaks the OpenCode 2.x HTTP and
// event surface, plus the router the sessions attach to.
type oc2TestOpenCode struct {
	route *openCodeServer
	sse   *oc2TestSSE

	mu           sync.Mutex
	prompts      []map[string]any
	creates      []map[string]any
	models       []map[string]any
	instructions []map[string]any
	replies      []map[string]any
	interrupts   int
}

// oc2TestNewOpenCode starts the test server and the router.
func oc2TestNewOpenCode(t *testing.T, rt *Runtime) *oc2TestOpenCode {
	t.Helper()
	oc := &oc2TestOpenCode{sse: oc2TestNewSSE()}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/event", oc.sse.serve)
	mux.HandleFunc("/api/session", func(w http.ResponseWriter, r *http.Request) {
		oc.mu.Lock()
		oc.creates = append(oc.creates, oc2TestReadBody(t, r))
		oc.mu.Unlock()
		oc2TestWriteJSON(t, w, map[string]any{"data": map[string]any{"id": "s1"}})
	})
	mux.HandleFunc("/api/session/s1", func(w http.ResponseWriter, r *http.Request) {
		oc2TestWriteJSON(t, w, map[string]any{"data": map[string]any{"id": "s1"}})
	})
	mux.HandleFunc("/api/session/s1/prompt", func(w http.ResponseWriter, r *http.Request) {
		body := oc2TestReadBody(t, r)
		oc.mu.Lock()
		oc.prompts = append(oc.prompts, body)
		oc.mu.Unlock()
		oc2TestWriteJSON(t, w, map[string]any{"data": map[string]any{"id": "msg_1"}})
	})
	mux.HandleFunc("/api/session/s1/model", func(w http.ResponseWriter, r *http.Request) {
		body := oc2TestReadBody(t, r)
		oc.mu.Lock()
		oc.models = append(oc.models, body)
		oc.mu.Unlock()
		oc2TestWriteJSON(t, w, map[string]any{"data": map[string]any{}})
	})
	mux.HandleFunc("/api/session/s1/instructions/entries/"+openCode2InstructionKey, func(w http.ResponseWriter, r *http.Request) {
		body := oc2TestReadBody(t, r)
		oc.mu.Lock()
		oc.instructions = append(oc.instructions, body)
		oc.mu.Unlock()
		oc2TestWriteJSON(t, w, map[string]any{"data": map[string]any{}})
	})
	mux.HandleFunc("/api/session/s1/permission/p1/reply", func(w http.ResponseWriter, r *http.Request) {
		body := oc2TestReadBody(t, r)
		oc.mu.Lock()
		oc.replies = append(oc.replies, body)
		oc.mu.Unlock()
		oc2TestWriteJSON(t, w, map[string]any{"data": map[string]any{}})
	})
	mux.HandleFunc("/api/session/s1/interrupt", func(w http.ResponseWriter, r *http.Request) {
		oc.mu.Lock()
		oc.interrupts++
		oc.mu.Unlock()
		oc2TestWriteJSON(t, w, map[string]any{"data": map[string]any{}})
	})
	ts := httptest.NewServer(mux)
	u, err := url.Parse(ts.URL)
	if err != nil {
		ts.Close()
		t.Fatalf("oc2TestOpenCode: server url: %v", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		ts.Close()
		t.Fatalf("oc2TestOpenCode: server port: %v", err)
	}
	oc.route = &openCodeServer{
		port: port, username: "u", password: "p", rt: rt, wire: openCode2{},
		sessions: map[string]openCodeRoute{},
	}
	t.Cleanup(ts.Close)
	t.Cleanup(func() {
		oc.sse.shutdown()
		oc.route.stop()
	})
	return oc
}

// session builds one OpenCode 2.x session over the shared route with the fields
// the driver sets, subscribes it, and starts its pump.
func (oc *oc2TestOpenCode) session(id, directory, instructions string, policy PermissionPolicy) *openCode2Session {
	s := &openCode2Session{
		openCodeBase: newOpenCodeBase(oc.route.rt, oc.route, id, directory, openCode2{}),
		instructions: instructions, policy: policy.Normalized(),
		text: map[string]int{}, tools: map[string]string{},
	}
	oc.route.subscribe(s)
	oc.route.ensureStream(directory)
	go s.pump()
	return s
}

func (oc *oc2TestOpenCode) promptBodies() []map[string]any {
	oc.mu.Lock()
	defer oc.mu.Unlock()
	return append([]map[string]any(nil), oc.prompts...)
}

func (oc *oc2TestOpenCode) replyBodies() []map[string]any {
	oc.mu.Lock()
	defer oc.mu.Unlock()
	return append([]map[string]any(nil), oc.replies...)
}

func (oc *oc2TestOpenCode) instructionBodies() []map[string]any {
	oc.mu.Lock()
	defer oc.mu.Unlock()
	return append([]map[string]any(nil), oc.instructions...)
}

// oc2TestWriteJSON writes one JSON response from a server handler.
func oc2TestWriteJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("oc2TestOpenCode: write response: %v", err)
	}
}

// oc2TestReadBody decodes one JSON request body from a server handler.
func oc2TestReadBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Errorf("oc2TestOpenCode: decode body: %v", err)
		return map[string]any{}
	}
	return body
}

// oc2TestText builds one text block payload for an assistant message.
func oc2TestText(messageID, ordinal, text string) map[string]any {
	return map[string]any{
		"sessionID": "s1", "assistantMessageID": messageID, "ordinal": num(ordinal), "text": text,
	}
}

// TestOpenCode2SessionEvents maps the OpenCode 2.x event vocabulary to
// normalized events.
func TestOpenCode2SessionEvents(t *testing.T) {
	oc := oc2TestNewOpenCode(t, coaTestRuntime())
	sess := oc.session("s1", "", "", PermissionPolicy{})
	events := coaTestWatch(t, sess.Events())
	oc.sse.wait(t)

	// The session reports itself live. The bus event is not the source: it is
	// published while the create is in flight, so it can arrive before the
	// session is routable and be dropped.
	sess.markLive()
	if ev := events.next(EventInit); ev.SessionID != "s1" {
		t.Fatalf("init event: %+v", ev)
	}
	sess.markLive()
	oc.sse.push(t, "session.created", map[string]any{"sessionID": "s1"})
	oc.sse.push(t, "session.text.delta", map[string]any{
		"sessionID": "s1", "assistantMessageID": "m0", "ordinal": 0, "delta": "x",
	})
	events.next(EventAssistant)
	for _, ev := range events.buf {
		if ev.Type == EventInit {
			t.Fatalf("a second init event was emitted: %+v", ev)
		}
	}

	// Text streams as deltas, and the ended event carries the full block.
	oc.sse.push(t, "session.text.delta", map[string]any{
		"sessionID": "s1", "assistantMessageID": "m1", "ordinal": 0, "delta": "po",
	})
	first := events.next(EventAssistant)
	if first.Text != "po" || !first.Delta {
		t.Fatalf("first delta: %+v", first)
	}
	oc.sse.push(t, "session.text.delta", map[string]any{
		"sessionID": "s1", "assistantMessageID": "m1", "ordinal": 0, "delta": "ng",
	})
	second := events.next(EventAssistant)
	if second.Text != "ng" || !second.Delta {
		t.Fatalf("second delta: %+v", second)
	}
	// The ended text appends a suffix that never streamed: the caller still
	// receives it, as a delta.
	oc.sse.push(t, "session.text.ended", oc2TestText("m1", "0", "pong\n(exit code 0)"))
	tail := events.next(EventAssistant)
	if tail.Text != "\n(exit code 0)" || !tail.Delta {
		t.Fatalf("reconciled tail: %+v", tail)
	}
	// A repeated ended event has nothing left to send.
	oc.sse.push(t, "session.text.ended", oc2TestText("m1", "0", "pong\n(exit code 0)"))
	oc.sse.push(t, "session.execution.succeeded", map[string]any{"sessionID": "s1"})
	result := events.next(EventResult)
	if result.Result == nil || result.Result.IsError || result.Result.Subtype != "success" {
		t.Fatalf("success result: %+v", result.Result)
	}
	for _, ev := range events.buf {
		if ev.Type == EventAssistant {
			t.Fatalf("the repeated ended event emitted %+v", ev)
		}
	}
}

// TestOpenCode2TextEndedReplaces checks the replacement path: a block whose
// final text is shorter than what streamed is re-sent whole, so the caller
// never keeps a discarded prefix.
func TestOpenCode2TextEndedReplaces(t *testing.T) {
	oc := oc2TestNewOpenCode(t, coaTestRuntime())
	sess := oc.session("s1", "", "", PermissionPolicy{})
	events := coaTestWatch(t, sess.Events())
	oc.sse.wait(t)

	oc.sse.push(t, "session.text.delta", map[string]any{
		"sessionID": "s1", "assistantMessageID": "m1", "ordinal": 0, "delta": "a long draft",
	})
	if ev := events.next(EventAssistant); !ev.Delta {
		t.Fatalf("streamed draft: %+v", ev)
	}
	oc.sse.push(t, "session.text.ended", oc2TestText("m1", "0", "short"))
	replaced := events.next(EventAssistant)
	if replaced.Text != "short" || replaced.Delta {
		t.Fatalf("replacement: %+v", replaced)
	}
}

// TestOpenCode2ToolEvents maps the V2 tool vocabulary to one normalized tool
// event per stage, with the name remembered from the start of the call.
func TestOpenCode2ToolEvents(t *testing.T) {
	oc := oc2TestNewOpenCode(t, coaTestRuntime())
	sess := oc.session("s1", "", "", PermissionPolicy{})
	events := coaTestWatch(t, sess.Events())
	oc.sse.wait(t)

	oc.sse.push(t, "session.tool.input.started", map[string]any{
		"sessionID": "s1", "assistantMessageID": "m1", "id": "call_1", "name": "write",
	})
	started := events.next(EventTool)
	if started.Tool == nil || started.Tool.Name != "write" || started.Tool.Status != "started" {
		t.Fatalf("tool start: %+v", started.Tool)
	}
	if started.Tool.Kind != ToolEdit {
		t.Fatalf("tool kind: %+v", started.Tool)
	}

	oc.sse.push(t, "session.tool.called", map[string]any{
		"sessionID": "s1", "id": "call_1",
		"input": map[string]any{"path": "/tmp/probe.txt", "content": "hello"},
	})
	called := events.next(EventTool)
	if called.Tool == nil || called.Tool.Input == "" {
		t.Fatalf("tool input: %+v", called.Tool)
	}
	if len(called.Tool.Paths) == 0 || called.Tool.Paths[0] != "/tmp/probe.txt" {
		t.Fatalf("tool paths: %+v", called.Tool.Paths)
	}

	oc.sse.push(t, "session.tool.success", map[string]any{
		"sessionID": "s1", "id": "call_1",
		"content": []any{map[string]any{"type": "text", "text": "Created file successfully: probe.txt"}},
	})
	done := events.next(EventTool)
	if done.Tool == nil || done.Tool.Status != "completed" || !strings.Contains(done.Tool.Output, "Created") {
		t.Fatalf("tool result: %+v", done.Tool)
	}
	if done.Tool.Name != "write" {
		t.Fatalf("tool name lost after the start event: %+v", done.Tool)
	}
}

// TestOpenCode2FailureIsClassified checks that a structured V2 failure becomes
// a coded error and an error result.
func TestOpenCode2FailureIsClassified(t *testing.T) {
	oc := oc2TestNewOpenCode(t, coaTestRuntime())
	sess := oc.session("s1", "", "", PermissionPolicy{})
	events := coaTestWatch(t, sess.Events())
	oc.sse.wait(t)

	oc.sse.push(t, "session.step.failed", map[string]any{
		"sessionID": "s1", "assistantMessageID": "m1",
		"error": map[string]any{"type": "provider.auth", "message": "OAuth access token is invalid.", "status": 401},
	})
	failure := events.next(EventError)
	if failure.Code != "opencode2.invalid_credentials" {
		t.Fatalf("error code: %q", failure.Code)
	}
	if failure.EndReason != string(FailAuth) {
		t.Fatalf("end reason: %q", failure.EndReason)
	}
	oc.sse.push(t, "session.execution.failed", map[string]any{
		"sessionID": "s1",
		"error":     map[string]any{"type": "provider.auth", "message": "OAuth access token is invalid.", "status": 401},
	})
	result := events.next(EventResult)
	if result.Result == nil || !result.Result.IsError {
		t.Fatalf("failure result: %+v", result.Result)
	}
	if !strings.HasPrefix(result.Result.Code, "opencode2.") {
		t.Fatalf("failure result code: %q", result.Result.Code)
	}
}

// TestOpenCode2UsageFromStep checks that a finished step reports tokens and
// cost, including the V2 nested cache counters.
func TestOpenCode2UsageFromStep(t *testing.T) {
	oc := oc2TestNewOpenCode(t, coaTestRuntime())
	sess := oc.session("s1", "", "", PermissionPolicy{})
	events := coaTestWatch(t, sess.Events())
	oc.sse.wait(t)

	oc.sse.push(t, "session.step.ended", map[string]any{
		"sessionID": "s1", "assistantMessageID": "m1", "finish": "stop",
		"cost": 0.0019,
		"tokens": map[string]any{
			"input": 12548, "output": 3, "reasoning": 0,
			"cache": map[string]any{"read": 128, "write": 0},
		},
	})
	usage := events.next(EventUsage)
	if usage.Usage == nil {
		t.Fatal("no usage event")
	}
	if usage.Usage.Input != 12548 || usage.Usage.Output != 3 || usage.Usage.CacheRead != 128 {
		t.Fatalf("usage: %+v", usage.Usage)
	}
	if usage.Usage.CostUSD != 0.0019 {
		t.Fatalf("cost: %+v", usage.Usage)
	}
}

// TestOpenCode2PermissionForwarded checks that a request the policy does not
// answer reaches the caller with the vendor action and resources.
func TestOpenCode2PermissionForwarded(t *testing.T) {
	oc := oc2TestNewOpenCode(t, coaTestRuntime())
	sess := oc.session("s1", "", "", PermissionPolicy{})
	events := coaTestWatch(t, sess.Events())
	oc.sse.wait(t)

	oc.sse.push(t, "permission.asked", map[string]any{
		"id": "p1", "sessionID": "s1", "action": "write",
		"resources": []any{"/tmp/probe.txt"}, "message": "OpenCode wants to write a file",
	})
	req := events.next(EventPermission)
	if req.Permission == nil || req.Permission.ID != "p1" {
		t.Fatalf("permission event: %+v", req.Permission)
	}
	if req.Permission.Tool != "write" || req.Permission.Kind != ToolEdit {
		t.Fatalf("permission tool: %+v", req.Permission)
	}
	if req.Permission.Question != "OpenCode wants to write a file" {
		t.Fatalf("permission question: %q", req.Permission.Question)
	}
	if err := sess.AnswerPermission(context.Background(), "p1", Decision{Allow: true}); err != nil {
		t.Fatalf("answer: %v", err)
	}
	replies := oc.replyBodies()
	if len(replies) != 1 || replies[0]["reply"] != "once" {
		t.Fatalf("reply bodies: %+v", replies)
	}
}

// TestOpenCode2PermissionPolicyAnswers checks that an auto policy answers the
// vendor itself and never bothers the caller.
func TestOpenCode2PermissionPolicyAnswers(t *testing.T) {
	oc := oc2TestNewOpenCode(t, coaTestRuntime())
	sess := oc.session("s1", "", "", PermissionPolicy{Mode: PermissionAuto})
	events := coaTestWatch(t, sess.Events())
	oc.sse.wait(t)

	oc.sse.push(t, "permission.asked", map[string]any{
		"id": "p1", "sessionID": "s1", "action": "write", "resources": []any{"/tmp/probe.txt"},
	})
	oc.sse.push(t, "session.execution.succeeded", map[string]any{"sessionID": "s1"})
	events.next(EventResult)
	for _, ev := range events.buf {
		if ev.Type == EventPermission {
			t.Fatalf("the auto policy forwarded a permission: %+v", ev)
		}
	}
	deadline := time.After(3 * time.Second)
	for len(oc.replyBodies()) == 0 {
		select {
		case <-deadline:
			t.Fatal("the auto policy never answered the request")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if reply := oc.replyBodies()[0]; reply["reply"] != "once" {
		t.Fatalf("auto reply: %+v", reply)
	}
}

// TestOpenCode2PromptBody pins the prompt route body: the text, the location
// the session runs in, and one file input per attachment.
func TestOpenCode2PromptBody(t *testing.T) {
	oc := oc2TestNewOpenCode(t, coaTestRuntime())
	dir := t.TempDir()
	sess := oc.session("s1", dir, "", PermissionPolicy{})
	oc.sse.wait(t)

	attachment := injTestAttachmentFile(t, "note.txt", []byte("hello attachment"))
	if err := sess.Prompt(context.Background(), Prompt{
		Text:        "hello",
		Attachments: []Attachment{{Path: attachment}},
	}); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	bodies := oc.promptBodies()
	if len(bodies) != 1 {
		t.Fatalf("prompt bodies: %d", len(bodies))
	}
	if bodies[0]["text"] != "hello" {
		t.Fatalf("prompt text: %+v", bodies[0]["text"])
	}
	location, _ := bodies[0]["location"].(map[string]any)
	if location == nil || location["directory"] != dir {
		t.Fatalf("prompt location: %+v", bodies[0]["location"])
	}
	files, _ := bodies[0]["files"].([]any)
	if len(files) != 1 {
		t.Fatalf("prompt files: %+v", bodies[0]["files"])
	}
	file, _ := files[0].(map[string]any)
	if file["uri"] != "file://"+attachment {
		t.Fatalf("file uri: %+v", file)
	}
	if file["name"] != "note.txt" {
		t.Fatalf("file name: %+v", file)
	}
}

// TestOpenCode2InstructionsEntry pins the V2 instruction path: a per-session
// entry under the key this library owns.
func TestOpenCode2InstructionsEntry(t *testing.T) {
	oc := oc2TestNewOpenCode(t, coaTestRuntime())
	sess := oc.session("s1", "", "be brief", PermissionPolicy{})
	oc.sse.wait(t)

	if err := sess.setInstructions(context.Background()); err != nil {
		t.Fatalf("setInstructions: %v", err)
	}
	bodies := oc.instructionBodies()
	if len(bodies) != 1 {
		t.Fatalf("instruction bodies: %d", len(bodies))
	}
	if bodies[0]["value"] != "be brief" {
		t.Fatalf("instruction value: %+v", bodies[0])
	}

	// A session with no instructions writes nothing.
	quiet := oc.session("s2", "", "  ", PermissionPolicy{})
	if err := quiet.setInstructions(context.Background()); err != nil {
		t.Fatalf("setInstructions (empty): %v", err)
	}
	if len(oc.instructionBodies()) != 1 {
		t.Fatalf("an empty instruction set wrote an entry")
	}
}

// TestOpenCode2ModelRef covers the model pin rendering, including the bare
// model name a caller may pass.
func TestOpenCode2ModelRef(t *testing.T) {
	tests := []struct {
		model   string
		variant string
		want    map[string]any
	}{
		{"opencode-go/deepseek-v4.1-flash", "", map[string]any{"providerID": "opencode-go", "id": "deepseek-v4.1-flash"}},
		{"anthropic/claude-sonnet-4", "high", map[string]any{"providerID": "anthropic", "id": "claude-sonnet-4", "variant": "high"}},
		{"deepseek-v4.1-flash", "", map[string]any{"id": "deepseek-v4.1-flash"}},
		{"", "", nil},
	}
	for _, tt := range tests {
		got := openCode2ModelRef(tt.model, tt.variant)
		if tt.want == nil {
			if got != nil {
				t.Errorf("openCode2ModelRef(%q) = %+v, want nil", tt.model, got)
			}
			continue
		}
		if len(got) != len(tt.want) {
			t.Errorf("openCode2ModelRef(%q) = %+v, want %+v", tt.model, got, tt.want)
			continue
		}
		for k, v := range tt.want {
			if got[k] != v {
				t.Errorf("openCode2ModelRef(%q)[%s] = %v, want %v", tt.model, k, got[k], v)
			}
		}
	}
}

// TestOpenCode2InterruptRoute pins the interrupt route: V2 renamed abort to
// interrupt.
func TestOpenCode2InterruptRoute(t *testing.T) {
	oc := oc2TestNewOpenCode(t, coaTestRuntime())
	sess := oc.session("s1", "", "", PermissionPolicy{})
	oc.sse.wait(t)

	if err := sess.Interrupt(context.Background()); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	if err := sess.InterruptTurn(context.Background()); err != nil {
		t.Fatalf("interrupt turn: %v", err)
	}
	oc.mu.Lock()
	got := oc.interrupts
	oc.mu.Unlock()
	if got != 2 {
		t.Fatalf("interrupts: %d, want 2", got)
	}
}

// TestOpenCode2StreamIsNotDirectoryScoped pins the V2 event scope: one stream
// serves every location, so two sessions in different directories share it.
func TestOpenCode2StreamIsNotDirectoryScoped(t *testing.T) {
	oc := oc2TestNewOpenCode(t, coaTestRuntime())

	oc.route.ensureStream("/tmp/aw-proj-one")
	oc.sse.wait(t)
	oc.route.ensureStream("/tmp/aw-proj-two")
	oc.route.ensureStream("")

	time.Sleep(50 * time.Millisecond)
	if got := oc.sse.connections(); got != 1 {
		t.Fatalf("got %d /api/event connections, want 1 for every location", got)
	}
}

// TestOpenCode2SessionRouting proves two sessions on one server each receive
// only their own events.
func TestOpenCode2SessionRouting(t *testing.T) {
	oc := oc2TestNewOpenCode(t, coaTestRuntime())
	first := oc.session("s1", "", "", PermissionPolicy{})
	second := oc.session("s2", "", "", PermissionPolicy{})
	firstEvents := coaTestWatch(t, first.Events())
	secondEvents := coaTestWatch(t, second.Events())
	oc.sse.wait(t)

	oc.sse.push(t, "session.text.delta", map[string]any{
		"sessionID": "s1", "assistantMessageID": "a1", "ordinal": 0, "delta": "one",
	})
	oc.sse.push(t, "session.text.delta", map[string]any{
		"sessionID": "s2", "assistantMessageID": "b1", "ordinal": 0, "delta": "two",
	})

	if ev := firstEvents.next(EventAssistant); ev.Text != "one" {
		t.Fatalf("first session event: %+v", ev)
	}
	if ev := secondEvents.next(EventAssistant); ev.Text != "two" {
		t.Fatalf("second session event: %+v", ev)
	}
}

// TestOpenCode2ServerExitEndsSession proves a session on a dead server ends
// with a reason instead of hanging with no events.
func TestOpenCode2ServerExitEndsSession(t *testing.T) {
	oc := oc2TestNewOpenCode(t, coaTestRuntime())
	sess := oc.session("s1", "", "", PermissionPolicy{})
	events := coaTestWatch(t, sess.Events())
	oc.sse.wait(t)

	oc.route.stop()
	exit := events.next(EventExit)
	if exit.Code != "server_exited" {
		t.Fatalf("exit event: %+v", exit)
	}
	if exit.ExitCode == nil || *exit.ExitCode != -1 {
		t.Fatalf("exit code: %+v", exit.ExitCode)
	}
	if err := sess.Err(); err == nil {
		t.Fatal("the session recorded no error for a dead server")
	}
}
