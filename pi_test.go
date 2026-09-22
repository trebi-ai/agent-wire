package agentwire

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/trebi-ai/agent-wire/internal/wire"
)

// This file tests the pi RPC protocol: the recorded lifecycle fixture, message
// blocks, streaming updates, provider errors, the permission policy, the
// attachment refusal and the first-prompt instruction block.

// cpTestPiFixtureLines mirror a recorded `pi --mode rpc` capture (pi 0.84.1):
// response ack, agent and turn lifecycle, a user message, one assistant
// message, turn_end, agent_end, agent_settled.
var cpTestPiFixtureLines = []string{
	`{"type":"response","command":"prompt","success":true}`,
	`{"type":"agent_start"}`,
	`{"type":"turn_start"}`,
	`{"type":"message_start","message":{"role":"user","content":[{"type":"text","text":"Reply with exactly PONG."}],"timestamp":1}}`,
	`{"type":"message_end","message":{"role":"user","content":[{"type":"text","text":"Reply with exactly PONG."}],"timestamp":1}}`,
	`{"type":"message_start","message":{"role":"assistant","content":[],"provider":"xai","model":"grok-4.5","usage":{"input":1,"output":2,"cacheRead":3,"cacheWrite":0,"totalTokens":6,"cost":{"total":0.001}},"stopReason":null,"timestamp":2}}`,
	`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"PONG"}],"provider":"xai","model":"grok-4.5","usage":{"input":1,"output":2,"cacheRead":3,"cacheWrite":0,"totalTokens":6,"cost":{"total":0.001}},"stopReason":"stop","timestamp":2}}`,
	`{"type":"turn_end","message":{"role":"assistant","content":[{"type":"text","text":"PONG"}]},"toolResults":[]}`,
	`{"type":"agent_end","messages":[],"willRetry":false}`,
	`{"type":"agent_settled"}`,
}

// cpTestPiProto builds a pi protocol with no writer.
func cpTestPiProto(policy PermissionPolicy, instructions string) *piProtocol {
	return newPiProtocol(policy, instructions)
}

// cpTestPiSession builds a pi protocol behind an in-memory wire session.
// Every frame the protocol writes lands in the returned buffer.
func cpTestPiSession(t *testing.T, policy PermissionPolicy) (*piProtocol, *bytes.Buffer, *wire.Session) {
	t.Helper()
	stdin := &bytes.Buffer{}
	proto := newPiProtocol(policy, "")
	s := wire.NewMemorySession("pi", proto, stdin, strings.NewReader(""),
		context.Background(), wire.Config{})
	return proto, stdin, s
}

