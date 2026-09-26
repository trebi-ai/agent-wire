package anthropic

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"

	"github.com/trebi-ai/agent-wire/native"
)

// thinkingMeta is the ProviderMeta shape of one reasoning part. It carries
// the thinking signature so a replayed block passes verification.
type thinkingMeta struct {
	Signature string `json:"signature"`
}

// params maps one native request onto the Messages API parameters.
func (p *provider) params(req native.Request) (sdk.MessageNewParams, error) {
	maxTokens := int64(req.MaxOutputTokens)
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}
	params := sdk.MessageNewParams{
		Model:     sdk.Model(p.name),
		MaxTokens: maxTokens,
		Messages:  make([]sdk.MessageParam, 0, len(req.Messages)),
	}
	if req.System != "" {
		params.System = []sdk.TextBlockParam{{Text: req.System}}
	}
	for i := range req.Messages {
		blocks, err := blocksOf(req.Messages[i])
		if err != nil {
			return params, err
		}
		role := sdk.MessageParamRoleUser
		if req.Messages[i].Role == native.RoleAssistant {
			role = sdk.MessageParamRoleAssistant
		}
		params.Messages = append(params.Messages, sdk.MessageParam{Role: role, Content: blocks})
	}
	for i := range req.Tools {
		tool, err := toolOf(req.Tools[i])
		if err != nil {
			return params, err
		}
		params.Tools = append(params.Tools, tool)
	}
	if p.web {
		params.Tools = append(params.Tools, sdk.ToolUnionParam{
			OfWebSearchTool20250305: &sdk.WebSearchTool20250305Param{
				MaxUses: param.NewOpt(webSearchMaxUses),
			},
		})
	}
	if len(params.Tools) > 0 {
		choice, err := choiceOf(req.ToolChoice)
		if err != nil {
			return params, err
		}
		params.ToolChoice = choice
	}
	if req.Effort != "" {
		budget, ok := effortBudgets[req.Effort]
		if !ok {
			return params, fmt.Errorf("anthropic: unknown effort %q", req.Effort)
		}
		if budget >= maxTokens {
			budget = maxTokens - 1
		}
		if budget < minThinkingBudget {
			return params, fmt.Errorf("anthropic: effort %q needs max_tokens above %d, have %d",
				req.Effort, minThinkingBudget, maxTokens)
		}
		params.Thinking = sdk.ThinkingConfigParamUnion{
			OfEnabled: &sdk.ThinkingConfigEnabledParam{BudgetTokens: budget},
		}
	}
	return params, nil
}

// blocksOf maps one native message onto content blocks.
func blocksOf(m native.Message) ([]sdk.ContentBlockParamUnion, error) {
	out := make([]sdk.ContentBlockParamUnion, 0, len(m.Parts))
	for i := range m.Parts {
		part := m.Parts[i]
		switch {
		case part.Text != nil:
			out = append(out, sdk.ContentBlockParamUnion{
				OfText: &sdk.TextBlockParam{Text: part.Text.Text},
			})
		case part.File != nil:
			image, document, err := fileOf(part.File)
			if err != nil {
				return nil, err
			}
			out = append(out, contentOf(image, document))
		case part.Reasoning != nil:
			out = append(out, sdk.ContentBlockParamUnion{
				OfThinking: &sdk.ThinkingBlockParam{
					Thinking:  part.Reasoning.Text,
					Signature: signatureOf(part.Reasoning.ProviderMeta),
				},
			})
		case part.ToolCall != nil:
			input := part.ToolCall.Input
			if len(input) == 0 {
				input = json.RawMessage("{}")
			}
			out = append(out, sdk.ContentBlockParamUnion{
				OfToolUse: &sdk.ToolUseBlockParam{ID: part.ToolCall.ID, Name: part.ToolCall.Name, Input: input},
			})
		case part.ToolResult != nil:
			block, err := toolResultOf(part.ToolResult)
			if err != nil {
				return nil, err
			}
			out = append(out, block)
		}
	}
	return out, nil
}

// signatureOf reads the thinking signature from one reasoning part.
func signatureOf(meta json.RawMessage) string {
	if len(meta) == 0 {
		return ""
	}
	var m thinkingMeta
	if err := json.Unmarshal(meta, &m); err != nil {
		return ""
	}
	return m.Signature
}

