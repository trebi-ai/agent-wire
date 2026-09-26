package agentwire

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/trebi-ai/agent-wire/internal/wire"
)

// This file tests the Claude stream-json protocol: fixture replay, the
// permission control round trip, the permission policy, tool bookkeeping,
// prompt encoding, malformed frames and exit classification.

// cpTestRuntime builds a runtime with no ledger, no logger and no shared
// servers. A protocol codec reads it only for the version cache.
func cpTestRuntime(t *testing.T) *Runtime {
	t.Helper()
	return New(Options{})
}

// cpTestClaudeProto builds a Claude protocol with no writer. Use it for
// Parse-only tests.
func cpTestClaudeProto(t *testing.T, policy PermissionPolicy) *claudeProtocol {
	t.Helper()
	return newClaudeProtocol(cpTestRuntime(t), policy)
}

// cpTestClaudeSession builds a Claude protocol behind an in-memory wire
// session. Every frame the protocol writes lands in the returned buffer, so a
// test reads back the control responses the CLI would have received.
func cpTestClaudeSession(t *testing.T, policy PermissionPolicy) (*claudeProtocol, *bytes.Buffer, *wire.Session) {
	t.Helper()
	stdin := &bytes.Buffer{}
	proto := newClaudeProtocol(cpTestRuntime(t), policy)
	s := wire.NewMemorySession("claude", proto, stdin, strings.NewReader(""),
		context.Background(), wire.Config{})
	return proto, stdin, s
}

// cpTestClaudeFixtures lists every recorded Claude fixture under testdata.
func cpTestClaudeFixtures(t *testing.T) []string {
	t.Helper()
	root := filepath.Join("testdata", "claude")
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".ndjson") {
			out = append(out, filepath.ToSlash(path))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk fixtures: %v", err)
	}
	sort.Strings(out)
	return out
}

// cpTestClaudeLines reads one fixture and returns its non-empty frames.
func cpTestClaudeLines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open fixture %s: %v", path, err)
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			out = append(out, line)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan fixture %s: %v", path, err)
	}
	if len(out) == 0 {
		t.Fatalf("fixture %s has no frames", path)
	}
	return out
}

// cpTestClaudeParseFile replays one fixture through the protocol parser.
func cpTestClaudeParseFile(t *testing.T, proto *claudeProtocol, path string) []Event {
	t.Helper()
	var out []Event
	for _, line := range cpTestClaudeLines(t, path) {
		out = append(out, proto.Parse([]byte(line))...)
	}
	return out
}

// cpTestCountEvents counts the events of one type.
func cpTestCountEvents(evs []Event, typ EventType) int {
	n := 0
	for _, e := range evs {
		if e.Type == typ {
			n++
		}
	}
	return n
}

// cpTestFirstEvent returns the first event of one type, or nil.
func cpTestFirstEvent(evs []Event, typ EventType) *Event {
	for i := range evs {
		if evs[i].Type == typ {
			return &evs[i]
		}
	}
	return nil
}

// cpTestCollectEvents drains a session's event channel until it closes.
func cpTestCollectEvents(t *testing.T, s *wire.Session) []Event {
	t.Helper()
	var out []Event
	deadline := time.After(30 * time.Second)
	for {
		select {
		case e, ok := <-s.Events():
			if !ok {
				return out
			}
			out = append(out, e)
		case <-deadline:
			t.Fatalf("timed out draining events after %d", len(out))
		}
	}
}

