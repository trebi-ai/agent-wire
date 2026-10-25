package agentwire

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trebi-ai/agent-wire/internal/wire"
)

// coaTestACPAgent answers the ACP initialize and session/new requests.
func coaTestACPAgent(p *coaTestPeer, m coaTestFrame) {
	switch m.method() {
	case "initialize":
		p.reply(m.m["id"], map[string]any{
			"protocolVersion": 1,
			"authMethods":     []any{map[string]any{"id": "github-device", "name": "GitHub"}},
			"agentCapabilities": map[string]any{
				"mcpCapabilities": map[string]any{"http": true},
			},
		})
	case "session/new":
		p.reply(m.m["id"], map[string]any{"sessionId": "ses_1"})
	}
}

// coaTestStartACP opens an in-memory ACP session against a scripted peer.
func coaTestStartACP(t *testing.T, kind acpKind, req StartRequest, handler func(*coaTestPeer, coaTestFrame), resume ...bool) (*coaTestPeer, *wire.Session, *acpProtocol) {
	isResume := len(resume) > 0 && resume[0]
	t.Helper()
	if req.HandshakeTimeout <= 0 {
		req.HandshakeTimeout = 2 * time.Second
	}
	peer := coaTestNewPeer(t, handler)
	proto := newACPProtocol(coaTestRuntime(), kind, req, &launch{}, false, isResume)
	s := wire.NewMemorySession(string(kind.harness()), proto, peer.inW, peer.out, context.Background(), wire.Config{})
	return peer, s, proto
}

// coaTestUpdate renders one session/update notification.
func coaTestUpdate(sessionID string, update map[string]any) map[string]any {
	return map[string]any{"sessionId": sessionID, "update": update}
}

// coaTestPromptID reads the request id out of an encoded prompt frame.
func coaTestPromptID(t *testing.T, frame []byte, method string) json.Number {
	t.Helper()
	var m struct {
		ID     json.Number `json:"id"`
		Method string      `json:"method"`
	}
	if err := json.Unmarshal(frame, &m); err != nil {
		t.Fatalf("coaTest: decode %s: %v", method, err)
	}
	if m.Method != method {
		t.Fatalf("coaTest: method %q, want %q", m.Method, method)
	}
	return m.ID
}

// coaTestResponse renders one JSON-RPC response frame for id.
func coaTestResponse(t *testing.T, id json.Number, result map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	if err != nil {
		t.Fatalf("coaTest: encode response: %v", err)
	}
	return b
}

// coaTestFS is a scripted FileSystem for the ACP file routes.
type coaTestFS struct {
	content string
	err     error

	mu     sync.Mutex
	reads  []string
	writes []string
}

// ReadTextFile returns the scripted content and records the path.
func (f *coaTestFS) ReadTextFile(ctx context.Context, path string) (string, error) {
	f.mu.Lock()
	f.reads = append(f.reads, path)
	f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	return f.content, nil
}

// WriteTextFile records the write.
func (f *coaTestFS) WriteTextFile(ctx context.Context, path, content string) error {
	f.mu.Lock()
	f.writes = append(f.writes, path+"="+content)
	f.mu.Unlock()
	return f.err
}

// readPaths returns the recorded read paths.
func (f *coaTestFS) readPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reads...)
}

// writePaths returns the recorded writes.
func (f *coaTestFS) writePaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.writes...)
}

// TestACPHandshake records the auth methods the agent offered and adopts the
// session id from session/new.
func TestACPHandshake(t *testing.T) {
	_, s, proto := coaTestStartACP(t, acpCopilot, StartRequest{}, coaTestACPAgent)

	if s.ID() != "ses_1" {
		t.Fatalf("session id: %q", s.ID())
	}
	proto.mu.Lock()
	methods := append([]string(nil), proto.authMethods...)
	httpMCP := proto.trustHTTPMCP
	proto.mu.Unlock()
	if !slices.Equal(methods, []string{"github-device"}) {
		t.Fatalf("auth methods: %v", methods)
	}
	if !httpMCP {
		t.Fatal("http mcp capability not recorded")
	}
}

// TestACPUpdates maps the ACP session/update notifications to events.
func TestACPUpdates(t *testing.T) {
	peer, s, _ := coaTestStartACP(t, acpCopilot, StartRequest{}, coaTestACPAgent)
	events := coaTestWatch(t, s.Events())

	peer.notify("session/update", coaTestUpdate("ses_1", map[string]any{
		"sessionUpdate": "agent_message_chunk",
		"content":       map[string]any{"type": "text", "text": "pong"},
	}))
	text := events.next(EventAssistant)
	if text.Text != "pong" || text.Delta {
		t.Fatalf("assistant event: %+v", text)
	}

	peer.notify("session/update", coaTestUpdate("ses_1", map[string]any{
		"sessionUpdate": "tool_call",
		"toolCallId":    "t1",
		"title":         "Edit file",
		"kind":          "edit",
		"status":        "in_progress",
		"rawInput":      map[string]any{"path": "/tmp/coa.txt"},
	}))
	started := events.next(EventTool)
	if started.Tool == nil {
		t.Fatalf("tool_call event: %+v", started)
	}
	if started.Tool.Kind != ToolEdit || started.Tool.Status != "started" {
		t.Fatalf("tool_call tool: %+v", started.Tool)
	}
	if !slices.Equal(started.Tool.Paths, []string{"/tmp/coa.txt"}) {
		t.Fatalf("tool_call paths: %v", started.Tool.Paths)
	}

	peer.notify("session/update", coaTestUpdate("ses_1", map[string]any{
		"sessionUpdate": "tool_call_update",
		"toolCallId":    "t1",
		"title":         "Edit file",
		"kind":          "edit",
		"status":        "failed",
		"content": []any{map[string]any{
			"type":    "content",
			"content": map[string]any{"type": "text", "text": "permission denied"},
		}},
	}))
	failed := events.next(EventTool)
	if failed.Tool == nil || failed.Tool.Status != "failed" {
		t.Fatalf("tool_call_update tool: %+v", failed.Tool)
	}
	if failed.Tool.Output != "permission denied" {
		t.Fatalf("tool_call_update output: %q", failed.Tool.Output)
	}

	peer.notify("session/update", coaTestUpdate("ses_1", map[string]any{
		"sessionUpdate": "usage_update",
		"used":          12,
		"size":          4,
	}))
	usage := events.next(EventUsage)
	if usage.Usage == nil || usage.Usage.Input != 12 || usage.Usage.Output != 4 {
		t.Fatalf("usage event: %+v", usage.Usage)
	}

	// A prompt for a different session is not this session's turn.
	peer.notify("session/update", coaTestUpdate("ses_other", map[string]any{
		"sessionUpdate": "agent_message_chunk",
		"content":       map[string]any{"type": "text", "text": "not mine"},
	}))
	peer.notify("session/update", coaTestUpdate("ses_1", map[string]any{
		"sessionUpdate": "agent_message_chunk",
		"content":       map[string]any{"type": "text", "text": "mine"},
	}))
	mine := events.next(EventAssistant)
	if mine.Text != "mine" {
		t.Fatalf("routed assistant event: %+v", mine)
	}
}

