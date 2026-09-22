//go:build live

package live

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/trebi-ai/agent-wire"
)

// TestLiveMCPHelperServer is the stdio MCP server that the live MCP test
// spawns. It is not a live test: the test binary runs again with
// AGENTWIRE_MCP_HELPER=1 in the environment, and this test then serves MCP on
// stdin and stdout until stdin closes.
func TestLiveMCPHelperServer(t *testing.T) {
	if os.Getenv(liveTestMCPHelperEnv) != "1" {
		t.Skip("helper server: it runs only as a re-executed test binary")
	}
	liveTestServeMCP(os.Stdin, os.Stdout)
	// Exit directly: the testing package must not print its summary on the
	// JSON-RPC stream.
	os.Exit(0)
}

// TestLiveMCPInjection injects a stdio MCP server built from the test binary
// and checks that the agent can call its echo tool.
func TestLiveMCPInjection(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("live tier: the MCP helper is a POSIX test binary path")
	}
	for _, h := range liveTestHarnesses {
		t.Run(string(h), func(t *testing.T) {
			if !liveEnabled(t, h) {
				return
			}
			rt := liveRuntime(t)
			d := liveTestReady(t, rt, h)
			if d.Features.MCPInject == agentwire.MCPNone {
				t.Skipf("live tier: %s takes no injected MCP server", h)
			}
			if h == agentwire.Pi {
				// pi reads MCP only from an isolated home, and a fresh home
				// carries no login. An operator must seed that home by hand.
				t.Skipf("live tier: %s MCP injection needs a seeded isolated home", h)
			}

			req := liveTestRequest(h, liveWorkDir(t))
			req.MCPServers = []agentwire.MCPServer{{
				Name:    "agentwire-echo",
				Command: os.Args[0],
				Args:    []string{"-test.run", "TestLiveMCPHelperServer"},
				Env:     map[string]string{liveTestMCPHelperEnv: "1"},
			}}
			s := liveTestStart(t, rt, req)
			o := liveTestTurn(t, h, s, "Use the echo tool with the text HELLO and report the result.", liveTestTurnTimeout)
			liveTestAssertTurn(t, h, o)
			if !liveTestMCPCalled(o) {
				names := make([]string, 0, len(o.tools))
				for _, tool := range o.tools {
					names = append(names, tool.Name)
				}
				t.Errorf("live tier: %s: the agent did not call the injected echo tool; tools=%v text=%q",
					h, names, o.text.String())
			}
			liveTestCloseSession(t, h, s, o)
		})
	}
}

// liveTestMCPCalled reports whether a turn mentions the injected server or its
// echo result.
func liveTestMCPCalled(o *liveTestObs) bool {
	for _, tool := range o.tools {
		if strings.Contains(tool.Name, "agentwire-echo") {
			return true
		}
	}
	return strings.Contains(strings.ToUpper(o.text.String()), "HELLO")
}

// liveTestMCPMessage is the subset of JSON-RPC the helper reads.
type liveTestMCPMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
}

// liveTestServeMCP speaks the MCP stdio handshake: newline-delimited JSON-RPC.
// It answers initialize, tools/list and tools/call, ignores notifications and
// responses, and returns when the reader reaches EOF.
func liveTestServeMCP(in io.Reader, out io.Writer) {
	reader := bufio.NewReaderSize(in, 1<<16)
	writer := bufio.NewWriter(out)
	for {
		line, err := reader.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			liveTestMCPDispatch(writer, line)
			_ = writer.Flush()
		}
		if err != nil {
			return
		}
	}
}

// liveTestMCPDispatch answers one request. A message without an id is a
// notification or a response, so it needs no answer.
func liveTestMCPDispatch(w io.Writer, line []byte) {
	var msg liveTestMCPMessage
	if err := json.Unmarshal(bytes.TrimSpace(line), &msg); err != nil {
		return
	}
	if msg.Method == "" || len(msg.ID) == 0 {
		return
	}
	switch msg.Method {
	case "initialize":
		version := "2024-11-05"
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if json.Unmarshal(msg.Params, &p) == nil && p.ProtocolVersion != "" {
			version = p.ProtocolVersion
		}
		liveTestMCPReply(w, msg.ID, map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "agentwire-echo", "version": "1.0.0"},
		})
	case "tools/list":
		liveTestMCPReply(w, msg.ID, map[string]any{"tools": []map[string]any{{
			"name":        "echo",
			"description": "Return the text given in the text argument, unchanged.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"text": map[string]any{"type": "string", "description": "The text to return."},
				},
				"required": []string{"text"},
			},
		}}})
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		liveTestMCPReply(w, msg.ID, map[string]any{
			"content": []map[string]any{{"type": "text", "text": liveTestMCPEcho(p.Name, p.Arguments)}},
			"isError": p.Name != "echo",
		})
	case "ping":
		liveTestMCPReply(w, msg.ID, map[string]any{})
	default:
		// A probing client gets an empty result, so the transport stays up.
		liveTestMCPReply(w, msg.ID, map[string]any{})
	}
}

// liveTestMCPEcho returns the text argument of the echo tool.
func liveTestMCPEcho(name string, args json.RawMessage) string {
	if name != "echo" {
		return "unknown tool: " + name
	}
	var a struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(args, &a); err == nil && a.Text != "" {
		return a.Text
	}
	if len(args) > 0 {
		return string(args)
	}
	return ""
}

// liveTestMCPReply writes one JSON-RPC result line.
func liveTestMCPReply(w io.Writer, id json.RawMessage, result any) {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	if err != nil {
		return
	}
	_, _ = w.Write(append(body, '\n'))
}
