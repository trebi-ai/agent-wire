// Package openai adapts the OpenAI Chat Completions wire to native.Model.
// It speaks the streaming API over openai-go and folds the chunks into
// native.StreamParts. A custom base URL turns it into a gateway client for
// Ollama, OpenRouter, vLLM, Groq, and LM Studio.
package openai

import (
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/trebi-ai/agent-wire"
	"github.com/trebi-ai/agent-wire/native"
)

// Format is the credentials format this provider serves.
const Format = "openai"

// options carries the constructor overrides.
type options struct {
	httpClient *http.Client
	baseURL    string
}

// Option changes one constructor setting.
type Option func(*options)

// WithHTTPClient sets the HTTP client for every provider call.
func WithHTTPClient(hc *http.Client) Option {
	return func(o *options) { o.httpClient = hc }
}

// WithBaseURL overrides the credentials base URL.
func WithBaseURL(u string) Option {
	return func(o *options) { o.baseURL = u }
}

// model is the native.Model over the Chat Completions API.
type model struct {
	client openai.Client
	name   string
}

// Name is the model identifier the driver reports on init.
func (m *model) Name() string { return m.name }

// New builds a native.Model for the OpenAI wire. The credentials format is
// "openai" or empty. The base URL comes from the credentials, or from
// WithBaseURL when set. Credentials headers ride on every request.
func New(c agentwire.Credentials, modelName string, opts ...Option) (native.Model, error) {
	if c.Format != "" && c.Format != Format {
		return nil, fmt.Errorf("openai: credentials format %q is not %q", c.Format, Format)
	}
	if c.APIKey == "" {
		return nil, errors.New("openai: credentials need an API key")
	}
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	copts := []option.RequestOption{option.WithAPIKey(c.APIKey)}
	base := c.BaseURL
	if o.baseURL != "" {
		base = o.baseURL
	}
	if base != "" {
		copts = append(copts, option.WithBaseURL(base))
	}
	if o.httpClient != nil {
		copts = append(copts, option.WithHTTPClient(o.httpClient))
	}
	// Sorted keys keep the request setup stable across runs.
	for _, k := range slices.Sorted(maps.Keys(c.Headers)) {
		copts = append(copts, option.WithHeader(k, c.Headers[k]))
	}
	return &model{client: openai.NewClient(copts...), name: modelName}, nil
}