// TestACPPromptResults maps the session/prompt response to the terminal result
// and proves the result latch resets for the next turn.
func TestACPPromptResults(t *testing.T) {
	peer, s, proto := coaTestStartACP(t, acpCopilot, StartRequest{}, coaTestACPAgent)
	events := coaTestWatch(t, s.Events())
	peer.drain()

	frame, err := proto.EncodePrompt(Prompt{Text: "hi"})
	if err != nil {
		t.Fatalf("encode prompt: %v", err)
	}
	id := coaTestPromptID(t, frame, "session/prompt")
	peer.push(coaTestResponse(t, id, map[string]any{"stopReason": "end_turn"}))
	res := events.next(EventResult)
	if res.Result == nil || res.Result.IsError || res.Result.Subtype != "success" {
		t.Fatalf("end_turn result: %+v", res.Result)
	}

	// The latch holds while no new turn is written.
	if evs := proto.Exit(0, nil, ""); len(evs) != 0 {
		t.Fatalf("exit after a completed turn: %+v", evs)
	}

	// A fresh prompt resets the latch, so a crash in the next turn is
	// classified.
	frame, err = proto.EncodePrompt(Prompt{Text: "again"})
	if err != nil {
		t.Fatalf("encode prompt: %v", err)
	}
	coaTestPromptID(t, frame, "session/prompt")
	evs := proto.Exit(1, nil, "copilot died")
	if len(evs) != 1 || evs[0].Type != EventError {
		t.Fatalf("exit events: %+v", evs)
	}
	if !strings.HasPrefix(evs[0].Code, "copilot.") {
		t.Fatalf("exit code: %q", evs[0].Code)
	}

	// The simulated crash left the prompt open; clear it for the next turn.
	proto.mu.Lock()
	proto.promptID = ""
	proto.mu.Unlock()

	// A refused stop reason is an error result.
	id = coaTestPromptID(t, coaTestMustEncode(t, proto, "third"), "session/prompt")
	peer.push(coaTestResponse(t, id, map[string]any{"stopReason": "refused"}))
	refused := events.next(EventResult)
	if refused.Result == nil || !refused.Result.IsError {
		t.Fatalf("refused result: %+v", refused.Result)
	}
	if refused.Result.EndReason == "" || !strings.HasPrefix(refused.Result.Code, "copilot.") {
		t.Fatalf("refused classification: %+v", refused.Result)
	}
}

// coaTestMustEncode encodes one prompt and fails the test on an error.
func coaTestMustEncode(t *testing.T, proto *acpProtocol, text string) []byte {
	t.Helper()
	frame, err := proto.EncodePrompt(Prompt{Text: text})
	if err != nil {
		t.Fatalf("encode prompt: %v", err)
	}
	return frame
}

// TestACPPermissionDecisions answers session/request_permission by hand and
// echoes the id type the agent used.
func TestACPPermissionDecisions(t *testing.T) {
	peer, s, _ := coaTestStartACP(t, acpCopilot, StartRequest{}, coaTestACPAgent)
	events := coaTestWatch(t, s.Events())
	peer.drain()

	peer.request(5, "session/request_permission", map[string]any{
		"sessionId": "ses_1",
		"toolCall":  map[string]any{"title": "Run tests", "kind": "execute"},
		"options": []any{
			map[string]any{"optionId": "a1", "kind": "allow_once"},
			map[string]any{"optionId": "r1", "kind": "reject_once"},
		},
	})
	perm := events.next(EventPermission)
	if perm.Permission == nil {
		t.Fatalf("permission event: %+v", perm)
	}
	if perm.Permission.ID != "5" {
		t.Fatalf("permission id: %q", perm.Permission.ID)
	}
	if perm.Permission.Kind != ToolExec {
		t.Fatalf("permission kind: %q", perm.Permission.Kind)
	}
	if err := s.AnswerPermission(context.Background(), perm.Permission.ID, Decision{Allow: true}); err != nil {
		t.Fatalf("answer permission: %v", err)
	}
	allow := peer.waitReply()
	var allowed struct {
		ID     json.Number `json:"id"`
		Result struct {
			Outcome struct {
				Outcome  string `json:"outcome"`
				OptionID string `json:"optionId"`
			} `json:"outcome"`
		} `json:"result"`
	}
	if err := json.Unmarshal(allow.raw, &allowed); err != nil {
		t.Fatalf("allow frame: %s (%v)", allow.raw, err)
	}
	if allowed.ID.String() != "5" {
		t.Fatalf("allow id: %q", allowed.ID)
	}
	if allowed.Result.Outcome.Outcome != "selected" || allowed.Result.Outcome.OptionID != "a1" {
		t.Fatalf("allow frame: %s", allow.raw)
	}

	// A string id stays a string in the reply.
	peer.request("p9", "session/request_permission", map[string]any{
		"sessionId": "ses_1",
		"toolCall":  map[string]any{"title": "Edit file", "kind": "edit"},
		"options": []any{
			map[string]any{"optionId": "a9", "kind": "allow_once"},
			map[string]any{"optionId": "r9", "kind": "reject_once"},
		},
	})
	perm = events.next(EventPermission)
	if perm.Permission == nil || perm.Permission.ID != `"p9"` {
		t.Fatalf("string permission id: %+v", perm.Permission)
	}
	if err := s.AnswerPermission(context.Background(), perm.Permission.ID, Decision{Allow: false}); err != nil {
		t.Fatalf("answer permission: %v", err)
	}
	deny := peer.waitReply()
	var denied struct {
		ID     json.RawMessage `json:"id"`
		Result struct {
			Outcome struct {
				Outcome  string `json:"outcome"`
				OptionID string `json:"optionId"`
			} `json:"outcome"`
		} `json:"result"`
	}
	if err := json.Unmarshal(deny.raw, &denied); err != nil {
		t.Fatalf("deny frame: %s (%v)", deny.raw, err)
	}
	if string(denied.ID) != `"p9"` {
		t.Fatalf("deny id: %s", denied.ID)
	}
	if denied.Result.Outcome.Outcome != "selected" || denied.Result.Outcome.OptionID != "r9" {
		t.Fatalf("deny frame: %s", deny.raw)
	}
}