// cpTestPiPromptMessage returns the `message` field of an EncodePrompt frame.
func cpTestPiPromptMessage(t *testing.T, proto *piProtocol, text string) string {
	t.Helper()
	frame, err := proto.EncodePrompt(Prompt{Text: text})
	if err != nil {
		t.Fatalf("encode prompt: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(frame, &m); err != nil {
		t.Fatalf("decode prompt frame: %v", err)
	}
	if m["type"] != "prompt" {
		t.Fatalf("prompt frame type = %v", m["type"])
	}
	msg, ok := m["message"].(string)
	if !ok {
		t.Fatalf("prompt message is not a string: %#v", m["message"])
	}
	return msg
}

// TestPiFixtureLifecycle pins the recorded turn: status frames, one assistant
// message, one usage event, and a success result. User-role messages never
// surface, and the quiet frames stay quiet.
func TestPiFixtureLifecycle(t *testing.T) {
	t.Parallel()
	proto := cpTestPiProto(PermissionPolicy{}, "")
	var evs []Event
	for _, line := range cpTestPiFixtureLines {
		got := proto.Parse([]byte(line))
		switch {
		case strings.Contains(line, `"turn_end"`),
			strings.Contains(line, `"agent_settled"`),
			strings.Contains(line, `"response"`):
			if len(got) != 0 {
				t.Fatalf("frame %s produced events: %+v", line, got)
			}
		}
		evs = append(evs, got...)
	}

	if got := cpTestCountEvents(evs, EventUser); got != 0 {
		t.Fatalf("user-role messages must not surface: %+v", evs)
	}
	if got := cpTestCountEvents(evs, EventAssistant); got != 1 {
		t.Fatalf("assistant events = %d, want 1: %+v", got, evs)
	}
	if a := cpTestFirstEvent(evs, EventAssistant); a.Text != "PONG" || a.Delta {
		t.Fatalf("assistant event = %+v", a)
	}
	if got := cpTestCountEvents(evs, EventStatus); got != 2 {
		t.Fatalf("status events = %d, want 2 (agent_start, turn_start): %+v", got, evs)
	}
	for _, e := range evs {
		if e.Type == EventStatus && e.Status != StatusRunning {
			t.Fatalf("status event = %+v", e)
		}
	}

	if got := cpTestCountEvents(evs, EventUsage); got != 1 {
		t.Fatalf("usage events = %d, want 1 (message_end only): %+v", got, evs)
	}
	usage := cpTestFirstEvent(evs, EventUsage)
	if usage.Usage == nil {
		t.Fatalf("usage event has no payload: %+v", usage)
	}
	u := usage.Usage
	if u.Input != 1 || u.Output != 2 || u.CacheRead != 3 || u.CacheCreation != 0 {
		t.Fatalf("usage = %+v", u)
	}
	if u.CostUSD != 0.001 {
		t.Fatalf("usage cost = %v", u.CostUSD)
	}

	res := cpTestFirstEvent(evs, EventResult)
	if res == nil || res.Result == nil {
		t.Fatalf("no result event: %+v", evs)
	}
	if res.Result.IsError || res.Result.Subtype != "success" {
		t.Fatalf("result = %+v", res.Result)
	}

	if msg := cpTestPiPromptMessage(t, proto, "hi"); msg != "hi" {
		t.Fatalf("prompt message = %q", msg)
	}
}

// TestPiMessageBlocks pins message_start and message_end content: assistant
// text, toolCall and toolResult blocks, and usage on message_end only.
func TestPiMessageBlocks(t *testing.T) {
	t.Parallel()
	proto := cpTestPiProto(PermissionPolicy{}, "")

	start := proto.Parse([]byte(`{"type":"message_start","message":{"role":"assistant","content":[{"type":"toolCall","id":"call_1","name":"bash","arguments":{"command":"ls"}}]}}`))
	if len(start) != 1 || start[0].Type != EventTool || start[0].Tool == nil {
		t.Fatalf("toolCall start events = %+v", start)
	}
	tool := start[0].Tool
	if tool.ID != "call_1" || tool.Name != "bash" || tool.Kind != ToolExec || tool.Status != "started" {
		t.Fatalf("toolCall block = %+v", tool)
	}
	if tool.Input != `{"command":"ls"}` {
		t.Fatalf("toolCall input = %q", tool.Input)
	}
	if got := cpTestCountEvents(start, EventUsage); got != 0 {
		t.Fatalf("message_start produced usage: %+v", start)
	}

	end := proto.Parse([]byte(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"all done"},{"type":"toolResult","id":"call_1","name":"bash","content":[{"type":"text","text":"ok"}]}],"usage":{"input":10,"output":4,"cacheRead":2,"cacheWrite":1,"cost":{"total":0.25}}}}`))
	assistant := cpTestFirstEvent(end, EventAssistant)
	if assistant == nil || assistant.Text != "all done" || assistant.Delta {
		t.Fatalf("assistant block = %+v", end)
	}
	done := cpTestFirstEvent(end, EventTool)
	if done == nil || done.Tool == nil {
		t.Fatalf("toolResult block = %+v", end)
	}
	if done.Tool.ID != "call_1" || done.Tool.Name != "bash" || done.Tool.Status != "completed" {
		t.Fatalf("toolResult block = %+v", done.Tool)
	}
	if done.Tool.Output != "ok" {
		t.Fatalf("toolResult output = %q", done.Tool.Output)
	}
	usage := cpTestFirstEvent(end, EventUsage)
	if usage == nil || usage.Usage == nil {
		t.Fatalf("message_end produced no usage: %+v", end)
	}
	if usage.Usage.Input != 10 || usage.Usage.Output != 4 || usage.Usage.CacheRead != 2 || usage.Usage.CacheCreation != 1 {
		t.Fatalf("message_end usage = %+v", usage.Usage)
	}
	if usage.Usage.CostUSD != 0.25 {
		t.Fatalf("message_end cost = %v", usage.Usage.CostUSD)
	}

	// A user-role message yields nothing at all.
	if evs := proto.Parse([]byte(`{"type":"message_start","message":{"role":"user","content":[{"type":"text","text":"hi"}]}}`)); len(evs) != 0 {
		t.Fatalf("user message events = %+v", evs)
	}
}

// TestPiMessageUpdateDelta pins the streaming update shape: text_delta gives a
// delta event, and a tool update gives a tool start.
func TestPiMessageUpdateDelta(t *testing.T) {
	t.Parallel()
	proto := cpTestPiProto(PermissionPolicy{}, "")

	evs := proto.Parse([]byte(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"PO"}}`))
	if len(evs) != 1 || evs[0].Type != EventAssistant {
		t.Fatalf("delta events = %+v", evs)
	}
	if evs[0].Text != "PO" || !evs[0].Delta {
		t.Fatalf("delta event = %+v", evs[0])
	}

	evs = proto.Parse([]byte(`{"type":"message_update","assistantMessageEvent":{"type":"tool_use","id":"call_9","name":"Edit"}}`))
	if len(evs) != 1 || evs[0].Type != EventTool || evs[0].Tool == nil {
		t.Fatalf("tool update events = %+v", evs)
	}
	if evs[0].Tool.ID != "call_9" || evs[0].Tool.Kind != ToolEdit || evs[0].Tool.Status != "started" {
		t.Fatalf("tool update = %+v", evs[0].Tool)
	}

	if evs := proto.Parse([]byte(`{"type":"message_update","assistantMessageEvent":{"type":"message_stop"}}`)); len(evs) != 0 {
		t.Fatalf("unknown update events = %+v", evs)
	}
}