// cpTestFrames decodes every frame a protocol wrote to its stdin.
func cpTestFrames(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("decode written frame %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

// cpTestFramesOfType returns every written frame of one type.
func cpTestFramesOfType(t *testing.T, buf *bytes.Buffer, typ string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, m := range cpTestFrames(t, buf) {
		if m["type"] == typ {
			out = append(out, m)
		}
	}
	return out
}

// cpTestLastFrameOfType returns the last written frame of one type.
func cpTestLastFrameOfType(t *testing.T, buf *bytes.Buffer, typ string) map[string]any {
	t.Helper()
	frames := cpTestFramesOfType(t, buf, typ)
	if len(frames) == 0 {
		t.Fatalf("no %q frame written; frames: %v", typ, cpTestFrames(t, buf))
	}
	return frames[len(frames)-1]
}

// cpTestControlInner returns the inner response object of a control_response
// frame: response.response.
func cpTestControlInner(t *testing.T, frame map[string]any) map[string]any {
	t.Helper()
	env, _ := frame["response"].(map[string]any)
	if env == nil {
		t.Fatalf("control_response has no response envelope: %v", frame)
	}
	inner, _ := env["response"].(map[string]any)
	if inner == nil {
		t.Fatalf("control_response has no inner response: %v", frame)
	}
	return inner
}

// cpTestControlRequest renders one can_use_tool control frame.
func cpTestControlRequest(id, tool, input string) string {
	return `{"type":"control_request","request_id":"` + id + `","request":{"subtype":"can_use_tool","tool_name":"` +
		tool + `","input":` + input + `}}`
}

// cpTestClaudeMaps reports the size of the two bookkeeping maps under the
// protocol lock.
//
// These maps are internal state, and a test normally must not assert on
// internal state. Part G.3 makes their emptiness the contract here: `tools` and
// `pending` grew for the life of the session before the fix, so the test pins
// that they do not grow.
func cpTestClaudeMaps(p *claudeProtocol) (pending, tools int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.pending), len(p.tools)
}

// TestClaudeFixtureStreams replays every recorded fixture and pins the
// invariants that hold for all of them: one init with a session id, exactly one
// terminal result, tool events that keep their names, and a classified code on
// every error result.
func TestClaudeFixtureStreams(t *testing.T) {
	t.Parallel()
	paths := cpTestClaudeFixtures(t)
	if len(paths) < 5 {
		t.Fatalf("expected the recorded fixtures, found %d: %v", len(paths), paths)
	}
	for _, path := range paths {
		t.Run(strings.TrimPrefix(path, "testdata/claude/"), func(t *testing.T) {
			t.Parallel()
			proto := cpTestClaudeProto(t, PermissionPolicy{})
			evs := cpTestClaudeParseFile(t, proto, path)
			if len(evs) == 0 {
				t.Fatal("fixture produced no events")
			}

			init := cpTestFirstEvent(evs, EventInit)
			if init == nil || init.SessionID == "" {
				t.Fatalf("no init event with a session id: %+v", evs)
			}

			if got := cpTestCountEvents(evs, EventResult); got != 1 {
				t.Fatalf("result events = %d, want 1", got)
			}
			res := cpTestFirstEvent(evs, EventResult)
			if res.Result == nil {
				t.Fatal("result event has no payload")
			}
			if res.Result.IsError {
				if res.Result.EndReason == "" || res.Result.Code == "" {
					t.Fatalf("error result is not classified: %+v", res.Result)
				}
			} else if res.Result.Code != "" {
				t.Fatalf("success result carries a failure code: %+v", res.Result)
			}

			// A completion event carries only the tool id on the wire, so it
			// must recover the name from the matching start event.
			starts := map[string]string{}
			for _, e := range evs {
				if e.Type != EventTool || e.Tool == nil {
					continue
				}
				if e.Tool.Kind == "" {
					t.Fatalf("tool event has no kind: %+v", e.Tool)
				}
				if e.Tool.Status == "started" {
					if e.Tool.ID == "" || e.Tool.Name == "" {
						t.Fatalf("tool start is incomplete: %+v", e.Tool)
					}
					starts[e.Tool.ID] = e.Tool.Name
					continue
				}
				if name, ok := starts[e.Tool.ID]; ok && e.Tool.Name != name {
					t.Fatalf("tool completion lost the name: got %q want %q", e.Tool.Name, name)
				}
			}
		})
	}
}

// TestClaudeFixtureBasic pins the event stream of the recorded basic turn:
// init, a complete assistant block, a text delta, a tool start and completion,
// and a success result with usage and cost.
func TestClaudeFixtureBasic(t *testing.T) {
	t.Parallel()
	proto := cpTestClaudeProto(t, PermissionPolicy{})
	evs := cpTestClaudeParseFile(t, proto, "testdata/claude/basic.ndjson")

	init := cpTestFirstEvent(evs, EventInit)
	if init == nil {
		t.Fatalf("no init event: %+v", evs)
	}
	if init.SessionID != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("init session id = %q", init.SessionID)
	}
	if init.Text != "claude-sonnet-4-5" {
		t.Fatalf("init model = %q", init.Text)
	}

	var assistant []Event
	for _, e := range evs {
		if e.Type == EventAssistant {
			assistant = append(assistant, e)
		}
	}
	if len(assistant) != 2 {
		t.Fatalf("assistant events = %d, want 2: %+v", len(assistant), assistant)
	}
	if assistant[0].Text != "hello from claude" || assistant[0].Delta {
		t.Fatalf("complete assistant event = %+v", assistant[0])
	}
	if assistant[1].Text != "more " || !assistant[1].Delta {
		t.Fatalf("delta assistant event = %+v", assistant[1])
	}

	var tools []*ToolEvent
	for _, e := range evs {
		if e.Type == EventTool {
			tools = append(tools, e.Tool)
		}
	}
	if len(tools) != 2 {
		t.Fatalf("tool events = %d, want 2: %+v", len(tools), tools)
	}
	if tools[0].Name != "Bash" || tools[0].Status != "started" || tools[0].Kind != ToolExec {
		t.Fatalf("tool start = %+v", tools[0])
	}
	if tools[0].Input != `{"command":"echo hi"}` {
		t.Fatalf("tool start input = %q", tools[0].Input)
	}
	if tools[1].Name != "Bash" || tools[1].Status != "completed" || tools[1].Output != "hi\n" {
		t.Fatalf("tool completion = %+v", tools[1])
	}

	res := cpTestFirstEvent(evs, EventResult)
	if res == nil || res.Result == nil {
		t.Fatalf("no result event: %+v", evs)
	}
	r := res.Result
	if r.Subtype != "success" || r.IsError {
		t.Fatalf("result = %+v", r)
	}
	if r.SessionID != init.SessionID {
		t.Fatalf("result session id = %q", r.SessionID)
	}
	if r.Usage.Input != 100 || r.Usage.Output != 25 || r.Usage.CacheRead != 10 || r.Usage.CacheCreation != 5 {
		t.Fatalf("result usage = %+v", r.Usage)
	}
	if r.Usage.CostUSD != 0.0123 {
		t.Fatalf("result cost = %v", r.Usage.CostUSD)
	}
	if r.Usage.Model != "claude-sonnet-4-5" {
		t.Fatalf("result usage model = %q", r.Usage.Model)
	}
}

