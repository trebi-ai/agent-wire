package agentwire

import (
	"context"
	"encoding/json"
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
func coaTestStartACP(t *testing.T, kind acpKind, req StartRequest, handler func(*coaTestPeer, coaTestFrame)) (*coaTestPeer, *wire.Session, *acpProtocol) {
	t.Helper()
	if req.HandshakeTimeout <= 0 {
		req.HandshakeTimeout = 2 * time.Second
	}
	peer := coaTestNewPeer(t, handler)
	proto := newACPProtocol(coaTestRuntime(), kind, req, &launch{}, false)
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
