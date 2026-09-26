package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"

	"github.com/trebi-ai/agent-wire"
	"github.com/trebi-ai/agent-wire/native"
)

// sseText is a text-only response.
const sseText = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-5","stop_reason":null,"usage":{"input_tokens":25,"output_tokens":1,"cache_read_input_tokens":5,"cache_creation_input_tokens":3}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}

event: message_stop
data: {"type":"message_stop"}
`

// sseThinking is a thinking block followed by one text block. The signature
// delta has no native carrier and must not reach the stream parts.
const sseThinking = `event: message_start
data: {"type":"message_start","message":{"id":"msg_2","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-5","usage":{"input_tokens":30,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"plan"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"ning"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sigT"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Answer"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9}}

event: message_stop
data: {"type":"message_stop"}
`

// sseToolUse is one text block and one tool_use block with a two-part input.
const sseToolUse = `event: message_start
data: {"type":"message_start","message":{"id":"msg_3","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-5","usage":{"input_tokens":40,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Let me check."}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_01abc","name":"get_weather","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"SF\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":12}}

event: message_stop
data: {"type":"message_stop"}
`

// capture records one request's headers and JSON body.
type capture struct {
	mu     sync.Mutex
	header http.Header
	body   map[string]any
}

// newFixture starts a server that answers every call with sse and records
// the request.
func newFixture(t *testing.T, sse string) (*httptest.Server, *capture) {
	t.Helper()
	cap := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		cap.mu.Lock()
		cap.header = r.Header.Clone()
		cap.body = map[string]any{}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &cap.body); err != nil {
				t.Errorf("decode request body: %v", err)
			}
		}
		cap.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	t.Cleanup(srv.Close)
	return srv, cap
}

// newModel builds a model pointed at the fixture server.
func newModel(t *testing.T, srv *httptest.Server, opts ...Option) native.Model {
	t.Helper()
	creds := agentwire.Credentials{
		Format:  "anthropic",
		APIKey:  "test-key-123",
		Headers: map[string]string{"x-trace": "t1"},
	}
	opts = append([]Option{WithBaseURL(srv.URL), WithHTTPClient(srv.Client())}, opts...)
	m, err := New(creds, "claude-sonnet-4-5", opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m
}

// run drains one stream and keeps the first error.
func run(m native.Model, req native.Request) ([]native.StreamPart, error) {
	var out []native.StreamPart
	for part, err := range m.Stream(context.Background(), req) {
		if err != nil {
			return out, err
		}
		out = append(out, part)
	}
	return out, nil
}

// mustRun drains one stream and fails on an error.
func mustRun(t *testing.T, m native.Model, req native.Request) []native.StreamPart {
	t.Helper()
	parts, err := run(m, req)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	return parts
}

func TestNewValidation(t *testing.T) {
	if _, err := New(agentwire.Credentials{Format: "openai", APIKey: "k"}, "m"); err == nil {
		t.Fatal("wrong format: want error, got nil")
	}
	if _, err := New(agentwire.Credentials{Format: "anthropic"}, "m"); err == nil {
		t.Fatal("empty key: want error, got nil")
	}
	if _, err := New(agentwire.Credentials{APIKey: "k"}, "m"); err != nil {
		t.Fatalf("empty format: %v", err)
	}
}

func TestStreamText(t *testing.T) {
	srv, _ := newFixture(t, sseText)
	m := newModel(t, srv)
	parts := mustRun(t, m, native.Request{Messages: []native.Message{{Role: native.RoleUser,
		Parts: []native.Part{{Text: &native.TextPart{Text: "Hi"}}}}}})
	want := []native.StreamPart{
		{Kind: native.StreamTextDelta, Delta: "Hel"},
		{Kind: native.StreamTextDelta, Delta: "lo"},
		{Kind: native.StreamFinish, Finish: &native.Finish{
			Reason: native.FinishStop,
			Raw:    "end_turn",
			Usage:  native.Usage{Input: 25, Output: 7, CacheRead: 5, CacheWrite: 3},
		}},
	}
	if !reflect.DeepEqual(parts, want) {
		t.Fatalf("parts = %+v, want %+v", parts, want)
	}
}

func TestStreamThinking(t *testing.T) {
	srv, _ := newFixture(t, sseThinking)
	m := newModel(t, srv)
	parts := mustRun(t, m, native.Request{Messages: []native.Message{{Role: native.RoleUser,
		Parts: []native.Part{{Text: &native.TextPart{Text: "Hi"}}}}}})
	want := []native.StreamPart{
		{Kind: native.StreamReasoningDelta, Delta: "plan"},
		{Kind: native.StreamReasoningDelta, Delta: "ning"},
		{Kind: native.StreamTextDelta, Delta: "Answer"},
		{Kind: native.StreamFinish, Finish: &native.Finish{
			Reason: native.FinishStop,
			Raw:    "end_turn",
			Usage:  native.Usage{Input: 30, Output: 9},
		}},
	}
	if !reflect.DeepEqual(parts, want) {
		t.Fatalf("parts = %+v, want %+v", parts, want)
	}
}

func TestStreamToolUse(t *testing.T) {
	srv, _ := newFixture(t, sseToolUse)
	m := newModel(t, srv)
	parts := mustRun(t, m, native.Request{Messages: []native.Message{{Role: native.RoleUser,
		Parts: []native.Part{{Text: &native.TextPart{Text: "Weather?"}}}}}})
	want := []native.StreamPart{
		{Kind: native.StreamTextDelta, Delta: "Let me check."},
		{Kind: native.StreamToolInputStart, ID: "toolu_01abc"},
		{Kind: native.StreamToolInputDelta, ID: "toolu_01abc", Delta: `{"city":`},
		{Kind: native.StreamToolInputDelta, ID: "toolu_01abc", Delta: `"SF"}`},
		{Kind: native.StreamToolCallDone, Call: &native.ToolCall{
			ID:    "toolu_01abc",
			Name:  "get_weather",
			Input: json.RawMessage(`{"city":"SF"}`),
		}},
		{Kind: native.StreamFinish, Finish: &native.Finish{
			Reason: native.FinishToolCalls,
			Raw:    "tool_use",
			Usage:  native.Usage{Input: 40, Output: 12},
		}},
	}
	if !reflect.DeepEqual(parts, want) {
		t.Fatalf("parts = %+v, want %+v", parts, want)
	}
}

// reqBody fetches the captured request body after one stream run.
func reqBody(t *testing.T, srv *httptest.Server, cap *capture, opts ...Option) map[string]any {
	t.Helper()
	m := newModel(t, srv, opts...)
	mustRun(t, m, native.Request{System: "be brief", Effort: "max",
		Messages: []native.Message{{Role: native.RoleUser,
			Parts: []native.Part{{Text: &native.TextPart{Text: "Hi"}}}}}})
	cap.mu.Lock()
	defer cap.mu.Unlock()
	return cap.body
}

func num(t *testing.T, body map[string]any, key string) float64 {
	t.Helper()
	v, ok := body[key].(float64)
	if !ok {
		t.Fatalf("body[%s] = %#v, want a number", key, body[key])
	}
	return v
}

func TestWireHeaders(t *testing.T) {
	srv, cap := newFixture(t, sseText)
	m := newModel(t, srv)
	mustRun(t, m, native.Request{})
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if got := cap.header.Get("x-api-key"); got != "test-key-123" {
		t.Fatalf("x-api-key = %q", got)
	}
	if got := cap.header.Get("x-trace"); got != "t1" {
		t.Fatalf("x-trace = %q", got)
	}
}

func TestWireRequest(t *testing.T) {
	srv, cap := newFixture(t, sseText)
	m := newModel(t, srv, WithWebSearch(true))
	req := native.Request{
		System:          "be brief",
		MaxOutputTokens: 32768,
		Effort:          "high",
		Messages: []native.Message{
			{Role: native.RoleUser, Parts: []native.Part{
				{Text: &native.TextPart{Text: "Hi"}},
				{File: &native.FilePart{MediaType: "image/png",
					Data: []byte{1, 2, 3}}},
			}},
			{Role: native.RoleAssistant, Parts: []native.Part{
				{Reasoning: &native.ReasoningPart{Text: "hmm",
					ProviderMeta: json.RawMessage(`{"signature":"sigA"}`)}},
				{ToolCall: &native.ToolCall{ID: "toolu_9", Name: "get_weather",
					Input: json.RawMessage(`{"city":"SF"}`)}},
			}},
			{Role: native.RoleTool, Parts: []native.Part{
				{ToolResult: &native.ToolResult{CallID: "toolu_9", Name: "get_weather",
					IsError: true,
					Content: []native.Part{{Text: &native.TextPart{Text: "nope"}}}}},
			}},
		},
		Tools: []native.ToolSpec{{
			Name:        "get_weather",
			Description: "Reads the weather",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"],"additionalProperties":false}`),
		}},
		ToolChoice: native.ToolChoice{Mode: native.ToolNamed, Name: "get_weather"},
	}
	mustRun(t, m, req)

	cap.mu.Lock()
	defer cap.mu.Unlock()
	body := cap.body
	if body["model"] != "claude-sonnet-4-5" {
		t.Fatalf("model = %#v", body["model"])
	}
	if num(t, body, "max_tokens") != 32768 {
		t.Fatalf("max_tokens = %#v", body["max_tokens"])
	}
	system, ok := body["system"].([]any)
	if !ok || len(system) != 1 || system[0].(map[string]any)["text"] != "be brief" {
		t.Fatalf("system = %#v", body["system"])
	}

	msgs, ok := body["messages"].([]any)
	if !ok || len(msgs) != 3 {
		t.Fatalf("messages = %#v", body["messages"])
	}
	user := msgs[0].(map[string]any)
	if user["role"] != "user" {
		t.Fatalf("role = %#v", user["role"])
	}
	blocks := user["content"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("user content = %#v", user["content"])
	}
	img := blocks[1].(map[string]any)
	if img["type"] != "image" {
		t.Fatalf("block 1 = %#v", img)
	}
	src := img["source"].(map[string]any)
	if src["type"] != "base64" || src["media_type"] != "image/png" || src["data"] != "AQID" {
		t.Fatalf("image source = %#v", src)
	}

	// The assistant replay carries the thinking signature and the tool call.
	asst := msgs[1].(map[string]any)
	ablocks := asst["content"].([]any)
	think := ablocks[0].(map[string]any)
	if think["type"] != "thinking" || think["thinking"] != "hmm" || think["signature"] != "sigA" {
		t.Fatalf("thinking block = %#v", think)
	}
	call := ablocks[1].(map[string]any)
	if call["type"] != "tool_use" || call["id"] != "toolu_9" || call["name"] != "get_weather" {
		t.Fatalf("tool_use block = %#v", call)
	}
	if input, _ := json.Marshal(call["input"]); string(input) != `{"city":"SF"}` {
		t.Fatalf("tool input = %s", input)
	}

	// A tool message becomes a user message with a tool_result block.
	tool := msgs[2].(map[string]any)
	if tool["role"] != "user" {
		t.Fatalf("tool message role = %#v", tool["role"])
	}
	result := tool["content"].([]any)[0].(map[string]any)
	if result["type"] != "tool_result" || result["tool_use_id"] != "toolu_9" || result["is_error"] != true {
		t.Fatalf("tool_result block = %#v", result)
	}

	// The tool schema keeps the typed keys and passes the rest through.
	tools := body["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools = %#v", tools)
	}
	custom := tools[0].(map[string]any)
	if custom["name"] != "get_weather" || custom["description"] != "Reads the weather" {
		t.Fatalf("custom tool = %#v", custom)
	}
	schema := custom["input_schema"].(map[string]any)
	if schema["type"] != "object" {
		t.Fatalf("schema type = %#v", schema["type"])
	}
	if props, _ := json.Marshal(schema["properties"]); string(props) != `{"city":{"type":"string"}}` {
		t.Fatalf("schema properties = %s", props)
	}
	req_, _ := json.Marshal(schema["required"])
	if string(req_) != `["city"]` {
		t.Fatalf("schema required = %s", req_)
	}
	if schema["additionalProperties"] != false {
		t.Fatalf("schema extra = %#v", schema["additionalProperties"])
	}
	search := tools[1].(map[string]any)
	if search["type"] != "web_search_20250305" || num(t, search, "max_uses") != 3 {
		t.Fatalf("web search tool = %#v", search)
	}

	choice := body["tool_choice"].(map[string]any)
	if choice["type"] != "tool" || choice["name"] != "get_weather" {
		t.Fatalf("tool_choice = %#v", choice)
	}
	thinking := body["thinking"].(map[string]any)
	if thinking["type"] != "enabled" || num(t, thinking, "budget_tokens") != 16384 {
		t.Fatalf("thinking = %#v", thinking)
	}
}

