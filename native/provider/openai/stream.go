package openai

import (
	"context"
	"encoding/json"
	"iter"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/shared"
	"github.com/trebi-ai/agent-wire/native"
)

// Stream runs one model call and folds the chunk stream into native
// StreamParts. The sequence yields text deltas, tool input deltas, one done
// part per tool call, and exactly one finish part. A transport error still
// ends with one finish part, then the error.
func (m *model) Stream(ctx context.Context, req native.Request) iter.Seq2[native.StreamPart, error] {
	return func(yield func(native.StreamPart, error) bool) {
		params := m.params(req)
		stream := m.client.Chat.Completions.NewStreaming(ctx, params)
		defer stream.Close() //nolint:errcheck // nothing left to do with a close error

		var (
			calls      callTable
			rawReason  string
			usage      native.Usage
			streamFail error
		)
		for stream.Next() {
			chunk := stream.Current()
			for _, choice := range chunk.Choices {
				if d := choice.Delta.Content; d != "" {
					if !yield(textDelta(d), nil) {
						return
					}
				}
				for _, shard := range choice.Delta.ToolCalls {
					call := calls.shard(shard.Index)
					if shard.ID != "" {
						call.id = shard.ID
					}
					if shard.Function.Name != "" {
						call.name = shard.Function.Name
					}
					if shard.Function.Arguments != "" {
						call.args.WriteString(shard.Function.Arguments)
						if !yield(toolDelta(call.id, shard.Function.Arguments), nil) {
							return
						}
					}
				}
				if choice.FinishReason != "" {
					rawReason = choice.FinishReason
				}
			}
			// The usage chunk carries no choice. Field validity marks a
			// present usage object, because a zero value means absent.
			if chunk.Usage.JSON.PromptTokens.Valid() {
				usage = native.Usage{
					Input:     chunk.Usage.PromptTokens,
					Output:    chunk.Usage.CompletionTokens,
					CacheRead: chunk.Usage.PromptTokensDetails.CachedTokens,
					Reasoning: chunk.Usage.CompletionTokensDetails.ReasoningTokens,
				}
			}
		}
		streamFail = stream.Err()

		finish := native.Finish{Raw: rawReason, Usage: usage}
		switch {
		case streamFail != nil:
			finish.Reason = native.FinishError
		default:
			finish.Reason = mapFinish(rawReason)
		}
		for _, call := range calls.ordered() {
			if !yield(call.done(), nil) {
				return
			}
		}
		if !yield(native.StreamPart{Kind: native.StreamFinish, Finish: &finish}, nil) {
			return
		}
		if streamFail != nil {
			yield(native.StreamPart{}, streamFail)
		}
	}
}

// params builds the request body for one call.
func (m *model) params(req native.Request) openai.ChatCompletionNewParams {
	p := openai.ChatCompletionNewParams{
		Model:    shared.ChatModel(m.name),
		Messages: messages(req),
		Tools:    tools(req.Tools),
	}
	p.ToolChoice = toolChoice(req.ToolChoice)
	if req.MaxOutputTokens > 0 {
		p.MaxTokens = param.NewOpt(int64(req.MaxOutputTokens))
	}
	if effort, ok := reasoningEffort(req.Effort); ok {
		p.ReasoningEffort = effort
	}
	return p
}

// messages maps the neutral conversation onto chat messages. The system
// prompt leads. Unsupported parts are left out.
func messages(req native.Request) []openai.ChatCompletionMessageParamUnion {
	var out []openai.ChatCompletionMessageParamUnion
	if req.System != "" {
		out = append(out, openai.SystemMessage(req.System))
	}
	for _, msg := range req.Messages {
		switch msg.Role {
		case native.RoleUser:
			if text := joinText(msg.Parts); text != "" {
				out = append(out, openai.UserMessage(text))
			}
		case native.RoleAssistant:
			am := openai.ChatCompletionAssistantMessageParam{}
			if text := joinText(msg.Parts); text != "" {
				am.Content = openai.ChatCompletionAssistantMessageParamContentUnion{
					OfString: param.NewOpt(text),
				}
			}
			for _, part := range msg.Parts {
				if part.ToolCall == nil {
					continue
				}
				am.ToolCalls = append(am.ToolCalls, functionCall(*part.ToolCall))
			}
			out = append(out, openai.ChatCompletionMessageParamUnion{OfAssistant: &am})
		case native.RoleTool:
			for _, part := range msg.Parts {
				if part.ToolResult == nil {
					continue
				}
				out = append(out, openai.ToolMessage(joinText(part.ToolResult.Content), part.ToolResult.CallID))
			}
		}
	}
	return out
}

