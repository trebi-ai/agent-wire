package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/trebi-ai/agent-wire"
	"github.com/trebi-ai/agent-wire/native"
)

// chunkJSON builds one chat.completion.chunk SSE frame.
func chunkJSON(t *testing.T, body string) string {
	t.Helper()
	chunk := map[string]any{
		"id":      "chatcmpl-test",
		"object":  "chat.completion.chunk",
		"created": 1,
		"model":   "gpt-test",
		"choices": []map[string]any{
			{"index": 0, "delta": json.RawMessage(body)},
		},
	}
	raw, err := json.Marshal(chunk)
	if err != nil {
		t.Fatalf("marshal chunk: %v", err)
	}
	return "data: " + string(raw) + "\n\n"
}

// finishChunkJSON builds the chunk that carries the finish reason. The
// reason sits on the choice, not inside the delta.
func finishChunkJSON(t *testing.T, reason string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"id":      "chatcmpl-test",
		"object":  "chat.completion.chunk",
		"created": 1,
		"model":   "gpt-test",
		"choices": []map[string]any{
			{"index": 0, "delta": map[string]any{}, "finish_reason": reason},
		},
	})
	if err != nil {
		t.Fatalf("marshal finish chunk: %v", err)
	}
	return "data: " + string(raw) + "\n\n"
}

// usageChunkJSON builds the terminal usage chunk. It carries no choices.
func usageChunkJSON(t *testing.T, prompt, completion, cached, reasoning int64) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"id":      "chatcmpl-test",
		"object":  "chat.completion.chunk",
		"created": 1,
		"model":   "gpt-test",
		"choices": []any{},
		"usage": map[string]any{
			"prompt_tokens":     prompt,
			"completion_tokens": completion,
			"total_tokens":      prompt + completion,
			"prompt_tokens_details": map[string]any{
				"cached_tokens": cached,
			},
			"completion_tokens_details": map[string]any{
				"reasoning_tokens": reasoning,
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal usage chunk: %v", err)
	}
	return "data: " + string(raw) + "\n\n"
}

// recorded holds what the fixture server saw.
type recorded struct {
	Authorization string
	Header        http.Header
	Body          map[string]any
}