// TestClaudeFixtureSessionReplay runs one fixture through the real wire
// session, so framing, session-id propagation, the handshake frame and channel
// closure are covered end to end.
func TestClaudeFixtureSessionReplay(t *testing.T) {
	t.Parallel()
	lines := cpTestClaudeLines(t, "testdata/claude/basic.ndjson")

	stdin := &bytes.Buffer{}
	proto := newClaudeProtocol(cpTestRuntime(t), PermissionPolicy{})
	s := wire.NewMemorySession("claude", proto, stdin,
		strings.NewReader(strings.Join(lines, "\n")), context.Background(), wire.Config{})
	evs := cpTestCollectEvents(t, s)

	if s.Provider() != "claude" {
		t.Fatalf("provider = %q", s.Provider())
	}
	if s.ID() != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("session id = %q", s.ID())
	}
	if got := cpTestCountEvents(evs, EventInit); got != 1 {
		t.Fatalf("init events = %d, want 1: %+v", got, evs)
	}
	if got := cpTestCountEvents(evs, EventAssistant); got != 2 {
		t.Fatalf("assistant events = %d, want 2: %+v", got, evs)
	}
	if got := cpTestCountEvents(evs, EventTool); got != 2 {
		t.Fatalf("tool events = %d, want 2: %+v", got, evs)
	}
	res := cpTestFirstEvent(evs, EventResult)
	if res == nil || res.Result == nil || res.Result.IsError {
		t.Fatalf("result = %+v", res)
	}
	if res.Result.Usage.CostUSD != 0.0123 {
		t.Fatalf("result cost = %v", res.Result.Usage.CostUSD)
	}
	// Every event inherits the session id the init frame carried.
	for _, e := range evs {
		if e.SessionID != s.ID() {
			t.Fatalf("event %s lost the session id: %q", e.Type, e.SessionID)
		}
	}
	// The handshake writes the control initialize request before any prompt.
	if got := cpTestFramesOfType(t, stdin, "control_request"); len(got) != 1 {
		t.Fatalf("handshake frames = %v", got)
	}
	select {
	case <-s.Done():
	default:
		t.Fatal("Done() is not closed after the event channel closed")
	}
}

