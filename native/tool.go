package native

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/trebi-ai/agent-wire"
)

// Annotations describe one tool for the permission policy and the UI, in
// the shape of the MCP tool annotations.
type Annotations struct {
	ReadOnly   bool
	Destructive bool
	Idempotent bool
	OpenWorld  bool
	// Kind is the coarse class the permission policy and the UI read.
	Kind agentwire.ToolKind
}

// ToolSpec is the model-visible description of one tool.
type ToolSpec struct {
	Name        string
	Title       string
	Description string
	// InputSchema is a JSON Schema object.
	InputSchema json.RawMessage
	Annotations Annotations
}

// Tool is one callable tool. Call returns an error only for an
// infrastructure failure the loop must surface; a failure the model should
// see is a ToolResult with IsError.
type Tool interface {
	Spec() ToolSpec
	Call(ctx context.Context, input json.RawMessage) (ToolResult, error)
}

// NewFunc builds a Tool from a typed function. The input schema is inferred
// from In via reflection: exported fields with a json tag, required every
// field that is not a pointer.
func NewFunc[In, Out any](spec ToolSpec, fn func(context.Context, In) (Out, error)) Tool {
	return &funcTool[In, Out]{spec: spec, fn: fn}
}

type funcTool[In, Out any] struct {
	spec ToolSpec
	fn   func(context.Context, In) (Out, error)
}

func (t *funcTool[In, Out]) Spec() ToolSpec { return t.spec }

func (t *funcTool[In, Out]) Call(ctx context.Context, input json.RawMessage) (ToolResult, error) {
	var in In
	if len(input) > 0 {
		if err := json.Unmarshal(input, &in); err != nil {
			return errResult(t.spec.Name, fmt.Sprintf("invalid input: %v", err)), nil
		}
	}
	out, err := t.fn(ctx, in)
	if err != nil {
		return errResult(t.spec.Name, err.Error()), nil
	}
	body, merr := json.Marshal(out)
	if merr != nil {
		return ToolResult{}, merr
	}
	return ToolResult{CallID: "", Name: t.spec.Name, Content: []Part{{Text: &TextPart{Text: string(body)}}}}, nil
}

func errResult(name, msg string) ToolResult {
	return ToolResult{Name: name, IsError: true, Content: []Part{{Text: &TextPart{Text: msg}}}}
}

// schemaOf infers a JSON Schema object from a struct type.
func schemaOf(t reflect.Type) json.RawMessage {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	fields := map[string]any{}
	var required []string
	if t.Kind() == reflect.Struct {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			name := f.Tag.Get("json")
			if name != "" {
				name, _, _ = strings.Cut(name, ",")
			}
			if name == "" {
				name = f.Name
			}
			ft := f.Type
			for ft.Kind() == reflect.Ptr {
				ft = ft.Elem()
			}
			desc := map[string]any{"type": goTypeToSchema(ft)}
			fields[name] = desc
			if f.Type.Kind() != reflect.Ptr {
				required = append(required, name)
			}
		}
	}
	schema := map[string]any{"type": "object", "properties": fields}
	if len(required) > 0 {
		schema["required"] = required
	}
	body, _ := json.Marshal(schema)
	return body
}


func goTypeToSchema(t reflect.Type) string {
	switch t.Kind() {
	case reflect.Bool:
		return "boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return "number"
	case reflect.String:
		return "string"
	case reflect.Slice, reflect.Array:
		return "array"
	case reflect.Map:
		return "object"
	}
	return "string"
}

// ToolSet is a named group of tools that shares one lifecycle. MCP servers,
// the built-in file tools, and a consumer's own tools are all ToolSets.
type ToolSet interface {
	Name() string
	Tools(ctx context.Context) ([]Tool, error)
	Close() error
}

// StaticTools wraps fixed tools as a ToolSet.
func StaticTools(name string, tools ...Tool) ToolSet {
	return staticTools{name: name, tools: tools}
}

type staticTools struct {
	name  string
	tools []Tool
}

func (s staticTools) Name() string { return s.name }

func (s staticTools) Tools(context.Context) ([]Tool, error) { return s.tools, nil }

func (staticTools) Close() error { return nil }