// newFixtureServer starts an SSE server that replays frames and records the
// request. It returns the base URL for the provider and the recording.
func newFixtureServer(t *testing.T, frames []string, rec *recorded) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.NotFound(w, r)
			return
		}
		rec.Authorization = r.Header.Get("Authorization")
		rec.Header = r.Header.Clone()
		if err := json.NewDecoder(r.Body).Decode(&rec.Body); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, frame := range frames {
			fmt.Fprint(w, frame)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// collect drains one stream into parts and the terminal error.
func collect(t *testing.T, seq native.Stream) ([]native.StreamPart, error) {
	t.Helper()
	var parts []native.StreamPart
	for part, err := range seq {
		if err != nil {
			return parts, err
		}
		parts = append(parts, part)
	}
	return parts, nil
}

// kinds lists the kind of every part.
func kinds(parts []native.StreamPart) []native.StreamKind {
	out := make([]native.StreamKind, 0, len(parts))
	for _, p := range parts {
		out = append(out, p.Kind)
	}
	return out
}

// finishOf returns the single finish part.
func finishOf(t *testing.T, parts []native.StreamPart) native.Finish {
	t.Helper()
	var found []native.Finish
	for _, p := range parts {
		if p.Kind == native.StreamFinish {
			if p.Finish == nil {
				t.Fatal("finish part carries no finish")
			}
			found = append(found, *p.Finish)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one finish part, got %d", len(found))
	}
	return found[0]
}

func TestNewRejectsWrongFormat(t *testing.T) {
	if _, err := New(agentwire.Credentials{Format: "anthropic", APIKey: "k"}, "m"); err == nil {
		t.Fatal("want an error for the anthropic format")
	}
}

func TestNewRejectsEmptyKey(t *testing.T) {
	if _, err := New(agentwire.Credentials{Format: "openai"}, "m"); err == nil {
		t.Fatal("want an error for the empty API key")
	}
	if _, err := New(agentwire.Credentials{Format: "", APIKey: "k"}, "m"); err != nil {
		t.Fatalf("empty format is valid, got %v", err)
	}
}

func TestStreamTextOnly(t *testing.T) {
	frames := []string{
		chunkJSON(t, `{"role":"assistant","content":"He"}`),
		chunkJSON(t, `{"content":"llo"}`),
		finishChunkJSON(t, "stop"),
		usageChunkJSON(t, 11, 7, 4, 2),
	}
	rec := &recorded{}
	base := newFixtureServer(t, frames, rec)
	m, err := New(agentwire.Credentials{
		Format:  "openai",
		APIKey:  "test-key",
		BaseURL: base,
		Headers: map[string]string{"X-Tenant": "tenant-1"},
	}, "gpt-test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	parts, err := collect(t, m.Stream(context.Background(), native.Request{
		System:   "Be brief.",
		Messages: []native.Message{{Role: native.RoleUser, Parts: []native.Part{{Text: &native.TextPart{Text: "Hi"}}}}},
	}))
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	var deltas []string
	for _, p := range parts {
		if p.Kind == native.StreamTextDelta {
			deltas = append(deltas, p.Delta)
		}
	}
	if strings.Join(deltas, "") != "Hello" {
		t.Fatalf("deltas fold to %q, want %q", strings.Join(deltas, ""), "Hello")
	}
	finish := finishOf(t, parts)
	if finish.Reason != native.FinishStop || finish.Raw != "stop" {
		t.Fatalf("finish = %q (raw %q), want stop", finish.Reason, finish.Raw)
	}
	want := native.Usage{Input: 11, Output: 7, CacheRead: 4, Reasoning: 2}
	if finish.Usage != want {
		t.Fatalf("usage = %+v, want %+v", finish.Usage, want)
	}
	// The server saw the bearer credential, the extra header, the system
	// prompt, and the user message.
	if rec.Authorization != "Bearer test-key" {
		t.Fatalf("authorization = %q", rec.Authorization)
	}
	if rec.Header.Get("X-Tenant") != "tenant-1" {
		t.Fatalf("extra header missing: %v", rec.Header)
	}
	msgs, _ := rec.Body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %v, want system plus user", msgs)
	}
	if role, _ := msgs[0].(map[string]any)["role"].(string); role != "system" {
		t.Fatalf("first message role = %v, want system", msgs[0])
	}
	if role, _ := msgs[1].(map[string]any)["role"].(string); role != "user" {
		t.Fatalf("second message role = %v, want user", msgs[1])
	}
}