// TestACPAutoPermission proves the auto policy answers the request in the
// library: a selected outcome on the wire and no permission event.
func TestACPAutoPermission(t *testing.T) {
	req := StartRequest{Permissions: PermissionPolicy{Mode: PermissionAuto}}
	peer, s, _ := coaTestStartACP(t, acpCopilot, req, coaTestACPAgent)
	events := coaTestWatch(t, s.Events())
	events.next(EventInit)
	peer.drain()

	peer.request(6, "session/request_permission", map[string]any{
		"sessionId": "ses_1",
		"toolCall":  map[string]any{"title": "Run tests", "kind": "execute"},
		"options": []any{
			map[string]any{"optionId": "a1", "kind": "allow_once"},
			map[string]any{"optionId": "r1", "kind": "reject_once"},
		},
	})
	peer.notify("session/update", coaTestUpdate("ses_1", map[string]any{
		"sessionUpdate": "usage_update",
		"used":          7,
		"size":          2,
	}))
	// The marker arrives first only when no permission event was emitted.
	events.next(EventUsage)
	if len(events.buf) != 0 {
		t.Fatalf("unexpected events before the marker: %+v", events.buf)
	}

	reply := peer.waitReply()
	var decoded struct {
		ID     json.Number `json:"id"`
		Result struct {
			Outcome struct {
				Outcome  string `json:"outcome"`
				OptionID string `json:"optionId"`
			} `json:"outcome"`
		} `json:"result"`
	}
	if err := json.Unmarshal(reply.raw, &decoded); err != nil {
		t.Fatalf("auto frame: %s (%v)", reply.raw, err)
	}
	if decoded.ID.String() != "6" {
		t.Fatalf("auto id: %q", decoded.ID)
	}
	if decoded.Result.Outcome.Outcome != "selected" || decoded.Result.Outcome.OptionID != "a1" {
		t.Fatalf("auto frame: %s", reply.raw)
	}
}