// TestClaudeFixturePermissionRequest pins the permission frame of the recorded
// permission turn: id, tool name, kind, question and input.
func TestClaudeFixturePermissionRequest(t *testing.T) {
	t.Parallel()
	proto := cpTestClaudeProto(t, PermissionPolicy{})
	evs := cpTestClaudeParseFile(t, proto, "testdata/claude/permission.ndjson")

	perm := cpTestFirstEvent(evs, EventPermission)
	if perm == nil || perm.Permission == nil {
		t.Fatalf("no permission event: %+v", evs)
	}
	if perm.Permission.ID != "req_9_abc12345" {
		t.Fatalf("permission id = %q", perm.Permission.ID)
	}
	if perm.Permission.Tool != "Bash" || perm.Permission.Kind != ToolExec {
		t.Fatalf("permission tool = %+v", perm.Permission)
	}
	if perm.Permission.Question != "Claude wants to run a command" {
		t.Fatalf("permission question = %q", perm.Permission.Question)
	}
	if !strings.Contains(perm.Permission.Input, "trebi-perm") {
		t.Fatalf("permission input = %q", perm.Permission.Input)
	}
}

// TestClaudeFixtureErrorResults pins the classified error result of the auth
// and limit fixtures.
func TestClaudeFixtureErrorResults(t *testing.T) {
	t.Parallel()
	cases := []struct {
		fixture string
		reason  string
		code    string
	}{
		{"testdata/claude/error_auth.ndjson", "auth", "claude.invalid_credentials"},
		{"testdata/claude/error_limit.ndjson", "limit", "claude.rate_limited"},
	}
	for _, tc := range cases {
		t.Run(filepath.Base(tc.fixture), func(t *testing.T) {
			t.Parallel()
			proto := cpTestClaudeProto(t, PermissionPolicy{})
			evs := cpTestClaudeParseFile(t, proto, tc.fixture)
			res := cpTestFirstEvent(evs, EventResult)
			if res == nil || res.Result == nil {
				t.Fatalf("no result event: %+v", evs)
			}
			r := res.Result
			if !r.IsError || r.Subtype != "error_during_execution" {
				t.Fatalf("result = %+v", r)
			}
			if r.Text == "" {
				t.Fatal("error result carries no text")
			}
			if r.EndReason != tc.reason {
				t.Fatalf("end reason = %q want %q (%s)", r.EndReason, tc.reason, r.Text)
			}
			if r.Code != tc.code {
				t.Fatalf("code = %q want %q", r.Code, tc.code)
			}
		})
	}
}

// TestClaudePermissionRoundTrip pins the control_request/control_response
// shapes: allow echoes the request input, deny carries the message.
func TestClaudePermissionRoundTrip(t *testing.T) {
	t.Parallel()
	const id = "req_9_abc12345"
	proto, stdin, s := cpTestClaudeSession(t, PermissionPolicy{})
	evs := proto.Parse([]byte(cpTestControlRequest(id, "Bash", `{"command":"echo side-effect > /tmp/x"}`)))
	perm := cpTestFirstEvent(evs, EventPermission)
	if perm == nil || perm.Permission == nil {
		t.Fatalf("no permission event: %+v", evs)
	}
	if perm.Permission.ID != id || perm.Permission.Tool != "Bash" {
		t.Fatalf("permission = %+v", perm.Permission)
	}
	if perm.Permission.Question != "Allow Bash?" {
		t.Fatalf("permission question = %q", perm.Permission.Question)
	}
	if !strings.Contains(perm.Permission.Input, "side-effect") {
		t.Fatalf("permission input = %q", perm.Permission.Input)
	}

	if err := s.AnswerPermission(context.Background(), id, Decision{Allow: true}); err != nil {
		t.Fatalf("answer allow: %v", err)
	}
	allow := cpTestLastFrameOfType(t, stdin, "control_response")
	env, _ := allow["response"].(map[string]any)
	if env["subtype"] != "success" || env["request_id"] != id {
		t.Fatalf("allow envelope = %+v", env)
	}
	inner := cpTestControlInner(t, allow)
	if inner["behavior"] != "allow" {
		t.Fatalf("allow behavior = %+v", inner)
	}
	updated, _ := inner["updatedInput"].(map[string]any)
	if updated == nil || updated["command"] != "echo side-effect > /tmp/x" {
		t.Fatalf("updatedInput must echo the request input: %+v", inner)
	}

	if err := s.AnswerPermission(context.Background(), id, Decision{Allow: false, Message: "no"}); err != nil {
		t.Fatalf("answer deny: %v", err)
	}
	deny := cpTestControlInner(t, cpTestLastFrameOfType(t, stdin, "control_response"))
	if deny["behavior"] != "deny" || deny["message"] != "no" {
		t.Fatalf("deny response = %+v", deny)
	}
}

