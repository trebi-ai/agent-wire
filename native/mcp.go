package native

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/trebi-ai/agent-wire"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCPToolSet connects one agentwire.MCPServer and exposes its tools. A
// server with Command runs over stdio; a server with URL speaks streamable
// HTTP (plan 2026-09-26 I.5).
func MCPToolSet(ctx context.Context, srv agentwire.MCPServer, client *mcp.Implementation) (ToolSet, error) {
	if client == nil {
		client = &mcp.Implementation{Name: "agentwire", Version: "native"}
	}
	c := mcp.NewClient(client, nil)
	var transport mcp.Transport
	switch {
	case srv.Command != "":
		cmd := exec.Command(srv.Command, srv.Args...)
		if len(srv.Env) > 0 {
			cmd.Env = osEnviron(srv.Env)
		}
		transport = &mcp.CommandTransport{Command: cmd}
	case srv.URL != "":
		transport = &mcp.StreamableClientTransport{Endpoint: srv.URL}
	default:
		return nil, fmt.Errorf("native: mcp server %q has no command and no url", srv.Name)
	}
	cs, err := c.Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("native: mcp server %s connect: %w", srv.Name, err)
	}
	return &mcpToolSet{name: srv.Name, session: cs}, nil
}

// MCPSessionToolSet wraps a connected client session, for an in-process
// server over mcp.NewInMemoryTransports.
func MCPSessionToolSet(name string, cs *mcp.ClientSession) ToolSet {
	return &mcpToolSet{name: name, session: cs}
}

type mcpToolSet struct {
	name    string
	session *mcp.ClientSession
}

func (t *mcpToolSet) Name() string { return t.name }

// Tools lists the server tools with the mcp__<server>__<tool> names the
// Claude harness also uses.
func (t *mcpToolSet) Tools(ctx context.Context) ([]Tool, error) {
	res, err := t.session.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		return nil, err
	}
	out := make([]Tool, 0, len(res.Tools))
	for _, tl := range res.Tools {
		out = append(out, &mcpTool{set: t.name, tool: tl, session: t.session})
	}
	return out, nil
}

func (t *mcpToolSet) Close() error { return t.session.Close() }

// mcpTool is one remote tool as a native Tool.
type mcpTool struct {
	set     string
	tool    *mcp.Tool
	session *mcp.ClientSession
}

func (t *mcpTool) Spec() ToolSpec {
	spec := ToolSpec{
		Name:        mcpToolName(t.set, t.tool.Name),
		Title:       t.tool.Title,
		Description: t.tool.Description,
	}
	if t.tool.InputSchema != nil {
		if body, err := json.Marshal(t.tool.InputSchema); err == nil {
			spec.InputSchema = body
		}
	}
	if a := t.tool.Annotations; a != nil {
		spec.Annotations = Annotations{
			ReadOnly:    a.ReadOnlyHint,
			Destructive: a.DestructiveHint != nil && *a.DestructiveHint,
			Idempotent:  a.IdempotentHint,
			OpenWorld:   a.OpenWorldHint != nil && *a.OpenWorldHint,
		}
	}
	if spec.Annotations.Kind == "" {
		spec.Annotations.Kind = agentwire.ToolMCP
	}
	return spec
}

func (t *mcpTool) Call(ctx context.Context, input json.RawMessage) (ToolResult, error) {
	var args any
	if len(input) > 0 {
		if err := json.Unmarshal(input, &args); err != nil {
			return errResult(t.Spec().Name, "invalid arguments: "+err.Error()), nil
		}
	}
	res, err := t.session.CallTool(ctx, &mcp.CallToolParams{Name: t.tool.Name, Arguments: args})
	if err != nil {
		return ToolResult{}, err
	}
	out := ToolResult{Name: t.Spec().Name, IsError: res.IsError}
	for _, c := range res.Content {
		switch cv := c.(type) {
		case *mcp.TextContent:
			out.Content = append(out.Content, Part{Text: &TextPart{Text: cv.Text}})
		default:
			body, _ := json.Marshal(c)
			out.Content = append(out.Content, Part{Text: &TextPart{Text: string(body)}})
		}
	}
	return out, nil
}

// mcpToolName renders the namespaced tool name.
func mcpToolName(server, tool string) string {
	return strings.Join([]string{"mcp", server, tool}, "__")
}

// osEnviron merges os.Environ with extra keys; extras win.
func osEnviron(extra map[string]string) []string {
	env := os.Environ()
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}