// TestACPFileSystem routes fs calls to the consumer when it set a FileSystem,
// and refuses them when it did not.
func TestACPFileSystem(t *testing.T) {
	fs := &coaTestFS{content: "file body"}
	peer, _, _ := coaTestStartACP(t, acpCopilot, StartRequest{FS: fs}, coaTestACPAgent)
	peer.drain()

	peer.request(3, "fs/read_text_file", map[string]any{"sessionId": "ses_1", "path": "/tmp/coa.txt"})
	read := peer.waitReply()
	var got struct {
		ID     json.Number `json:"id"`
		Result struct {
			Content string `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(read.raw, &got); err != nil {
		t.Fatalf("read frame: %s (%v)", read.raw, err)
	}
	if got.Result.Content != "file body" {
		t.Fatalf("read frame: %s", read.raw)
	}
	if paths := fs.readPaths(); !slices.Equal(paths, []string{"/tmp/coa.txt"}) {
		t.Fatalf("read paths: %v", paths)
	}

	peer.request(4, "fs/write_text_file", map[string]any{
		"sessionId": "ses_1", "path": "/tmp/coa.txt", "content": "new body",
	})
	write := peer.waitReply()
	if !write.has("result") {
		t.Fatalf("write frame: %s", write.raw)
	}
	if paths := fs.writePaths(); !slices.Equal(paths, []string{"/tmp/coa.txt=new body"}) {
		t.Fatalf("write paths: %v", paths)
	}

	// Without a FileSystem the call is refused.
	noFS, _, _ := coaTestStartACP(t, acpCopilot, StartRequest{}, coaTestACPAgent)
	noFS.drain()
	noFS.request(5, "fs/read_text_file", map[string]any{"sessionId": "ses_1", "path": "/tmp/coa.txt"})
	refused := noFS.waitReply()
	if !refused.has("error") {
		t.Fatalf("file system refusal: %s", refused.raw)
	}
}

// TestACPUnknownClientMethod proves an unanswered agent request gets a valid
// JSON-RPC error reply, even when the id is a string.
func TestACPUnknownClientMethod(t *testing.T) {
	peer, _, _ := coaTestStartACP(t, acpCopilot, StartRequest{}, coaTestACPAgent)
	peer.drain()

	peer.request("abc", "session/not_a_client_method", map[string]any{"sessionId": "ses_1"})
	reply := peer.wait(func(f coaTestFrame) bool { return !f.has("method") })
	if !json.Valid(reply.raw) {
		t.Fatalf("refusal frame is not valid JSON: %s", reply.raw)
	}
	var decoded struct {
		ID    json.RawMessage `json:"id"`
		Error map[string]any  `json:"error"`
	}
	if err := json.Unmarshal(reply.raw, &decoded); err != nil {
		t.Fatalf("refusal frame: %s (%v)", reply.raw, err)
	}
	if string(decoded.ID) != `"abc"` {
		t.Fatalf("refusal id: %s", decoded.ID)
	}
	if decoded.Error == nil {
		t.Fatalf("refusal frame: %s", reply.raw)
	}
}

// TestCursorLastIdentifier pins the chat id picker of `cursor-agent
// create-chat`.
func TestCursorLastIdentifier(t *testing.T) {
	const id = "0f8f8a8e-1234-4abc-9def-0123456789ab"
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"uuid line", id + "\n", id},
		{"created chat line", "created chat " + id + "\n", id},
		{"quoted uuid", `chat id: "` + id + `"` + "\n", id},
		{"bare last line", "creating chat\nmy chat id\n", "my chat id"},
		{"no uuid", "no identifier here\n", "no identifier here"},
		{"empty input", "", ""},
		{"blank lines", "  \n\n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := lastIdentifier(tc.in); got != tc.want {
				t.Fatalf("lastIdentifier(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// acpTestFixtureFrame returns the frame with the wanted result id from a
// recorded ACP fixture.
func acpTestFixtureFrame(t *testing.T, name string, id string) map[string]any {
	t.Helper()
	b, err := os.ReadFile("testdata/acp/" + name)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var m map[string]any
		dec := json.NewDecoder(strings.NewReader(line))
		dec.UseNumber()
		if dec.Decode(&m) != nil {
			continue
		}
		if number, _ := m["id"].(json.Number); number.String() == id {
			return m
		}
	}
	t.Fatalf("fixture %s has no frame with id %s", name, id)
	return nil
}

// TestACPConfigOptionUpdate proves a config_option_update notification changes
// the model the session reports.
func TestACPConfigOptionUpdate(t *testing.T) {
	newReply := mdTestFixture(t, "omp_session_new.json")
	handler := func(p *coaTestPeer, f coaTestFrame) {
		switch f.method() {
		case "session/new":
			p.reply(f.m["id"], json.RawMessage(newReply))
		default:
			coaTestACPAgent(p, f)
		}
	}
	peer, ws, proto := coaTestStartACP(t, acpOMP, StartRequest{}, handler)
	s := &acpSession{Session: ws, p: proto}
	events := coaTestWatch(t, s.Events())
	events.next(EventInit)

	var update map[string]any
	if err := json.Unmarshal(newReply, &update); err != nil {
		t.Fatal(err)
	}
	options := update["configOptions"].([]any)
	for _, raw := range options {
		opt := raw.(map[string]any)
		if opt["id"] == "model" {
			opt["currentValue"] = "opencode-go/beam-free"
		}
	}
	peer.notify("session/update", coaTestUpdate("ses_omp_1", map[string]any{
		"sessionUpdate": "config_option_update",
		"configOptions": options,
	}))
	events.next(EventStatus)
	if got := mdTestDefault(s.Models()); got != "opencode-go/beam-free" {
		t.Fatalf("current model after config_option_update = %q", got)
	}
}

// TestACPSteer drives steering through _session/steering and proves the
// refusal holds the prompt.
func TestACPSteer(t *testing.T) {
	refuse := false
	handler := func(p *coaTestPeer, f coaTestFrame) {
		switch f.method() {
		case "initialize":
			p.reply(f.m["id"], map[string]any{
				"protocolVersion": 1,
				"authMethods":     []any{map[string]any{"id": "agent"}},
				"_meta":           map[string]any{"steering": map[string]any{"supported": true}},
			})
		case "session/new":
			p.reply(f.m["id"], map[string]any{"sessionId": "ses_1"})
		case "_session/steering":
			outcome := "injected"
			if refuse {
				outcome = "promptRequired"
			}
			p.reply(f.m["id"], map[string]any{"outcome": outcome})
		}
	}
	peer, ws, proto := coaTestStartACP(t, acpOMP, StartRequest{}, handler)
	coaTestWatch(t, ws.Events()).next(EventInit)

	// No turn runs.
	if err := proto.Steer(context.Background(), Prompt{Text: "idle"}); !errors.Is(err, ErrNoActiveTurn) {
		t.Fatalf("steer with no turn: %v", err)
	}

	if _, err := proto.EncodePrompt(Prompt{Text: "run"}); err != nil {
		t.Fatal(err)
	}
	if err := proto.Steer(context.Background(), Prompt{Text: "also this"}); err != nil {
		t.Fatalf("steer during a turn: %v", err)
	}
	call := peer.waitMethod("_session/steering")
	params := call.params()
	if params["sessionId"] != "ses_1" {
		t.Fatalf("steering params: %s", call.raw)
	}
	prompt, _ := params["prompt"].([]any)
	if len(prompt) != 1 || prompt[0].(map[string]any)["text"] != "also this" {
		t.Fatalf("steering prompt: %s", call.raw)
	}

	// A vendor refusal holds the prompt.
	refuse = true
	if err := proto.Steer(context.Background(), Prompt{Text: "late"}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("refused steer: %v", err)
	}
	s := &acpSession{Session: ws, p: proto}
	if d, err := s.Send(context.Background(), Prompt{Text: "held"}); err != nil || d != DeliveryHeld {
		t.Fatalf("send after a refused steer: %q %v, want held", d, err)
	}
}

// TestACPSessionMeta proves the per-kind _meta block reaches only the kinds
// that fill it.
func TestACPSessionMeta(t *testing.T) {
	requireAuto := true
	req := StartRequest{
		Instructions: "the rules",
		Permissions:  PermissionPolicy{Mode: PermissionAuto},
		Raw:          map[string]any{"grok_auto_mode": requireAuto},
	}
	proto := newACPProtocol(coaTestRuntime(), acpGrok, req, &launch{instructions: "the rules"}, false, false)
	params := proto.sessionParams()
	meta, _ := params["_meta"].(map[string]any)
	if meta["rules"] != "the rules" || meta["yoloMode"] != true || meta["autoMode"] != true {
		t.Fatalf("grok session _meta: %+v", meta)
	}
	if proto.instructions != "" || proto.rules != "the rules" {
		t.Fatalf("grok instructions: instructions=%q rules=%q", proto.instructions, proto.rules)
	}

	omp := newACPProtocol(coaTestRuntime(), acpOMP, StartRequest{Instructions: "the rules"}, &launch{instructions: "the rules"}, false, false)
	if _, ok := omp.sessionParams()["_meta"]; ok {
		t.Fatalf("omp session params carry _meta: %+v", omp.sessionParams())
	}

	// autoMode is opt-in, and yoloMode follows the policy.
	proto = newACPProtocol(coaTestRuntime(), acpGrok, StartRequest{Instructions: "r"}, &launch{instructions: "r"}, false, false)
	meta, _ = proto.sessionParams()["_meta"].(map[string]any)
	if meta["yoloMode"] != nil || meta["autoMode"] != nil {
		t.Fatalf("grok _meta without auto: %+v", meta)
	}
}

// TestACPAuthRequired proves the grok login refusal becomes a not_logged_in
// event without an authenticate call.
func TestACPAuthRequired(t *testing.T) {
	init := acpTestFixtureFrame(t, "grok/handshake.ndjson", "1")
	authErr := acpTestFixtureFrame(t, "grok/auth_required.ndjson", "2")
	authenticates := 0
	handler := func(p *coaTestPeer, f coaTestFrame) {
		switch f.method() {
		case "initialize":
			p.reply(f.m["id"], init["result"])
		case "session/new":
			p.push(map[string]any{"jsonrpc": "2.0", "id": f.m["id"], "error": authErr["error"]})
		case "authenticate":
			authenticates++
		}
	}
	peer := coaTestNewPeer(t, handler)
	proto := newACPProtocol(coaTestRuntime(), acpGrok, StartRequest{HandshakeTimeout: 2 * time.Second}, &launch{}, false, false)
	proto.client.SetWrite(func(b []byte) error {
		_, err := peer.inW.Write(b)
		return err
	})
	go func() {
		sc := bufio.NewScanner(peer.out)
		for sc.Scan() {
			proto.client.Handle(sc.Bytes())
		}
	}()
	if _, err := proto.initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	events := proto.openOrLoad(context.Background())
	if len(events) != 1 || events[0].Type != EventError {
		t.Fatalf("openOrLoad events: %+v", events)
	}
	if events[0].Code != "grok.not_logged_in" || events[0].EndReason != "auth" {
		t.Fatalf("auth error: %+v", events[0])
	}
	if authenticates != 0 {
		t.Fatalf("authenticate calls = %d, want 0", authenticates)
	}
}

// acpTestFixtureLines returns every frame of a recorded ACP fixture.
func acpTestFixtureLines(t *testing.T, name string) []map[string]any {
	t.Helper()
	b, err := os.ReadFile("testdata/acp/" + name)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		dec := json.NewDecoder(strings.NewReader(line))
		dec.UseNumber()
		var m map[string]any
		if dec.Decode(&m) != nil {
			continue
		}
		out = append(out, m)
	}
	return out
}

// acpTestResultOf returns the result of the first frame whose id matches.
func acpTestResultOf(t *testing.T, frames []map[string]any, id string) map[string]any {
	t.Helper()
	for _, f := range frames {
		if number, _ := f["id"].(json.Number); number.String() == id {
			result, _ := f["result"].(map[string]any)
			return result
		}
	}
	t.Fatalf("no frame with id %s", id)
	return nil
}

// acpTestStartReplay opens a session whose handshake answers come from a
// recorded fixture and returns a func that pushes the other frames. A method in
// refuse gets an error, so the driver takes its fallback. The recorded turn
// result takes the id of the live prompt.
func acpTestStartReplay(t *testing.T, kind acpKind, req StartRequest, name string, refuse ...string) (*coaTestPeer, *acpSession, *coaTestEvents, func()) {
	t.Helper()
	frames := acpTestFixtureLines(t, name)
	prompts := make(chan any, 4)
	handler := func(p *coaTestPeer, f coaTestFrame) {
		switch {
		case slices.Contains(refuse, f.method()):
			p.replyErr(f.m["id"], -32601, "refused by the test")
		case f.method() == "initialize":
			p.reply(f.m["id"], acpTestResultOf(t, frames, "1"))
		case f.method() == "session/new" || f.method() == "session/resume" || f.method() == "session/load":
			p.reply(f.m["id"], acpTestResultOf(t, frames, "2"))
		case f.method() == "session/prompt":
			prompts <- f.m["id"]
		default:
			coaTestACPAgent(p, f)
		}
	}
	peer, ws, proto := coaTestStartACP(t, kind, req, handler, req.SessionID != "")
	s := &acpSession{Session: ws, p: proto}
	events := coaTestWatch(t, s.Events())
	events.next(EventInit)
	replay := func() {
		var promptID any
		select {
		case promptID = <-prompts:
		case <-time.After(200 * time.Millisecond):
		}
		for _, f := range frames {
			if id, _ := f["id"].(json.Number); id.String() == "1" || id.String() == "2" {
				continue
			}
			if res, ok := f["result"].(map[string]any); ok && res["stopReason"] != nil && promptID != nil {
				f["id"] = promptID
			}
			peer.push(f)
		}
	}
	return peer, s, events, replay
}

// acpTestTurn collects the assistant text and the tool events up to the turn
// result.
func acpTestTurn(t *testing.T, events *coaTestEvents) (string, []ToolEvent, Event) {
	t.Helper()
	var text strings.Builder
	var tools []ToolEvent
	for {
		ev := events.nextWhere(func(ev Event) bool {
			return ev.Type == EventAssistant || ev.Type == EventTool || ev.Type == EventResult || ev.Type == EventError
		})
		switch ev.Type {
		case EventAssistant:
			text.WriteString(ev.Text)
		case EventTool:
			if ev.Tool != nil {
				tools = append(tools, *ev.Tool)
			}
		default:
			return text.String(), tools, ev
		}
	}
}

// TestOMPSession replays a recorded omp turn: init, assistant text, tool call,
// usage and the success result.
func TestOMPSession(t *testing.T) {
	_, s, events, replay := acpTestStartReplay(t, acpOMP, StartRequest{}, "omp/turn_tool.ndjson")
	if err := s.Prompt(context.Background(), Prompt{Text: "run"}); err != nil {
		t.Fatal(err)
	}
	replay()
	var text strings.Builder
	sawTool, sawExec := false, false
	for {
		ev := events.nextWhere(func(ev Event) bool {
			return ev.Type == EventAssistant || ev.Type == EventTool || ev.Type == EventUsage || ev.Type == EventResult
		})
		switch ev.Type {
		case EventAssistant:
			text.WriteString(ev.Text)
		case EventTool:
			if ev.Tool == nil {
				t.Fatal("tool event without a tool")
			}
			if ev.Tool.Kind == ToolExec {
				sawExec = true
			}
			if ev.Tool.Status == "completed" {
				sawTool = true
			}
		case EventUsage:
			if ev.Usage == nil || ev.Usage.Input == 0 {
				t.Fatalf("usage event: %+v", ev.Usage)
			}
		case EventResult:
			if ev.Result == nil || ev.Result.IsError || ev.Result.Subtype != "success" {
				t.Fatalf("result: %+v", ev.Result)
			}
			if !strings.Contains(text.String(), "SPIKE-SHELL") {
				t.Fatalf("assistant text: %q", text.String())
			}
			if !sawTool || !sawExec {
				t.Fatalf("tool events: completed=%v exec=%v", sawTool, sawExec)
			}
			return
		}
	}
}

// TestOMPPermission replays the first recorded permission turn under each
// policy.
func TestOMPPermission(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mode       PermissionMode
		wantEvents int
	}{
		{"ask", PermissionAsk, 1},
		{"auto_edit", PermissionAutoEdit, 1},
		{"auto", PermissionAuto, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := StartRequest{Permissions: PermissionPolicy{Mode: tc.mode}}
			frames := acpTestFixtureLines(t, "omp/permission.ndjson")
			// Keep the first turn: everything through its prompt result.
			var first []map[string]any
			for _, f := range frames[2:] {
				first = append(first, f)
				if number, _ := f["id"].(json.Number); number.String() == "3" {
					break
				}
			}
			handler := func(p *coaTestPeer, f coaTestFrame) {
				switch f.method() {
				case "initialize":
					p.reply(f.m["id"], acpTestResultOf(t, frames, "1"))
				case "session/new":
					p.reply(f.m["id"], acpTestResultOf(t, frames, "2"))
				default:
					coaTestACPAgent(p, f)
				}
			}
			peer, ws, proto := coaTestStartACP(t, acpOMP, req, handler)
			s := &acpSession{Session: ws, p: proto}
			events := coaTestWatch(t, s.Events())
			events.next(EventInit)
			if err := s.Prompt(context.Background(), Prompt{Text: "run"}); err != nil {
				t.Fatal(err)
			}
			for _, f := range first {
				peer.push(f)
			}
			seen := 0
			for {
				ev := events.nextWhere(func(ev Event) bool {
					return ev.Type == EventPermission || ev.Type == EventResult
				})
				if ev.Type == EventResult {
					break
				}
				seen++
				if tc.mode == PermissionAuto {
					t.Fatalf("permission event under auto: %+v", ev.Permission)
				}
				if err := s.AnswerPermission(context.Background(), ev.Permission.ID, Decision{Allow: true}); err != nil {
					t.Fatal(err)
				}
			}
			if seen != tc.wantEvents {
				t.Fatalf("permission events = %d, want %d", seen, tc.wantEvents)
			}
		})
	}
}

// TestOMPResume replays the recorded resume handshake and recall turn.
func TestOMPResume(t *testing.T) {
	_, s, _, replay := acpTestStartReplay(t, acpOMP, StartRequest{SessionID: "ses_omp_1"}, "omp/resume.ndjson")
	if err := s.Prompt(context.Background(), Prompt{Text: "recall"}); err != nil {
		t.Fatal(err)
	}
	replay()
}

// TestOMPCancel replays the recorded cancel turn.
func TestOMPCancel(t *testing.T) {
	_, s, events, replay := acpTestStartReplay(t, acpOMP, StartRequest{}, "omp/cancel.ndjson")
	if err := s.Prompt(context.Background(), Prompt{Text: "run"}); err != nil {
		t.Fatal(err)
	}
	_ = s.Interrupt(context.Background())
	replay()
	res := events.next(EventResult)
	if res.Result == nil || res.Result.Subtype != "cancelled" {
		t.Fatalf("cancel result: %+v", res.Result)
	}
}

// TestOMPSetModel replays the recorded config option change.
func TestOMPSetModel(t *testing.T) {
	_, s, _, replay := acpTestStartReplay(t, acpOMP, StartRequest{}, "omp/set_model.ndjson")
	done := make(chan error, 1)
	go func() { done <- s.SetModel(context.Background(), "opencode-go/beam-free") }()
	replay()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// TestOMPModels reads the provider/model ids and the default from the recorded
// session/new.
func TestOMPModels(t *testing.T) {
	newReply := mdTestFixture(t, "omp_session_new.json")
	handler := func(p *coaTestPeer, f coaTestFrame) {
		switch f.method() {
		case "session/new":
			p.reply(f.m["id"], json.RawMessage(newReply))
		default:
			coaTestACPAgent(p, f)
		}
	}
	_, ws, proto := coaTestStartACP(t, acpOMP, StartRequest{}, handler)
	s := &acpSession{Session: ws, p: proto}
	coaTestWatch(t, s.Events()).next(EventInit)
	models := s.Models()
	if len(models) != 3 || models[0].ID != "opencode-go/deepseek-v4.1-flash" || !models[0].Default {
		t.Fatalf("omp models: %+v", models)
	}
}

// TestOMPAuthProbe covers the omp models --json parser.
func TestOMPAuthProbe(t *testing.T) {
	parse := authProbes[OMP].parse
	cases := []struct {
		out   string
		err   error
		state AuthState
	}{
		{`{"models":[{"id":"x"}]}`, nil, AuthOK},
		{`{"models":[]}`, nil, AuthMissing},
		{"not json", nil, AuthUnknown},
	}
	for _, tc := range cases {
		state, _, _ := parse(tc.out, tc.err)
		if state != tc.state {
			t.Fatalf("parse(%q) = %q, want %q", tc.out, state, tc.state)
		}
	}
}

// TestGrokSessionMeta covers the grok session _meta fields.
func TestGrokSessionMeta(t *testing.T) {
	req := StartRequest{Instructions: "rules", Permissions: PermissionPolicy{Mode: PermissionAuto}, Raw: map[string]any{"grok_auto_mode": true}}
	proto := newACPProtocol(coaTestRuntime(), acpGrok, req, &launch{instructions: "rules"}, false, false)
	meta, _ := proto.sessionParams()["_meta"].(map[string]any)
	if meta["rules"] != "rules" || meta["yoloMode"] != true || meta["autoMode"] != true {
		t.Fatalf("session _meta: %+v", meta)
	}
}

// TestGrokModelsFromInitialize proves the model list comes from initialize
// without a session/new.
func TestGrokModelsFromInitialize(t *testing.T) {
	var init map[string]any
	if err := json.Unmarshal(mdTestFixture(t, "grok_initialize.json"), &init); err != nil {
		t.Fatal(err)
	}
	sessionOpens := 0
	handler := func(p *coaTestPeer, f coaTestFrame) {
		switch f.method() {
		case "initialize":
			p.reply(f.m["id"], init)
		case "session/new":
			sessionOpens++
		}
	}
	peer := coaTestNewPeer(t, handler)
	proto := newACPProtocol(coaTestRuntime(), acpGrok, StartRequest{HandshakeTimeout: 2 * time.Second}, &launch{}, false, false)
	proto.client.SetWrite(func(b []byte) error { _, err := peer.inW.Write(b); return err })
	go func() {
		sc := bufio.NewScanner(peer.out)
		for sc.Scan() {
			proto.client.Handle(sc.Bytes())
		}
	}()
	if _, err := proto.initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !proto.offersModels() {
		t.Fatal("initialize offers no models")
	}
	models := proto.sessionModels()
	if len(models) != 2 || models[0].ID != "grok-4.7" || !models[0].Default {
		t.Fatalf("grok models: %+v", models)
	}
	if models[0].Description == "" {
		t.Fatalf("model description is empty: %+v", models[0])
	}
	if sessionOpens != 0 {
		t.Fatalf("session/new calls = %d, want 0", sessionOpens)
	}
}

// TestGrokSession replays the recorded grok turn.
func TestGrokSession(t *testing.T) {
	_, s, events, replay := acpTestStartReplay(t, acpGrok, StartRequest{}, "grok/turn_tool.ndjson")
	if err := s.Prompt(context.Background(), Prompt{Text: "run"}); err != nil {
		t.Fatal(err)
	}
	replay()
	var text strings.Builder
	for {
		ev := events.nextWhere(func(ev Event) bool {
			return ev.Type == EventAssistant || ev.Type == EventResult || ev.Type == EventUsage
		})
		switch ev.Type {
		case EventAssistant:
			text.WriteString(ev.Text)
		case EventUsage:
			if ev.Usage == nil || ev.Usage.Input == 0 {
				t.Fatalf("usage: %+v", ev.Usage)
			}
		case EventResult:
			if ev.Result == nil || ev.Result.Subtype != "success" {
				t.Fatalf("result: %+v", ev.Result)
			}
			if !strings.Contains(text.String(), "GROK-SHELL") && !strings.Contains(text.String(), "GROK-PERM") {
				t.Fatalf("assistant text: %q", text.String())
			}
			return
		}
	}
}

// TestGrokPermission replays the recorded ask-mode turn: grok 1.0.70 runs the
// tool and never sends session/request_permission, so no permission event may
// arrive before the result (see docs/compat.md).
func TestGrokPermission(t *testing.T) {
	_, s, events, replay := acpTestStartReplay(t, acpGrok, StartRequest{Permissions: PermissionPolicy{Mode: PermissionAsk}}, "grok/permission.ndjson")
	if err := s.Prompt(context.Background(), Prompt{Text: "run"}); err != nil {
		t.Fatal(err)
	}
	replay()
	ev := events.nextWhere(func(ev Event) bool { return ev.Type == EventPermission || ev.Type == EventResult })
	if ev.Type == EventPermission {
		t.Fatalf("grok sent a permission request: %+v", ev.Permission)
	}
	if ev.Result == nil || ev.Result.Subtype != "success" {
		t.Fatalf("result: %+v", ev.Result)
	}
}

// TestGrokSteerHolds proves a grok session holds mid-turn input.
func TestGrokSteerHolds(t *testing.T) {
	handler := func(p *coaTestPeer, f coaTestFrame) {
		switch f.method() {
		case "initialize":
			p.reply(f.m["id"], map[string]any{"protocolVersion": 1, "authMethods": []any{}})
		case "session/new":
			p.reply(f.m["id"], map[string]any{"sessionId": "ses_1"})
		}
	}
	_, ws, proto := coaTestStartACP(t, acpGrok, StartRequest{}, handler)
	s := &acpSession{Session: ws, p: proto}
	coaTestWatch(t, s.Events()).next(EventInit)
	if err := s.Prompt(context.Background(), Prompt{Text: "run"}); err != nil {
		t.Fatal(err)
	}
	d, err := s.Send(context.Background(), Prompt{Text: "late"})
	if err != nil || d != DeliveryHeld {
		t.Fatalf("send during a turn: %q %v, want held", d, err)
	}
}

// TestGrokAuthProbe covers the grok models parser.
func TestGrokAuthProbe(t *testing.T) {
	parse := authProbes[Grok].parse
	cases := []struct {
		out   string
		state AuthState
	}{
		{"You are not authenticated.\n\nAvailable models:", AuthMissing},
		{"You are logged in with grok.com.\n\nAvailable models:", AuthOK},
		{"You are not logged in.", AuthMissing},
		{"something else", AuthUnknown},
	}
	for _, tc := range cases {
		state, _, _ := parse(tc.out, nil)
		if state != tc.state {
			t.Fatalf("parse(%q) = %q, want %q", tc.out, state, tc.state)
		}
	}
}

// TestACPSessionArgsDropsPaneFlags proves the pane session flags do not reach
// the ACP subcommand. The wire owns the session id.
func TestACPSessionArgsDropsPaneFlags(t *testing.T) {
	got := stripACPSessionArgs([]string{"--session-id", "u", "-s", "v", "--resume=w", "--continue", "-c", "-m", "x", "--resume", "--model", "y", "-r"})
	want := []string{"-m", "x", "--model", "y"}
	if !slices.Equal(got, want) {
		t.Fatalf("stripACPSessionArgs = %v, want %v", got, want)
	}
}

// TestACPStartIgnoresPaneSessionID proves a fresh Start opens session/new even
// when the caller carries a session id minted for the pane.
func TestACPStartIgnoresPaneSessionID(t *testing.T) {
	var methods []string
	handler := func(p *coaTestPeer, f coaTestFrame) {
		methods = append(methods, f.method())
		switch f.method() {
		case "initialize":
			p.reply(f.m["id"], map[string]any{"protocolVersion": 1, "authMethods": []any{}})
		case "session/new":
			p.reply(f.m["id"], map[string]any{"sessionId": "ses_new"})
		}
	}
	_, s, _ := coaTestStartACP(t, acpGrok, StartRequest{SessionID: "pane-id"}, handler)
	coaTestWatch(t, s.Events()).next(EventInit)
	for _, m := range methods {
		if m == "session/resume" || m == "session/load" {
			t.Fatalf("fresh Start sent %s", m)
		}
	}
	if s.ID() != "ses_new" {
		t.Fatalf("session id = %q, want ses_new", s.ID())
	}
}

// TestOMPHandshake replays the recorded handshake: the model list comes from
// the config options, and a failed MCP server stays assistant text.
func TestOMPHandshake(t *testing.T) {
	_, s, events, replay := acpTestStartReplay(t, acpOMP, StartRequest{}, "omp/handshake.ndjson")
	if models := s.Models(); len(models) == 0 {
		t.Fatal("handshake offers no models")
	}
	replay()
	ev := events.next(EventAssistant)
	if !strings.Contains(ev.Text, "failed to connect") {
		t.Fatalf("MCP warning text: %q", ev.Text)
	}
}

// TestOMPLoad replays a resume that falls back to session/load.
func TestOMPLoad(t *testing.T) {
	peer, s, events, replay := acpTestStartReplay(t, acpOMP, StartRequest{SessionID: "ses_omp_15"}, "omp/load.ndjson", "session/resume")
	peer.waitMethod("session/load")
	if err := s.Prompt(context.Background(), Prompt{Text: "recall"}); err != nil {
		t.Fatal(err)
	}
	replay()
	text, _, res := acpTestTurn(t, events)
	if res.Type != EventResult || res.Result.Subtype != "success" || !strings.Contains(text, "PELICAN-77") {
		t.Fatalf("load turn: %q %+v", text, res)
	}
}

// TestOMPSteer replays the recorded steering frames. The first steer gets the
// promptRequired refusal, the second joins the running turn.
func TestOMPSteer(t *testing.T) {
	frames := acpTestFixtureLines(t, "omp/steer.ndjson")
	var turn []map[string]any
	inTurn := false
	for _, f := range frames {
		id, _ := f["id"].(json.Number)
		if id.String() == "5" {
			inTurn = true
			continue
		}
		if inTurn {
			turn = append(turn, f)
		}
		if inTurn && id.String() == "4" {
			break
		}
	}
	prompts := make(chan any, 1)
	steers := 0
	handler := func(p *coaTestPeer, f coaTestFrame) {
		switch f.method() {
		case "initialize":
			p.reply(f.m["id"], acpTestResultOf(t, frames, "1"))
		case "session/new":
			p.reply(f.m["id"], acpTestResultOf(t, frames, "2"))
		case "session/prompt":
			prompts <- f.m["id"]
		case "_session/steering":
			steers++
			if steers == 1 {
				p.reply(f.m["id"], acpTestResultOf(t, frames, "3"))
				return
			}
			p.reply(f.m["id"], acpTestResultOf(t, frames, "5"))
			promptID := <-prompts
			for _, tf := range turn {
				if id, _ := tf["id"].(json.Number); id.String() == "4" {
					tf["id"] = promptID
				}
				p.push(tf)
			}
		}
	}
	_, ws, proto := coaTestStartACP(t, acpOMP, StartRequest{}, handler)
	s := &acpSession{Session: ws, p: proto}
	events := coaTestWatch(t, s.Events())
	events.next(EventInit)
	if err := s.Prompt(context.Background(), Prompt{Text: "run a slow shell command"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Steer(context.Background(), Prompt{Text: "early"}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("refused steer: %v, want ErrUnsupported", err)
	}
	if err := s.Steer(context.Background(), Prompt{Text: "STEER-BANANA"}); err != nil {
		t.Fatalf("steer: %v", err)
	}
	text, tools, res := acpTestTurn(t, events)
	if res.Type != EventResult || res.Result.Subtype != "success" || !strings.Contains(text, "STEER-BANANA") || len(tools) == 0 {
		t.Fatalf("steered turn: %q tools=%d %+v", text, len(tools), res)
	}
}

// TestGrokCancel replays the recorded cancel turn.
func TestGrokCancel(t *testing.T) {
	_, s, events, replay := acpTestStartReplay(t, acpGrok, StartRequest{}, "grok/cancel.ndjson")
	if err := s.Prompt(context.Background(), Prompt{Text: "run"}); err != nil {
		t.Fatal(err)
	}
	_ = s.Interrupt(context.Background())
	replay()
	res := events.next(EventResult)
	if res.Result == nil || res.Result.Subtype != "cancelled" {
		t.Fatalf("cancel result: %+v", res.Result)
	}
}

// TestGrokResume replays the recorded session/resume and recall turn.
func TestGrokResume(t *testing.T) {
	peer, s, events, replay := acpTestStartReplay(t, acpGrok, StartRequest{SessionID: "ses_grok_17"}, "grok/resume.ndjson")
	peer.waitMethod("session/resume")
	if err := s.Prompt(context.Background(), Prompt{Text: "recall"}); err != nil {
		t.Fatal(err)
	}
	replay()
	text, _, res := acpTestTurn(t, events)
	if res.Type != EventResult || res.Result.Subtype != "success" || !strings.Contains(text, "OWL-31") {
		t.Fatalf("resume turn: %q %+v", text, res)
	}
}

// TestGrokLoad replays a resume that falls back to session/load.
func TestGrokLoad(t *testing.T) {
	peer, s, events, replay := acpTestStartReplay(t, acpGrok, StartRequest{SessionID: "ses_grok_17"}, "grok/load.ndjson", "session/resume")
	peer.waitMethod("session/load")
	if err := s.Prompt(context.Background(), Prompt{Text: "recall"}); err != nil {
		t.Fatal(err)
	}
	replay()
	text, _, res := acpTestTurn(t, events)
	if res.Type != EventResult || res.Result.Subtype != "success" || !strings.Contains(text, "OWL-31") {
		t.Fatalf("load turn: %q %+v", text, res)
	}
}

// TestGrokSkill replays the recorded turn that reads an injected skill.
func TestGrokSkill(t *testing.T) {
	_, s, events, replay := acpTestStartReplay(t, acpGrok, StartRequest{}, "grok/skill.ndjson")
	if err := s.Prompt(context.Background(), Prompt{Text: "use the skill"}); err != nil {
		t.Fatal(err)
	}
	replay()
	text, tools, res := acpTestTurn(t, events)
	if res.Type != EventResult || res.Result.Subtype != "success" || !strings.Contains(text, "SKILL-LIME") || len(tools) == 0 {
		t.Fatalf("skill turn: %q tools=%d %+v", text, len(tools), res)
	}
}

// TestGrokSetModel answers session/set_config_option with the recorded grok
// replies. The value must be a plain string, and a bad id keeps the vendor
// reason from error.data in the error text.
func TestGrokSetModel(t *testing.T) {
	frames := acpTestFixtureLines(t, "grok/set_model.ndjson")
	var unknown map[string]any
	for _, f := range frames {
		if id, _ := f["id"].(json.Number); id.String() == "6" {
			unknown = f
		}
	}
	handler := func(p *coaTestPeer, f coaTestFrame) {
		switch f.method() {
		case "initialize":
			p.reply(f.m["id"], acpTestResultOf(t, frames, "1"))
		case "session/new":
			p.reply(f.m["id"], acpTestResultOf(t, frames, "2"))
		case "session/set_config_option":
			params, _ := f.m["params"].(map[string]any)
			value, isString := params["value"].(string)
			if !isString || params["configId"] != "model" {
				p.push(map[string]any{"jsonrpc": "2.0", "id": f.m["id"], "error": map[string]any{"code": -32602, "message": "Invalid params"}})
				return
			}
			if value != "grok-4.6" {
				p.push(map[string]any{"jsonrpc": "2.0", "id": f.m["id"], "error": unknown["error"]})
				return
			}
			p.reply(f.m["id"], acpTestResultOf(t, frames, "3"))
		}
	}
	_, ws, proto := coaTestStartACP(t, acpGrok, StartRequest{}, handler)
	s := &acpSession{Session: ws, p: proto}
	coaTestWatch(t, s.Events()).next(EventInit)
	if err := s.SetModel(context.Background(), "grok-4.6"); err != nil {
		t.Fatalf("set model: %v", err)
	}
	var current string
	for _, m := range s.Models() {
		if m.Default {
			current = m.ID
		}
	}
	if current != "grok-4.6" {
		t.Fatalf("current model = %q, want grok-4.6", current)
	}
	err := s.SetModel(context.Background(), "grok-nope")
	if err == nil || !strings.Contains(err.Error(), "unknown model id") {
		t.Fatalf("bad model error: %v", err)
	}
}