func TestStreamParallelToolCalls(t *testing.T) {
	frames := []string{
		chunkJSON(t, `{"role":"assistant"}`),
		chunkJSON(t, `{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"get_weather","arguments":""}}]}`),
		chunkJSON(t, `{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"get_time","arguments":""}}]}`),
		chunkJSON(t, `{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]}`),
		chunkJSON(t, `{"tool_calls":[{"index":1,"function":{"arguments":"{\"tz\":"}}]}`),
		chunkJSON(t, `{"tool_calls":[{"index":0,"function":{"arguments":"\"SF\"}"}}]}`),
		chunkJSON(t, `{"tool_calls":[{"index":1,"function":{"arguments":"\"UTC\"}"}}]}`),
		chunkJSON(t, `{"content":"Calling tools."}`),
		finishChunkJSON(t, "tool_calls"),
		usageChunkJSON(t, 20, 9, 0, 0),
	}
	rec := &recorded{}
	base := newFixtureServer(t, frames, rec)
	m, err := New(agentwire.Credentials{APIKey: "test-key", BaseURL: base}, "gpt-test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	parts, err := collect(t, m.Stream(context.Background(), native.Request{
		Messages: []native.Message{
			{Role: native.RoleUser, Parts: []native.Part{{Text: &native.TextPart{Text: "Weather and time?"}}}},
			{Role: native.RoleTool, Parts: []native.Part{{ToolResult: &native.ToolResult{CallID: "call_a", Content: []native.Part{{Text: &native.TextPart{Text: "sunny"}}}}}}},
		},
		Tools: []native.ToolSpec{{
			Name:        "get_weather",
			Description: "Reads the weather.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
		}},
		MaxOutputTokens: 512,
		Effort:          "max",
	}))
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	// Two tool input deltas per call, one per shard fragment.
	var inputDeltas int
	for _, p := range parts {
		if p.Kind == native.StreamToolInputDelta {
			inputDeltas++
		}
	}
	if inputDeltas != 4 {
		t.Fatalf("tool input deltas = %d, want 4", inputDeltas)
	}
	var done []native.ToolCall
	for _, p := range parts {
		if p.Kind == native.StreamToolCallDone {
			if p.Call == nil {
				t.Fatal("done part carries no call")
			}
			done = append(done, *p.Call)
		}
	}
	if len(done) != 2 {
		t.Fatalf("done calls = %d, want 2", len(done))
	}
	if done[0].ID != "call_a" || done[0].Name != "get_weather" || string(done[0].Input) != `{"city":"SF"}` {
		t.Fatalf("first call = %+v", done[0])
	}
	if done[1].ID != "call_b" || done[1].Name != "get_time" || string(done[1].Input) != `{"tz":"UTC"}` {
		t.Fatalf("second call = %+v", done[1])
	}
	finish := finishOf(t, parts)
	if finish.Reason != native.FinishToolCalls || finish.Raw != "tool_calls" {
		t.Fatalf("finish = %q (raw %q), want tool_calls", finish.Reason, finish.Raw)
	}
	if finish.Usage.Input != 20 || finish.Usage.Output != 9 {
		t.Fatalf("usage = %+v, want 20 in and 9 out", finish.Usage)
	}
	// Exactly one finish part sits at the end.
	finishCount := 0
	for _, p := range parts {
		if p.Kind == native.StreamFinish {
			finishCount++
		}
	}
	if finishCount != 1 {
		t.Fatalf("finish parts = %d, want 1", finishCount)
	}
	// The request mapped tools, choice, budget, and effort.
	toolsJSON, _ := json.Marshal(rec.Body["tools"])
	if !strings.Contains(string(toolsJSON), `"get_weather"`) ||
		!strings.Contains(string(toolsJSON), `"required":["city"]`) {
		t.Fatalf("tools not passed through: %s", toolsJSON)
	}
	if choice, _ := rec.Body["tool_choice"].(string); choice != "auto" {
		t.Fatalf("tool_choice = %v, want auto", rec.Body["tool_choice"])
	}
	if max, _ := rec.Body["max_tokens"].(float64); max != 512 {
		t.Fatalf("max_tokens = %v, want 512", rec.Body["max_tokens"])
	}
	if effort, _ := rec.Body["reasoning_effort"].(string); effort != "high" {
		t.Fatalf("reasoning_effort = %v, want high", rec.Body["reasoning_effort"])
	}
	// The tool result replayed as a role tool message with its call id.
	msgs, _ := rec.Body["messages"].([]any)
	var sawTool bool
	for _, msg := range msgs {
		m, _ := msg.(map[string]any)
		if m == nil || m["role"] != "tool" {
			continue
		}
		sawTool = true
		if m["tool_call_id"] != "call_a" {
			t.Fatalf("tool_call_id = %v, want call_a", m["tool_call_id"])
		}
		if m["content"] != "sunny" {
			t.Fatalf("tool content = %v, want sunny", m["content"])
		}
	}
	if !sawTool {
		t.Fatalf("no tool message in %v", msgs)
	}
}

func TestStreamNamedToolChoiceAndNone(t *testing.T) {
	frames := []string{finishChunkJSON(t, "stop"), usageChunkJSON(t, 1, 1, 0, 0)}
	cases := []struct {
		choice native.ToolChoice
		want   any
	}{
		{native.ToolChoice{Mode: native.ToolNone}, "none"},
		{native.ToolChoice{Mode: native.ToolRequired}, "required"},
		{native.ToolChoice{Mode: native.ToolNamed, Name: "get_weather"}, map[string]any{
			"type":     "function",
			"function": map[string]any{"name": "get_weather"},
		}},
	}
	for _, tc := range cases {
		rec := &recorded{}
		base := newFixtureServer(t, frames, rec)
		m, err := New(agentwire.Credentials{APIKey: "test-key", BaseURL: base}, "gpt-test")
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		parts, err := collect(t, m.Stream(context.Background(), native.Request{
			Messages:   []native.Message{{Role: native.RoleUser, Parts: []native.Part{{Text: &native.TextPart{Text: "go"}}}}},
			ToolChoice: tc.choice,
		}))
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		if finishOf(t, parts).Reason != native.FinishStop {
			t.Fatal("want a stop finish")
		}
		got := rec.Body["tool_choice"]
		if !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("tool_choice = %v, want %v", got, tc.want)
		}
	}
}