func TestToolChoiceModes(t *testing.T) {
	cases := []struct {
		choice native.ToolChoice
		want   map[string]any
	}{
		{native.ToolChoice{}, map[string]any{"type": "auto"}},
		{native.ToolChoice{Mode: native.ToolNone}, map[string]any{"type": "none"}},
		{native.ToolChoice{Mode: native.ToolRequired}, map[string]any{"type": "any"}},
	}
	for _, tc := range cases {
		srv, cap := newFixture(t, sseText)
		m := newModel(t, srv)
		req := native.Request{
			Messages: []native.Message{{Role: native.RoleUser,
				Parts: []native.Part{{Text: &native.TextPart{Text: "Hi"}}}}},
			Tools:      []native.ToolSpec{{Name: "t", InputSchema: json.RawMessage(`{}`)}},
			ToolChoice: tc.choice,
		}
		mustRun(t, m, req)
		cap.mu.Lock()
		got := cap.body["tool_choice"]
		cap.mu.Unlock()
		if !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("mode %d: tool_choice = %#v, want %#v", tc.choice.Mode, got, tc.want)
		}
	}
}

func TestNoToolsOmitsChoice(t *testing.T) {
	srv, cap := newFixture(t, sseText)
	m := newModel(t, srv)
	mustRun(t, m, native.Request{Messages: []native.Message{{Role: native.RoleUser,
		Parts: []native.Part{{Text: &native.TextPart{Text: "Hi"}}}}}})
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if _, ok := cap.body["tool_choice"]; ok {
		t.Fatalf("tool_choice = %#v, want omitted", cap.body["tool_choice"])
	}
}