// functionCall maps one neutral tool call onto a function tool call.
func functionCall(tc native.ToolCall) openai.ChatCompletionMessageToolCallUnionParam {
	return openai.ChatCompletionMessageToolCallUnionParam{
		OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
			ID: tc.ID,
			Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
				Name:      tc.Name,
				Arguments: string(tc.Input),
			},
		},
	}
}

// joinText concatenates the text of all text parts with newline separators.
func joinText(parts []native.Part) string {
	var b strings.Builder
	for _, part := range parts {
		if part.Text == nil {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(part.Text.Text)
	}
	return b.String()
}

// tools maps the tool surface onto function tools. The input schema passes
// through as the parameters object.
func tools(specs []native.ToolSpec) []openai.ChatCompletionToolUnionParam {
	if len(specs) == 0 {
		return nil
	}
	out := make([]openai.ChatCompletionToolUnionParam, 0, len(specs))
	for _, spec := range specs {
		def := shared.FunctionDefinitionParam{
			Name:        spec.Name,
			Description: param.NewOpt(spec.Description),
		}
		if len(spec.InputSchema) > 0 {
			var schema shared.FunctionParameters
			if err := json.Unmarshal(spec.InputSchema, &schema); err == nil {
				def.Parameters = schema
			}
		}
		out = append(out, openai.ChatCompletionToolUnionParam{
			OfFunction: &openai.ChatCompletionFunctionToolParam{Function: def},
		})
	}
	return out
}

// toolChoice maps the steering mode onto the tool_choice value. The zero
// mode maps to "auto".
func toolChoice(tc native.ToolChoice) openai.ChatCompletionToolChoiceOptionUnionParam {
	switch tc.Mode {
	case native.ToolNone:
		return openai.ChatCompletionToolChoiceOptionUnionParam{OfAuto: param.NewOpt("none")}
	case native.ToolRequired:
		return openai.ChatCompletionToolChoiceOptionUnionParam{OfAuto: param.NewOpt("required")}
	case native.ToolNamed:
		return openai.ChatCompletionToolChoiceOptionUnionParam{
			OfFunctionToolChoice: &openai.ChatCompletionNamedToolChoiceParam{
				Function: openai.ChatCompletionNamedToolChoiceFunctionParam{Name: tc.Name},
			},
		}
	default:
		return openai.ChatCompletionToolChoiceOptionUnionParam{OfAuto: param.NewOpt("auto")}
	}
}

// reasoningEffort maps the neutral effort onto reasoning_effort. Unknown or
// empty efforts stay unset, so the provider default applies.
func reasoningEffort(effort string) (shared.ReasoningEffort, bool) {
	switch effort {
	case "low", "medium", "high":
		return shared.ReasoningEffort(effort), true
	case "max":
		// The chat wire has no "max", so the top chat value serves.
		return shared.ReasoningEffortHigh, true
	default:
		return "", false
	}
}

// mapFinish unifies the vendor finish reason. An unknown value falls back
// to FinishOther and keeps the raw string.
func mapFinish(raw string) native.FinishReason {
	switch raw {
	case "stop":
		return native.FinishStop
	case "length":
		return native.FinishLength
	case "tool_calls", "function_call":
		return native.FinishToolCalls
	case "content_filter":
		return native.FinishContentFilter
	default:
		return native.FinishOther
	}
}

// callState accumulates one tool call from its index shards.
type callState struct {
	id   string
	name string
	args strings.Builder
}

// done builds the finished call. Empty arguments stay unset.
func (c *callState) done() native.StreamPart {
	input := json.RawMessage(c.args.String())
	if len(input) == 0 {
		input = nil
	}
	return native.StreamPart{
		Kind: native.StreamToolCallDone,
		ID:   c.id,
		Call: &native.ToolCall{ID: c.id, Name: c.name, Input: input},
	}
}

// callTable tracks index-sharded tool calls in arrival order.
type callTable struct {
	byIndex map[int64]*callState
	order   []*callState
}

// shard returns the accumulator for one index and creates it on first use.
func (t *callTable) shard(index int64) *callState {
	if t.byIndex == nil {
		t.byIndex = make(map[int64]*callState)
	}
	call, ok := t.byIndex[index]
	if !ok {
		call = &callState{}
		t.byIndex[index] = call
		t.order = append(t.order, call)
	}
	return call
}

// ordered lists the accumulated calls in arrival order.
func (t *callTable) ordered() []*callState { return t.order }

// textDelta builds one text delta part.
func textDelta(s string) native.StreamPart {
	return native.StreamPart{Kind: native.StreamTextDelta, Delta: s}
}

// toolDelta builds one tool input delta part.
func toolDelta(id, fragment string) native.StreamPart {
	return native.StreamPart{Kind: native.StreamToolInputDelta, ID: id, Delta: fragment}
}