// TestClaudePermissionAutoAnswer pins the policy decisions: auto answers every
// request, auto_edit answers edits only, and AllowTools matches a tool prefix.
func TestClaudePermissionAutoAnswer(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		policy  PermissionPolicy
		tool    string
		input   string
		allowed bool
	}{
		{"auto answers a command", PermissionPolicy{Mode: PermissionAuto}, "Bash", `{"command":"ls"}`, true},
		{"auto_edit answers an edit", PermissionPolicy{Mode: PermissionAutoEdit}, "Edit", `{"file_path":"/tmp/x"}`, true},
		{"auto_edit still asks for a command", PermissionPolicy{Mode: PermissionAutoEdit}, "Bash", `{"command":"ls"}`, false},
		{"allow list matches the server prefix", PermissionPolicy{AllowTools: []string{"mcp__sling__*"}}, "mcp__sling__echo", `{}`, true},
		{"allow list leaves another server alone", PermissionPolicy{AllowTools: []string{"mcp__sling__*"}}, "mcp__other__echo", `{}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			proto, stdin, _ := cpTestClaudeSession(t, tc.policy)
			evs := proto.Parse([]byte(cpTestControlRequest("req_1", tc.tool, tc.input)))
			perm := cpTestFirstEvent(evs, EventPermission)
			responses := cpTestFramesOfType(t, stdin, "control_response")

			if !tc.allowed {
				if perm == nil {
					t.Fatalf("expected a permission event: %+v", evs)
				}
				if len(responses) != 0 {
					t.Fatalf("the policy answered a request it must ask about: %v", responses)
				}
				return
			}
			if perm != nil {
				t.Fatalf("the policy answered but a permission event surfaced: %+v", perm.Permission)
			}
			if len(responses) != 1 {
				t.Fatalf("control responses = %d, want 1: %v", len(responses), responses)
			}
			inner := cpTestControlInner(t, responses[0])
			if inner["behavior"] != "allow" {
				t.Fatalf("auto answer behavior = %+v", inner)
			}
			// The tool_use block already announced the start, and the
			// tool_result will complete it. An extra event keyed by the
			// control request_id would never complete (plan 2026-09-26 B).
			if tool := cpTestFirstEvent(evs, EventTool); tool != nil {
				t.Fatalf("auto answer emitted a phantom tool event: %+v", tool.Tool)
			}
		})
	}
}

// TestClaudeToolBookkeeping pins Part G.3: the id-to-name map drops an entry on
// tool_result, and the pending permission map drops an entry on the answer.
func TestClaudeToolBookkeeping(t *testing.T) {
	t.Parallel()
	proto, _, s := cpTestClaudeSession(t, PermissionPolicy{})

	start := proto.Parse([]byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"ls"}}]}}`))
	if len(start) != 1 || start[0].Tool == nil || start[0].Tool.Status != "started" {
		t.Fatalf("tool start events = %+v", start)
	}
	if pending, tools := cpTestClaudeMaps(proto); pending != 0 || tools != 1 {
		t.Fatalf("after tool_use: pending=%d tools=%d, want 0 and 1", pending, tools)
	}

	done := proto.Parse([]byte(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}}`))
	if len(done) != 1 || done[0].Tool == nil {
		t.Fatalf("tool result events = %+v", done)
	}
	if done[0].Tool.Name != "Bash" || done[0].Tool.Output != "ok" || done[0].Tool.Status != "completed" {
		t.Fatalf("completion event = %+v", done[0].Tool)
	}
	if pending, tools := cpTestClaudeMaps(proto); pending != 0 || tools != 0 {
		t.Fatalf("the maps grew for the life of the session: pending=%d tools=%d", pending, tools)
	}

	proto.Parse([]byte(cpTestControlRequest("req_7", "Bash", `{"command":"ls"}`)))
	if pending, _ := cpTestClaudeMaps(proto); pending != 1 {
		t.Fatalf("pending after the request = %d, want 1", pending)
	}
	if err := s.AnswerPermission(context.Background(), "req_7", Decision{Allow: true}); err != nil {
		t.Fatalf("answer permission: %v", err)
	}
	if pending, _ := cpTestClaudeMaps(proto); pending != 0 {
		t.Fatalf("pending survived the answer: %d", pending)
	}

	// Replay a recorded turn that uses several tools and one permission, and
	// answer that permission. Nothing may be left behind at the end.
	replay, stdin, replaySession := cpTestClaudeSession(t, PermissionPolicy{})
	for _, line := range cpTestClaudeLines(t, "testdata/claude/0.3.273-2.1.267/permission_deny.ndjson") {
		for _, e := range replay.Parse([]byte(line)) {
			if e.Type != EventPermission || e.Permission == nil {
				continue
			}
			if err := replaySession.AnswerPermission(context.Background(), e.Permission.ID, Decision{Allow: false, Message: "no"}); err != nil {
				t.Fatalf("answer replayed permission: %v", err)
			}
		}
	}
	if pending, tools := cpTestClaudeMaps(replay); pending != 0 || tools != 0 {
		t.Fatalf("bookkeeping survived the session: pending=%d tools=%d", pending, tools)
	}
	if got := cpTestFramesOfType(t, stdin, "control_response"); len(got) != 1 {
		t.Fatalf("control responses = %v", got)
	}
}

// TestClaudeToolKindAndPaths pins the coarse tool kind and the edit paths.
func TestClaudeToolKindAndPaths(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		input string
		kind  ToolKind
		paths string
	}{
		{"Edit", `{"file_path":"/tmp/x"}`, ToolEdit, "/tmp/x"},
		{"Bash", `{"command":"ls"}`, ToolExec, ""},
		{"Read", `{"file_path":"/tmp/x"}`, ToolRead, ""},
		{"mcp__x__y", `{}`, ToolMCP, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			proto := cpTestClaudeProto(t, PermissionPolicy{})
			frame := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"` +
				tc.name + `","input":` + tc.input + `}]}}`
			evs := proto.Parse([]byte(frame))
			if len(evs) != 1 || evs[0].Tool == nil {
				t.Fatalf("events = %+v", evs)
			}
			tool := evs[0].Tool
			if tool.Kind != tc.kind {
				t.Fatalf("kind = %q want %q", tool.Kind, tc.kind)
			}
			if got := strings.Join(tool.Paths, ","); got != tc.paths {
				t.Fatalf("paths = %q want %q", got, tc.paths)
			}
		})
	}
}