// TestPiErrorFrameAndFailedResult pins the failure shape: an error frame is
// classified and remembered, so agent_end reports a failed result instead of a
// silent success.
func TestPiErrorFrameAndFailedResult(t *testing.T) {
	t.Parallel()
	proto := cpTestPiProto(PermissionPolicy{}, "")

	evs := proto.Parse([]byte(`{"type":"error","error":"invalid api key"}`))
	if len(evs) != 1 || evs[0].Type != EventError {
		t.Fatalf("error frame events = %+v", evs)
	}
	if evs[0].EndReason != "auth" || !strings.HasPrefix(evs[0].Code, "pi.") {
		t.Fatalf("error frame classification = %+v", evs[0])
	}

	end := proto.Parse([]byte(`{"type":"agent_end","messages":[],"willRetry":false}`))
	res := cpTestFirstEvent(end, EventResult)
	if res == nil || res.Result == nil {
		t.Fatalf("agent_end events = %+v", end)
	}
	if !res.Result.IsError || res.Result.EndReason != "auth" {
		t.Fatalf("failed result = %+v", res.Result)
	}
	if !strings.HasPrefix(res.Result.Code, "pi.") {
		t.Fatalf("failed result code = %q", res.Result.Code)
	}
}

// TestPiProviderErrorOnMessageEnd pins the real provider failure shape: the
// error rides on the assistant message and must still fail the turn.
func TestPiProviderErrorOnMessageEnd(t *testing.T) {
	t.Parallel()
	proto := cpTestPiProto(PermissionPolicy{}, "")
	lines := []string{
		`{"type":"agent_start"}`,
		`{"type":"turn_start"}`,
		`{"type":"message_start","message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"OAuth refresh failed for xai: invalid_grant"}}`,
		`{"type":"message_end","message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"OAuth refresh failed for xai: invalid_grant"}}`,
		`{"type":"turn_end","message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"OAuth refresh failed for xai: invalid_grant"},"toolResults":[]}`,
		`{"type":"agent_end","messages":[],"willRetry":false}`,
	}
	var evs []Event
	for _, line := range lines {
		evs = append(evs, proto.Parse([]byte(line))...)
	}
	res := cpTestFirstEvent(evs, EventResult)
	if res == nil || res.Result == nil || !res.Result.IsError {
		t.Fatalf("result = %+v", res)
	}
	if res.Result.EndReason != "auth" {
		t.Fatalf("end reason = %q (%s)", res.Result.EndReason, res.Result.Text)
	}
	if !strings.Contains(res.Result.Text, "invalid_grant") {
		t.Fatalf("result text = %q", res.Result.Text)
	}
}

