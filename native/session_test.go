package native

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/trebi-ai/agent-wire"
)

// scriptModel answers every Stream call with the next script entry.
type scriptModel struct {
	mu      sync.Mutex
	name    string
	calls   []Request
	scripts [][]StreamPart
	errs    []error
}

func (m *scriptModel) Name() string {
	if m.name == "" {
		return "script-1"
	}
	return m.name
}

func (m *scriptModel) Stream(_ context.Context, req Request) Seq[StreamPart] {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, req)
	i := len(m.calls) - 1
	if i < len(m.errs) && m.errs[i] != nil {
		err := m.errs[i]
		return func(yield func(StreamPart, error) bool) {
			yield(StreamPart{}, err)
		}
	}
	var parts []StreamPart
	if i < len(m.scripts) {
		parts = m.scripts[i]
	}
	return func(yield func(StreamPart, error) bool) {
		for _, p := range parts {
			if !yield(p, nil) {
				return
			}
		}
	}
}

func (m *scriptModel) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

func textParts(text string) []StreamPart {
	return []StreamPart{
		{Kind: StreamTextDelta, Delta: text},
		{Kind: StreamFinish, Finish: &Finish{Reason: FinishStop, Usage: Usage{Input: 10, Output: 5}}},
	}
}

func toolCallParts(id, name, input string) []StreamPart {
	return []StreamPart{
		{Kind: StreamToolInputStart, ID: id},
		{Kind: StreamToolInputDelta, ID: id, Delta: input},
		{Kind: StreamToolCallDone, Call: &ToolCall{ID: id, Name: name, Input: json.RawMessage(input)}},
		{Kind: StreamFinish, Finish: &Finish{Reason: FinishToolCalls, Usage: Usage{Input: 20, Output: 8}}},
	}
}

// collect drains one session's events until the idle status or the timeout.
func collect(t *testing.T, s *session) []agentwire.Event {
	t.Helper()
	var out []agentwire.Event
	deadline := time.After(5 * time.Second)
	for {
		select {
		case evt, ok := <-s.Events():
			if !ok {
				return out
			}
			out = append(out, evt)
			if evt.Type == agentwire.EventStatus && evt.Status == agentwire.StatusIdle {
				return out
			}
		case <-deadline:
			t.Fatalf("timed out collecting events; got %+v", out)
		}
	}
}

// echoTool is a read-only echo tool.
func echoTool() Tool {
	return NewFunc(ToolSpec{
		Name:        "echo",
		Description: "echo the text",
		Annotations: Annotations{ReadOnly: true, Kind: agentwire.ToolOther},
	}, func(ctx context.Context, in struct {
		Text string `json:"text"`
	}) (string, error) {
		return "echo: " + in.Text, nil
	})
}