// cpTestPromptContent decodes an EncodePrompt frame and returns its
// message.content value.
func cpTestPromptContent(t *testing.T, proto *claudeProtocol, pr Prompt) any {
	t.Helper()
	frame, err := proto.EncodePrompt(pr)
	if err != nil {
		t.Fatalf("encode prompt: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(frame, &m); err != nil {
		t.Fatalf("decode prompt frame: %v", err)
	}
	if m["type"] != "user" {
		t.Fatalf("prompt frame type = %v", m["type"])
	}
	msg, _ := m["message"].(map[string]any)
	if msg == nil || msg["role"] != "user" {
		t.Fatalf("prompt message = %v", m["message"])
	}
	return msg["content"]
}

// cpTestBlocks returns a prompt content value as content blocks.
func cpTestBlocks(t *testing.T, content any) []map[string]any {
	t.Helper()
	list, ok := content.([]any)
	if !ok {
		t.Fatalf("content is not a block list: %T", content)
	}
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		b, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("block is not an object: %T", item)
		}
		out = append(out, b)
	}
	return out
}

// TestClaudeEncodePromptAttachments pins the prompt body shape: a plain string
// without attachments, a block list with images first otherwise.
func TestClaudeEncodePromptAttachments(t *testing.T) {
	t.Parallel()
	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}

	t.Run("text only is a plain string", func(t *testing.T) {
		t.Parallel()
		proto := cpTestClaudeProto(t, PermissionPolicy{})
		content := cpTestPromptContent(t, proto, Prompt{Text: "hello"})
		text, ok := content.(string)
		if !ok || text != "hello" {
			t.Fatalf("content = %#v", content)
		}
	})

	t.Run("an image becomes a base64 block", func(t *testing.T) {
		t.Parallel()
		proto := cpTestClaudeProto(t, PermissionPolicy{})
		content := cpTestPromptContent(t, proto, Prompt{
			Text:        "what is this?",
			Attachments: []Attachment{{MIME: "image/png", Data: png}},
		})
		blocks := cpTestBlocks(t, content)
		if len(blocks) != 2 {
			t.Fatalf("blocks = %+v", blocks)
		}
		first := blocks[0]
		if first["type"] != "image" {
			t.Fatalf("first block = %+v", first)
		}
		source, _ := first["source"].(map[string]any)
		if source["type"] != "base64" {
			t.Fatalf("image source type = %v", source["type"])
		}
		if source["media_type"] != "image/png" {
			t.Fatalf("image media type = %v", source["media_type"])
		}
		if want := base64.StdEncoding.EncodeToString(png); source["data"] != want {
			t.Fatalf("image data = %v want %v", source["data"], want)
		}
		last := blocks[1]
		if last["type"] != "text" || last["text"] != "what is this?" {
			t.Fatalf("last block = %+v", last)
		}
	})

	t.Run("an image after a text attachment is reordered first", func(t *testing.T) {
		t.Parallel()
		proto := cpTestClaudeProto(t, PermissionPolicy{})
		content := cpTestPromptContent(t, proto, Prompt{
			Text: "prompt",
			Attachments: []Attachment{
				{MIME: "text/plain", Data: []byte("snippet")},
				{MIME: "image/png", Data: png},
			},
		})
		blocks := cpTestBlocks(t, content)
		if len(blocks) != 3 {
			t.Fatalf("blocks = %+v", blocks)
		}
		if blocks[0]["type"] != "image" {
			t.Fatalf("the image must come first: %+v", blocks)
		}
		if blocks[1]["type"] != "text" || blocks[1]["text"] != "snippet" {
			t.Fatalf("text attachment block = %+v", blocks[1])
		}
		if blocks[2]["type"] != "text" || blocks[2]["text"] != "prompt" {
			t.Fatalf("prompt text block = %+v", blocks[2])
		}
	})
}