// TestPiPermissionPolicy pins both permission paths: auto answers on the wire,
// ask surfaces the request and the session writes the decision.
func TestPiPermissionPolicy(t *testing.T) {
	t.Parallel()
	const request = `{"type":"permission_request","id":"perm_1","tool":"bash","message":"Allow bash?"}`

	t.Run("auto answers on the wire", func(t *testing.T) {
		t.Parallel()
		proto, stdin, _ := cpTestPiSession(t, PermissionPolicy{Mode: PermissionAuto})
		if evs := proto.Parse([]byte(request)); len(evs) != 0 {
			t.Fatalf("auto answer surfaced events: %+v", evs)
		}
		frames := cpTestFramesOfType(t, stdin, "permission_response")
		if len(frames) != 1 {
			t.Fatalf("permission responses = %v", frames)
		}
		if frames[0]["id"] != "perm_1" || frames[0]["approved"] != true {
			t.Fatalf("auto answer frame = %+v", frames[0])
		}
	})

	t.Run("ask surfaces the request", func(t *testing.T) {
		t.Parallel()
		proto, stdin, s := cpTestPiSession(t, PermissionPolicy{})
		evs := proto.Parse([]byte(request))
		perm := cpTestFirstEvent(evs, EventPermission)
		if perm == nil || perm.Permission == nil {
			t.Fatalf("no permission event: %+v", evs)
		}
		if perm.Permission.ID != "perm_1" || perm.Permission.Tool != "bash" || perm.Permission.Kind != ToolExec {
			t.Fatalf("permission = %+v", perm.Permission)
		}
		if perm.Permission.Question != "Allow bash?" {
			t.Fatalf("permission question = %q", perm.Permission.Question)
		}
		if frames := cpTestFramesOfType(t, stdin, "permission_response"); len(frames) != 0 {
			t.Fatalf("ask mode wrote a response: %v", frames)
		}

		if err := s.AnswerPermission(context.Background(), "perm_1", Decision{Allow: false, Message: "no"}); err != nil {
			t.Fatalf("answer permission: %v", err)
		}
		frames := cpTestFramesOfType(t, stdin, "permission_response")
		if len(frames) != 1 {
			t.Fatalf("permission responses = %v", frames)
		}
		if frames[0]["id"] != "perm_1" || frames[0]["approved"] != false || frames[0]["message"] != "no" {
			t.Fatalf("denial frame = %+v", frames[0])
		}
	})
}

// TestPiEncodePromptUnsupportedAttachment pins the refusal: pi takes no prompt
// attachment, and the error is a typed ErrUnsupported.
func TestPiEncodePromptUnsupportedAttachment(t *testing.T) {
	t.Parallel()
	proto := cpTestPiProto(PermissionPolicy{}, "")
	_, err := proto.EncodePrompt(Prompt{
		Text:        "look",
		Attachments: []Attachment{{MIME: "image/png", Data: []byte{0x89, 'P', 'N', 'G'}}},
	})
	if err == nil {
		t.Fatal("an attachment must be refused")
	}
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("error = %v, want ErrUnsupported", err)
	}
	if !strings.Contains(err.Error(), "pi") {
		t.Fatalf("error text = %q", err.Error())
	}
}

// TestPiInstructionsFirstPromptOnly pins the instructions fallback: the block
// is prepended to the first prompt and to no later prompt.
func TestPiInstructionsFirstPromptOnly(t *testing.T) {
	t.Parallel()
	proto := cpTestPiProto(PermissionPolicy{}, "Be terse.")
	first := cpTestPiPromptMessage(t, proto, "one")
	want := "<session-instructions>\nBe terse.\n</session-instructions>\n\none"
	if first != want {
		t.Fatalf("first prompt = %q want %q", first, want)
	}
	if second := cpTestPiPromptMessage(t, proto, "two"); second != "two" {
		t.Fatalf("second prompt = %q", second)
	}

	bare := cpTestPiProto(PermissionPolicy{}, "   ")
	if got := cpTestPiPromptMessage(t, bare, "plain"); got != "plain" {
		t.Fatalf("prompt with blank instructions = %q", got)
	}
}
