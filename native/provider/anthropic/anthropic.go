// Package anthropic adapts the native harness to the Anthropic Messages
// wire. New builds a native.Model that posts one streaming request per call
// and folds the server-sent events into native stream parts.
package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/trebi-ai/agent-wire"
	"github.com/trebi-ai/agent-wire/native"
)

const (
	// defaultMaxTokens bounds one response when the request sets none.
	defaultMaxTokens = 8192
	// minThinkingBudget is the API floor for one thinking budget.
	minThinkingBudget = 1024
	// webSearchMaxUses bounds server web_search uses for one response.
	webSearchMaxUses int64 = 3
)

// effortBudgets maps the request effort onto a thinking token budget.
var effortBudgets = map[string]int64{
	"low":    1024,
	"medium": 4096,
	"high":   16384,
	"max":    32768,
}

type options struct {
	webSearch bool
	http      *http.Client
	baseURL   string
}

// Option configures one provider instance.
type Option func(*options)

// WithWebSearch adds the server web_search tool to every call.
func WithWebSearch(enable bool) Option {
	return func(o *options) { o.webSearch = enable }
}

// WithHTTPClient sends every request through a custom HTTP client.
func WithHTTPClient(c *http.Client) Option {
	return func(o *options) { o.http = c }
}

// WithBaseURL overrides the endpoint in the credentials.
func WithBaseURL(url string) Option {
	return func(o *options) { o.baseURL = url }
}

// provider is one native.Model over the Anthropic Messages wire.
type provider struct {
	client sdk.Client
	name   string
	web    bool
}

// New validates the credentials and builds the model. The format must be
// empty or "anthropic", and the key must be present.
func New(c agentwire.Credentials, model string, opts ...Option) (native.Model, error) {
	if c.Format != "" && c.Format != "anthropic" {
		return nil, fmt.Errorf("anthropic: credentials format %q is not %q", c.Format, "anthropic")
	}
	if c.APIKey == "" {
		return nil, errors.New("anthropic: credentials have no API key")
	}
	var o options
	for _, fn := range opts {
		fn(&o)
	}
	base := o.baseURL
	if base == "" {
		base = c.BaseURL
	}
	clientOpts := []option.RequestOption{option.WithAPIKey(c.APIKey)}
	if base != "" {
		clientOpts = append(clientOpts, option.WithBaseURL(base))
	}
	if o.http != nil {
		clientOpts = append(clientOpts, option.WithHTTPClient(o.http))
	}
	for k, v := range c.Headers {
		clientOpts = append(clientOpts, option.WithHeader(k, v))
	}
	return &provider{client: sdk.NewClient(clientOpts...), name: model, web: o.webSearch}, nil
}

// Name reports the model identifier.
func (p *provider) Name() string { return p.name }

// Stream runs one Messages call. The sequence folds the event stream into
// native stream parts and ends with exactly one StreamFinish. A transport
// error yields one error and ends the sequence.
func (p *provider) Stream(ctx context.Context, req native.Request) native.Stream {
	return func(yield func(native.StreamPart, error) bool) {
		params, err := p.params(req)
		if err != nil {
			yield(native.StreamPart{}, err)
			return
		}
		stream := p.client.Messages.NewStreaming(ctx, params)
		defer stream.Close()

		// Blocks carries the open tool_use blocks by index.
		blocks := map[int64]*toolBlock{}
		var usage native.Usage
		var stop sdk.StopReason

		for stream.Next() {
			ev := stream.Current()
			switch ev.Type {
			case "message_start":
				u := ev.Message.Usage
				usage = native.Usage{
					Input:      u.InputTokens,
					Output:     u.OutputTokens,
					CacheRead:  u.CacheReadInputTokens,
					CacheWrite: u.CacheCreationInputTokens,
				}
			case "content_block_start":
				b := ev.ContentBlock
				if b.Type != "tool_use" {
					continue
				}
				blocks[ev.Index] = &toolBlock{id: b.ID, name: b.Name}
				if !yield(native.StreamPart{Kind: native.StreamToolInputStart, ID: b.ID}, nil) {
					return
				}
			case "content_block_delta":
				d := ev.Delta
				switch d.Type {
				case "text_delta":
					if !yield(native.StreamPart{Kind: native.StreamTextDelta, Delta: d.Text}, nil) {
						return
					}
				case "thinking_delta":
					if !yield(native.StreamPart{Kind: native.StreamReasoningDelta, Delta: d.Thinking}, nil) {
						return
					}
				case "input_json_delta":
					b := blocks[ev.Index]
					if b == nil {
						continue
					}
					b.input.WriteString(d.PartialJSON)
					if !yield(native.StreamPart{Kind: native.StreamToolInputDelta, ID: b.id, Delta: d.PartialJSON}, nil) {
						return
					}
				}
			case "content_block_stop":
				b := blocks[ev.Index]
				if b == nil {
					continue
				}
				delete(blocks, ev.Index)
				call := native.ToolCall{ID: b.id, Name: b.name, Input: json.RawMessage(b.input.String())}
				if len(call.Input) == 0 {
					call.Input = json.RawMessage("{}")
				}
				if !yield(native.StreamPart{Kind: native.StreamToolCallDone, Call: &call}, nil) {
					return
				}
			case "message_delta":
				// The delta usage is cumulative, so a non-zero value
				// supersedes the counts from message_start.
				stop = ev.Delta.StopReason
				if u := ev.Usage; u.OutputTokens != 0 {
					usage.Output = u.OutputTokens
				}
				if u := ev.Usage; u.InputTokens != 0 {
					usage.Input = u.InputTokens
				}
				if u := ev.Usage; u.CacheReadInputTokens != 0 {
					usage.CacheRead = u.CacheReadInputTokens
				}
				if u := ev.Usage; u.CacheCreationInputTokens != 0 {
					usage.CacheWrite = u.CacheCreationInputTokens
				}
			}
		}
		if err := stream.Err(); err != nil {
			yield(native.StreamPart{}, err)
			return
		}
		yield(native.StreamPart{Kind: native.StreamFinish, Finish: &native.Finish{
			Reason: finishReason(stop),
			Raw:    string(stop),
			Usage:  usage,
		}}, nil)
	}
}

// toolBlock tracks one open tool_use block until its stop event.
type toolBlock struct {
	id    string
	name  string
	input strings.Builder
}

// finishReason maps the vendor stop reason onto the native union.
func finishReason(stop sdk.StopReason) native.FinishReason {
	switch stop {
	case sdk.StopReasonEndTurn:
		return native.FinishStop
	case sdk.StopReasonMaxTokens:
		return native.FinishLength
	case sdk.StopReasonToolUse:
		return native.FinishToolCalls
	case sdk.StopReasonRefusal:
		return native.FinishRefusal
	default:
		// stop_sequence, pause_turn, and model_context_window_exceeded
		// have no closer native reason.
		return native.FinishOther
	}
}