// TestClaudeMalformedFrame pins the two frame-failure paths: a non-JSON
// diagnostic line is a status, and a broken JSON object is an error.
func TestClaudeMalformedFrame(t *testing.T) {
	t.Parallel()
	proto := cpTestClaudeProto(t, PermissionPolicy{})

	evs := proto.Parse([]byte("npm notice: update available"))
	if len(evs) != 1 || evs[0].Type != EventStatus {
		t.Fatalf("non-JSON line = %+v", evs)
	}
	if evs[0].Status != StatusRunning || evs[0].Text != "npm notice: update available" {
		t.Fatalf("status event = %+v", evs[0])
	}

	evs = proto.Parse([]byte(`{"type":"assistant","message":`))
	if len(evs) != 1 || evs[0].Type != EventError {
		t.Fatalf("broken JSON frame = %+v", evs)
	}
	if evs[0].Code != "malformed_frame" {
		t.Fatalf("code = %q", evs[0].Code)
	}
	if evs[0].EndReason != "protocol" {
		t.Fatalf("end reason = %q", evs[0].EndReason)
	}
	if evs[0].Error == "" {
		t.Fatal("malformed frame carries no error text")
	}

	if evs := proto.Parse([]byte("   ")); len(evs) != 0 {
		t.Fatalf("blank line = %+v", evs)
	}
}

// TestClaudeExitClassification pins the crash classification of a process that
// died before a result frame, and the silence after a result.
func TestClaudeExitClassification(t *testing.T) {
	t.Parallel()

	proto := cpTestClaudeProto(t, PermissionPolicy{})
	evs := proto.Exit(1, nil, "Invalid API key")
	if len(evs) != 1 || evs[0].Type != EventError {
		t.Fatalf("exit events = %+v", evs)
	}
	if evs[0].EndReason != "auth" || evs[0].Code != "claude.invalid_credentials" {
		t.Fatalf("exit classification = %+v", evs[0])
	}
	if !strings.Contains(evs[0].Error, "Invalid API key") {
		t.Fatalf("exit error text = %q", evs[0].Error)
	}

	// No stderr and no cause: the code still names the failure.
	bare := cpTestClaudeProto(t, PermissionPolicy{}).Exit(2, nil, "")
	if len(bare) != 1 || bare[0].Type != EventError {
		t.Fatalf("bare exit events = %+v", bare)
	}
	if !strings.Contains(bare[0].Error, "code 2") {
		t.Fatalf("bare exit text = %q", bare[0].Error)
	}
	if bare[0].Code == "" || bare[0].EndReason == "" {
		t.Fatalf("bare exit is not classified: %+v", bare[0])
	}

	// A turn that produced a result is not a crash.
	after := cpTestClaudeProto(t, PermissionPolicy{})
	after.Parse([]byte(`{"type":"result","subtype":"success","is_error":false,"result":"done"}`))
	if evs := after.Exit(1, nil, "Invalid API key"); len(evs) != 0 {
		t.Fatalf("exit after a result = %+v", evs)
	}
}