func TestParamErrors(t *testing.T) {
	srv, _ := newFixture(t, sseText)
	m := newModel(t, srv)
	user := []native.Message{{Role: native.RoleUser,
		Parts: []native.Part{{Text: &native.TextPart{Text: "Hi"}}}}}

	if _, err := run(m, native.Request{Messages: user, Effort: "bogus"}); err == nil {
		t.Fatal("unknown effort: want error, got nil")
	}
	// A max at the thinking floor leaves no room for a budget.
	if _, err := run(m, native.Request{Messages: user, Effort: "low", MaxOutputTokens: 1024}); err == nil {
		t.Fatal("thinking budget over max_tokens: want error, got nil")
	}
	req := native.Request{Messages: user, Tools: []native.ToolSpec{{Name: "t"}}}
	req.ToolChoice = native.ToolChoice{Mode: native.ToolNamed}
	if _, err := run(m, req); err == nil {
		t.Fatal("named choice without name: want error, got nil")
	}
}

func TestDefaultMaxTokensAndBudgetClamp(t *testing.T) {
	srv, cap := newFixture(t, sseText)
	body := reqBody(t, srv, cap)
	if num(t, body, "max_tokens") != defaultMaxTokens {
		t.Fatalf("max_tokens = %#v", body["max_tokens"])
	}
	thinking := body["thinking"].(map[string]any)
	// effort max clamps to the default max minus one.
	if num(t, thinking, "budget_tokens") != float64(defaultMaxTokens-1) {
		t.Fatalf("budget = %#v", thinking["budget_tokens"])
	}
}