// fileOf maps one attachment onto an image or a document source. An image
// media type picks the image block; anything else picks the document block.
func fileOf(f *native.FilePart) (*sdk.ImageBlockParam, *sdk.DocumentBlockParam, error) {
	if f.URL != "" {
		if strings.HasPrefix(f.MediaType, "image/") {
			return &sdk.ImageBlockParam{
				Source: sdk.ImageBlockParamSourceUnion{OfURL: &sdk.URLImageSourceParam{URL: f.URL}},
			}, nil, nil
		}
		return nil, &sdk.DocumentBlockParam{
			Source: sdk.DocumentBlockParamSourceUnion{OfURL: &sdk.URLPDFSourceParam{URL: f.URL}},
		}, nil
	}
	if len(f.Data) > 0 {
		data := base64.StdEncoding.EncodeToString(f.Data)
		if strings.HasPrefix(f.MediaType, "image/") {
			return &sdk.ImageBlockParam{
				Source: sdk.ImageBlockParamSourceUnion{
					OfBase64: &sdk.Base64ImageSourceParam{Data: data, MediaType: sdk.Base64ImageSourceMediaType(f.MediaType)},
				},
			}, nil, nil
		}
		return nil, &sdk.DocumentBlockParam{
			Source: sdk.DocumentBlockParamSourceUnion{OfBase64: &sdk.Base64PDFSourceParam{Data: data}},
		}, nil
	}
	return nil, nil, errors.New("anthropic: file part has neither data nor url")
}

// contentOf wraps one image or document source into a content block.
func contentOf(image *sdk.ImageBlockParam, document *sdk.DocumentBlockParam) sdk.ContentBlockParamUnion {
	if image != nil {
		return sdk.ContentBlockParamUnion{OfImage: image}
	}
	return sdk.ContentBlockParamUnion{OfDocument: document}
}

// toolResultOf maps one tool answer onto a tool_result block.
func toolResultOf(r *native.ToolResult) (sdk.ContentBlockParamUnion, error) {
	out := sdk.ToolResultBlockParam{ToolUseID: r.CallID}
	if r.IsError {
		out.IsError = param.NewOpt(true)
	}
	for i := range r.Content {
		part := r.Content[i]
		switch {
		case part.Text != nil:
			out.Content = append(out.Content, sdk.ToolResultBlockParamContentUnion{
				OfText: &sdk.TextBlockParam{Text: part.Text.Text},
			})
		case part.File != nil:
			image, document, err := fileOf(part.File)
			if err != nil {
				return sdk.ContentBlockParamUnion{}, err
			}
			if image != nil {
				out.Content = append(out.Content, sdk.ToolResultBlockParamContentUnion{OfImage: image})
				continue
			}
			out.Content = append(out.Content, sdk.ToolResultBlockParamContentUnion{OfDocument: document})
		}
	}
	return sdk.ContentBlockParamUnion{OfToolResult: &out}, nil
}

// toolOf maps one tool spec onto a custom tool definition.
func toolOf(spec native.ToolSpec) (sdk.ToolUnionParam, error) {
	schema, err := schemaOf(spec.InputSchema)
	if err != nil {
		return sdk.ToolUnionParam{}, err
	}
	tool := sdk.ToolParam{Name: spec.Name, InputSchema: schema}
	if spec.Description != "" {
		tool.Description = param.NewOpt(spec.Description)
	}
	return sdk.ToolUnionParam{OfTool: &tool}, nil
}

// schemaOf copies one JSON schema object onto the tool input schema. The
// keys the API reads directly stay typed; the rest passes through untouched.
func schemaOf(raw json.RawMessage) (sdk.ToolInputSchemaParam, error) {
	var doc map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &doc); err != nil {
			return sdk.ToolInputSchemaParam{}, fmt.Errorf("anthropic: tool input schema: %w", err)
		}
	}
	var schema sdk.ToolInputSchemaParam
	if props, ok := doc["properties"]; ok {
		schema.Properties = props
	}
	if required, ok := doc["required"].([]any); ok {
		for _, v := range required {
			if s, ok := v.(string); ok {
				schema.Required = append(schema.Required, s)
			}
		}
	}
	extra := map[string]any{}
	for k, v := range doc {
		switch k {
		case "properties", "required", "type":
		default:
			extra[k] = v
		}
	}
	if len(extra) > 0 {
		schema.ExtraFields = extra
	}
	return schema, nil
}

// choiceOf maps the native tool choice onto the wire union.
func choiceOf(c native.ToolChoice) (sdk.ToolChoiceUnionParam, error) {
	switch c.Mode {
	case native.ToolAuto:
		return sdk.ToolChoiceUnionParam{OfAuto: &sdk.ToolChoiceAutoParam{}}, nil
	case native.ToolNone:
		return sdk.ToolChoiceUnionParam{OfNone: &sdk.ToolChoiceNoneParam{}}, nil
	case native.ToolRequired:
		return sdk.ToolChoiceUnionParam{OfAny: &sdk.ToolChoiceAnyParam{}}, nil
	case native.ToolNamed:
		if c.Name == "" {
			return sdk.ToolChoiceUnionParam{}, errors.New("anthropic: named tool choice has no name")
		}
		return sdk.ToolChoiceUnionParam{OfTool: &sdk.ToolChoiceToolParam{Name: c.Name}}, nil
	default:
		return sdk.ToolChoiceUnionParam{}, fmt.Errorf("anthropic: unknown tool choice mode %d", c.Mode)
	}
}