// TestClaudeMCPAutoAllowFixture replays the recorded auto-allowed MCP call
// (plan 2026-09-26 B): the policy answers the can_use_tool control request,
// and the only tool events carry the toolu_ id — never the control
// request_id. Every started id ends completed.
func TestClaudeMCPAutoAllowFixture(t *testing.T) {
	t.Parallel()
	proto := cpTestClaudeProto(t, PermissionPolicy{AllowTools: []string{"mcp__trebi__*"}})
	evs := cpTestClaudeParseFile(t, proto, "testdata/claude/0.3.273-2.1.267/mcp_auto_allow.ndjson")

	var ids []string
	starts, ends := map[string]bool{}, map[string]bool{}
	for _, e := range evs {
		if e.Type != EventTool || e.Tool == nil {
			continue
		}
		ids = append(ids, e.Tool.ID)
		if e.Tool.ID == "2a53d801-0b9a-49a1-8073-c1fb604130c3" {
			t.Fatalf("a tool event is keyed by the control request_id: %+v", e.Tool)
		}
		switch e.Tool.Status {
		case "started":
			starts[e.Tool.ID] = true
		case "completed", "failed", "cancelled":
			ends[e.Tool.ID] = true
		}
	}
	if len(ids) == 0 {
		t.Fatalf("no tool events: %+v", evs)
	}
	for _, e := range evs {
		if e.Type == EventPermission && e.Permission != nil && e.Permission.ToolUseID != "toolu_01AutoAllowMcp" {
			t.Fatalf("permission event lost the tool_use id: %+v", e.Permission)
		}
	}
	for id := range starts {
		if !ends[id] {
			t.Fatalf("tool %q started but never ended: %+v", id, ids)
		}
	}
}

// TestClaudeResultClosesOpenTools pins the cancelled drain: a start with no
// tool_result, then the result frame, gives one cancelled event per open id.
func TestClaudeResultClosesOpenTools(t *testing.T) {
	t.Parallel()
	proto := cpTestClaudeProto(t, PermissionPolicy{})

	proto.Parse([]byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_lost","name":"Bash","input":{"command":"ls"}}]}}`))
	evs := proto.Parse([]byte(`{"type":"result","subtype":"success","is_error":false,"result":"done"}`))

	var cancelled int
	sawResult := false
	for _, e := range evs {
		if e.Type == EventTool && e.Tool != nil && e.Tool.Status == "cancelled" {
			if e.Tool.ID != "toolu_lost" {
				t.Fatalf("cancelled event for the wrong id: %+v", e.Tool)
			}
			cancelled++
		}
		if e.Type == EventResult {
			sawResult = true
		}
	}
	if !sawResult || cancelled != 1 {
		t.Fatalf("result events=%v cancelled=%d, want one result then one cancelled", evs, cancelled)
	}

	// The table is clear: a second result drains nothing.
	if evs := proto.Parse([]byte(`{"type":"result","subtype":"success","is_error":false,"result":"again"}`)); len(evs) != 1 || evs[0].Type != EventResult {
		t.Fatalf("second result events = %+v", evs)
	}
}

// TestClaudeExitCancelsOpenTools covers the process-exit drain.
func TestClaudeExitCancelsOpenTools(t *testing.T) {
	t.Parallel()
	proto := cpTestClaudeProto(t, PermissionPolicy{})
	proto.Parse([]byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_dead","name":"Write","input":"{}"}]}}`))

	evs := proto.Exit(1, nil, "segfault")
	if len(evs) != 2 {
		t.Fatalf("exit events = %+v", evs)
	}
	if evs[0].Type != EventTool || evs[0].Tool.Status != "cancelled" || evs[0].Tool.ID != "toolu_dead" {
		t.Fatalf("first exit event = %+v", evs[0])
	}
	if evs[1].Type != EventError {
		t.Fatalf("second exit event = %+v", evs[1])
	}
}