// TestSessionTextOnlyTurn pins the plain text path: deltas and the final
// assistant event, one usage event, and a success result.
func TestSessionTextOnlyTurn(t *testing.T) {
	m := &scriptModel{scripts: [][]StreamPart{textParts("hello world")}}
	s := newSession(nil, m, "conv-1", sessionOpts{maxTurns: 5}, nil, SkillCatalog{})
	s.store = nil
	ctx := context.Background()
	if err := s.Prompt(ctx, agentwire.Prompt{Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	evts := collect(t, s)
	var sawDelta, sawFinal, sawUsage, sawResult bool
	for _, e := range evts {
		switch {
		case e.Type == agentwire.EventAssistant && e.Delta:
			sawDelta = true
		case e.Type == agentwire.EventAssistant && !e.Delta && e.Text == "hello world":
			sawFinal = true
		case e.Type == agentwire.EventUsage:
			sawUsage = true
		case e.Type == agentwire.EventResult:
			sawResult = e.Result != nil && e.Result.EndReason == "end_turn"
		}
	}
	if !sawDelta || !sawFinal || !sawUsage || !sawResult {
		t.Fatalf("delta=%v final=%v usage=%v result=%v", sawDelta, sawFinal, sawUsage, sawResult)
	}
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestSessionOneToolCall covers start, execution, completion, and the tool
// message fed back to the model.
func TestSessionOneToolCall(t *testing.T) {
	m := &scriptModel{scripts: [][]StreamPart{
		toolCallParts("call_1", "echo", `{"text":"hi"}`),
		textParts("done"),
	}}
	tools := []nativeTool{{tool: echoTool(), spec: echoTool().Spec()}}
	s := newSession(nil, m, "conv-2", sessionOpts{maxTurns: 5}, tools, SkillCatalog{})
	s.store = nil
	if err := s.Prompt(context.Background(), agentwire.Prompt{Text: "use the tool"}); err != nil {
		t.Fatal(err)
	}
	evts := collect(t, s)
	var started, completed bool
	for _, e := range evts {
		if e.Type == agentwire.EventTool && e.Tool != nil && e.Tool.ID == "call_1" {
			switch e.Tool.Status {
			case "started":
				started = true
				if e.Tool.Name != "echo" || e.Tool.Kind != agentwire.ToolOther {
					t.Fatalf("start event = %+v", e.Tool)
				}
			case "completed":
				completed = true
				if !strings.Contains(e.Tool.Output, "echo: hi") {
					t.Fatalf("output = %q", e.Tool.Output)
				}
			}
		}
	}
	if !started || !completed {
		t.Fatalf("started=%v completed=%v", started, completed)
	}
	// The second model call carries the tool result.
	reqs := m.calls
	if len(reqs) != 2 {
		t.Fatalf("model calls = %d", len(reqs))
	}
	var sawToolResult bool
	for _, msg := range reqs[1].Messages {
		for _, p := range msg.Parts {
			if p.ToolResult != nil && p.ToolResult.CallID == "call_1" {
				sawToolResult = true
			}
		}
	}
	if !sawToolResult {
		t.Fatal("the tool result never reached the model")
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestSessionToolError pins a model-visible failure: IsError reaches the
// model as a tool message, and the turn still ends cleanly.
func TestSessionToolError(t *testing.T) {
	errTool := NewFunc(ToolSpec{
		Name:        "boom",
		Annotations: Annotations{ReadOnly: true, Kind: agentwire.ToolOther},
	}, func(ctx context.Context, in struct{}) (string, error) {
		return "", errors.New("the operation failed")
	})
	m := &scriptModel{scripts: [][]StreamPart{
		toolCallParts("call_e", "boom", `{}`),
		textParts("recovered"),
	}}
	tools := []nativeTool{{tool: errTool, spec: errTool.Spec()}}
	s := newSession(nil, m, "conv-3", sessionOpts{maxTurns: 5}, tools, SkillCatalog{})
	s.store = nil
	if err := s.Prompt(context.Background(), agentwire.Prompt{Text: "go"}); err != nil {
		t.Fatal(err)
	}
	evts := collect(t, s)
	var failed bool
	for _, e := range evts {
		if e.Type == agentwire.EventTool && e.Tool != nil && e.Tool.Status == "failed" {
			failed = true
		}
	}
	if !failed {
		t.Fatalf("no failed tool event in %+v", evts)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestSessionParallelToolCalls covers two calls in one response: both run
// and both answers reach the model.
func TestSessionParallelToolCalls(t *testing.T) {
	m := &scriptModel{scripts: [][]StreamPart{
		append(toolCallParts("c1", "echo", `{"text":"one"}`), toolCallParts("c2", "echo", `{"text":"two"}`)...),
		textParts("both done"),
	}}
	tools := []nativeTool{{tool: echoTool(), spec: echoTool().Spec()}}
	s := newSession(nil, m, "conv-4", sessionOpts{maxTurns: 5}, tools, SkillCatalog{})
	s.store = nil
	if err := s.Prompt(context.Background(), agentwire.Prompt{Text: "go"}); err != nil {
		t.Fatal(err)
	}
	evts := collect(t, s)
	completed := map[string]bool{}
	for _, e := range evts {
		if e.Type == agentwire.EventTool && e.Tool != nil && e.Tool.Status == "completed" {
			completed[e.Tool.ID] = true
		}
	}
	if !completed["c1"] || !completed["c2"] {
		t.Fatalf("completions = %v", completed)
	}
	if m.callCount() != 2 {
		t.Fatalf("model calls = %d", m.callCount())
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestSessionMaxTurns pins the max_turn_requests end reason.
func TestSessionMaxTurns(t *testing.T) {
	// Every call asks for the same tool; MaxTurns = 2 ends the turn.
	call := toolCallParts("cx", "echo", `{"text":"again"}`)
	m := &scriptModel{scripts: [][]StreamPart{call, call, call, call}}
	tools := []nativeTool{{tool: echoTool(), spec: echoTool().Spec()}}
	s := newSession(nil, m, "conv-5", sessionOpts{maxTurns: 2}, tools, SkillCatalog{})
	s.store = nil
	ctx := context.Background()
	if err := s.Prompt(ctx, agentwire.Prompt{Text: "loop"}); err != nil {
		t.Fatal(err)
	}
	evts := collect(t, s)
	var endReason string
	for _, e := range evts {
		if e.Type == agentwire.EventResult && e.Result != nil {
			endReason = e.Result.EndReason
		}
	}
	if endReason != "max_turn_requests" {
		t.Fatalf("end reason = %q", endReason)
	}
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestSessionInterruptMidStream pins the cancelled result on context cancel.
func TestSessionInterruptMidStream(t *testing.T) {
	m := &scriptModel{errs: []error{context.Canceled}}
	s := newSession(nil, m, "conv-6", sessionOpts{maxTurns: 5}, nil, SkillCatalog{})
	s.store = nil
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Prompt(ctx, agentwire.Prompt{Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	evts := collect(t, s)
	var sawCancelled bool
	for _, e := range evts {
		if e.Type == agentwire.EventResult && e.Result != nil && e.Result.EndReason == "cancelled" {
			sawCancelled = true
		}
	}
	if !sawCancelled {
		t.Fatalf("no cancelled result in %+v", evts)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestSessionApprovalDenied covers the ask path: an EventPermission goes out,
// a denial comes back, and the tool never runs.
func TestSessionApprovalDenied(t *testing.T) {
	writeTool := NewFunc(ToolSpec{
		Name:        "write_file",
		Annotations: Annotations{Kind: agentwire.ToolEdit},
	}, func(ctx context.Context, in struct {
		Path string `json:"path"`
	}) (string, error) {
		return "wrote " + in.Path, nil
	})
	m := &scriptModel{scripts: [][]StreamPart{
		toolCallParts("call_w", "write_file", `{"path":"x.txt"}`),
		textParts("acknowledged"),
	}}
	tools := []nativeTool{{tool: writeTool, spec: writeTool.Spec()}}
	s := newSession(nil, m, "conv-7", sessionOpts{maxTurns: 5}, tools, SkillCatalog{})
	s.store = nil
	answered := false
	s.opts.approver = PolicyApprover{
		Policy: agentwire.PermissionPolicy{Mode: agentwire.PermissionAsk},
		Emit: func(ctx context.Context, r ApprovalRequest) (func(context.Context) (agentwire.Decision, error), func()) {
			return s.approvals.emit(r, func(id string, rr ApprovalRequest) {
				s.emit(agentwire.Event{Type: agentwire.EventPermission, SessionID: s.ID(),
					Permission: &agentwire.Permission{ID: id, Tool: rr.Spec.Name}})
				if !answered {
					answered = true
					_ = s.approvals.decide(id, agentwire.Decision{Allow: false, Message: "no writes"})
				}
			})
		},
	}
	if err := s.Prompt(context.Background(), agentwire.Prompt{Text: "write"}); err != nil {
		t.Fatal(err)
	}
	evts := collect(t, s)
	var sawPerm, sawFailed bool
	for _, e := range evts {
		switch {
		case e.Type == agentwire.EventPermission:
			sawPerm = true
		case e.Type == agentwire.EventTool && e.Tool != nil && e.Tool.Status == "failed" &&
			strings.Contains(e.Tool.Output, "no writes"):
			sawFailed = true
		}
	}
	if !sawPerm || !sawFailed {
		t.Fatalf("permission=%v failed=%v", sawPerm, sawFailed)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestSessionSkillActivation covers the catalog and activate_skill tool.
func TestSessionSkillActivation(t *testing.T) {
	dir := t.TempDir()
	skillBody := "---\nname: deploy\ndescription: Ship a release.\nallowed-tools: bash\n---\nRun the release script."
	if err := os.WriteFile(dir+"/SKILL.md", []byte(skillBody), 0o644); err != nil {
		t.Fatal(err)
	}
	catalog, err := LoadSkills([]agentwire.Skill{{Name: "deploy", Dir: dir}})
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Skills) != 1 || catalog.Skills[0].Name != "deploy" {
		t.Fatalf("catalog = %+v", catalog.Skills)
	}
	if got := catalog.Skills[0].AllowedTools; len(got) != 1 || got[0] != "bash" {
		t.Fatalf("allowed tools = %v", got)
	}
	prompt := catalog.Prompt()
	if !strings.Contains(prompt, "deploy") || !strings.Contains(prompt, "Ship a release.") {
		t.Fatalf("prompt = %q", prompt)
	}

	// The activate tool loads the body and the resource list.
	act := catalog.ActivateTool()
	res, err := act.Call(context.Background(), json.RawMessage(`{"name":"deploy"}`))
	if err != nil {
		t.Fatal(err)
	}
	text := ""
	for _, p := range res.Content {
		if p.Text != nil {
			text += p.Text.Text
		}
	}
	if !strings.Contains(text, "Run the release script.") {
		t.Fatalf("activated content = %q", text)
	}

	// An unknown skill is a model-visible error, not a panic.
	res, err = act.Call(context.Background(), json.RawMessage(`{"name":"nope"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("unknown skill must be an error result")
	}
}

// TestFileStoreRoundTripAndLegacy covers Append/Load/Replace and the legacy
// trebi log shape.
func TestFileStoreRoundTripAndLegacy(t *testing.T) {
	dir := t.TempDir()
	st := FileStore(dir)
	ctx := context.Background()
	if _, err := st.Load(ctx, "s1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty load err = %v", err)
	}
	if err := st.Append(ctx, "s1", Message{Role: RoleUser, Parts: []Part{{Text: &TextPart{Text: "one"}}}}); err != nil {
		t.Fatal(err)
	}
	if err := st.Append(ctx, "s1", Message{Role: RoleAssistant, Parts: []Part{{Text: &TextPart{Text: "two"}}}}); err != nil {
		t.Fatal(err)
	}
	msgs, err := st.Load(ctx, "s1")
	if err != nil || len(msgs) != 2 {
		t.Fatalf("load = %v, %v", msgs, err)
	}
	// A session id with hostile characters stays inside the directory.
	if err := st.Append(ctx, "../escape", Message{Role: RoleUser, Parts: []Part{{Text: &TextPart{Text: "x"}}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir + "/___escape.messages.jsonl"); err != nil {
		t.Fatalf("the sanitized name is missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "..", "escape.messages.jsonl")); !os.IsNotExist(err) {
		t.Fatal("the session id escaped the store directory")
	}

	// The legacy trebi shape loads as native messages.
	legacy := `{"Role":"user","Content":"hello from the old writer"}
{"Role":"assistant","Content":"","ToolCalls":[{"ID":"tc1","Name":"bash","Arguments":{"command":"ls"}}]}
{"Role":"tool","ToolCallID":"tc1","Name":"bash","Content":"file.txt"}`
	if err := os.WriteFile(dir+"/legacy.messages.jsonl", []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	msgs, err = st.Load(ctx, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 {
		t.Fatalf("legacy load = %+v", msgs)
	}
	if msgs[0].Parts[0].Text == nil || msgs[0].Parts[0].Text.Text != "hello from the old writer" {
		t.Fatalf("legacy user = %+v", msgs[0])
	}
	if msgs[1].Parts[0].ToolCall == nil || msgs[1].Parts[0].ToolCall.Name != "bash" {
		t.Fatalf("legacy tool call = %+v", msgs[1])
	}
	if msgs[2].Parts[0].ToolResult == nil || msgs[2].Parts[0].ToolResult.Content[0].Text.Text != "file.txt" {
		t.Fatalf("legacy tool result = %+v", msgs[2])
	}
}

// TestResumeFromStore pins the resume path: the second session starts with
// the first session's log.
func TestResumeFromStore(t *testing.T) {
	st := NewMemoryStore()
	ctx := context.Background()
	if err := st.Append(ctx, "conv-r", Message{Role: RoleUser, Parts: []Part{{Text: &TextPart{Text: "earlier question"}}}}); err != nil {
		t.Fatal(err)
	}
	m := &scriptModel{scripts: [][]StreamPart{textParts("answered with context")}}
	s := newSession(nil, m, "conv-r", sessionOpts{maxTurns: 5}, nil, SkillCatalog{})
	s.store = st
	s.mu.Lock()
	s.msgs, _ = st.Load(ctx, "conv-r")
	s.mu.Unlock()
	if err := s.Prompt(ctx, agentwire.Prompt{Text: "and now?"}); err != nil {
		t.Fatal(err)
	}
	collect(t, s)
	req := m.calls[0]
	var sawEarlier bool
	for _, msg := range req.Messages {
		for _, p := range msg.Parts {
			if p.Text != nil && p.Text.Text == "earlier question" {
				sawEarlier = true
			}
		}
	}
	if !sawEarlier {
		t.Fatal("the resumed session lost the earlier message")
	}
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestSummaryCompactorFolds pins the compactor contract.
func TestSummaryCompactorFolds(t *testing.T) {
	var msgs []Message
	for i := 0; i < 10; i++ {
		msgs = append(msgs, Message{Role: RoleUser, Parts: []Part{{Text: &TextPart{Text: fmt.Sprintf("message %d", i)}}}})
	}
	fake := &scriptModel{scripts: [][]StreamPart{textParts("the summary")}}
	c := SummaryCompactor(2)
	out, err := c.Compact(context.Background(), fake, msgs)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 {
		t.Fatalf("folded length = %d, want summary + 2", len(out))
	}
	if fake.callCount() != 1 {
		t.Fatalf("the summarizer made %d calls", fake.callCount())
	}
	if out[0].Parts[0].Text == nil || !strings.Contains(out[0].Parts[0].Text.Text, "the summary") {
		t.Fatalf("summary = %+v", out[0])
	}
}

// TestMCPOverInMemoryTransport drives the MCP tool set over an in-process
// server pair.
func TestMCPOverInMemoryTransport(t *testing.T) {
	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1.0.0"}, nil)
	server.AddTool(&mcp.Tool{Name: "ping", Description: "answer pong",
		InputSchema: &jsonschema.Schema{Type: "object"}},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "pong"}}}, nil
		})
	if _, err := server.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "agentwire-test", Version: "1.0.0"}, nil)
	cs, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	set := MCPSessionToolSet("trebi", cs)
	tools, err := set.Tools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0].Spec().Name != "mcp__trebi__ping" {
		t.Fatalf("tools = %+v", tools)
	}
	res, err := tools[0].Call(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Content) != 1 || res.Content[0].Text == nil || res.Content[0].Text.Text != "pong" {
		t.Fatalf("result = %+v", res)
	}
	if err := set.Close(); err != nil {
		t.Fatal(err)
	}
}
