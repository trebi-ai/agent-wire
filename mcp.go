package agentwire

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// MCPServer is one Model Context Protocol server injected into a session.
// Command selects a stdio server; URL selects an HTTP server.
type MCPServer struct {
	Name    string
	Command string
	Args    []string
	Env     map[string]string
	URL     string
	Headers map[string]string
}

// MCPCapability reports how a harness accepts per-session MCP servers.
type MCPCapability string

const (
	// MCPNone means the harness cannot take an injected server.
	MCPNone MCPCapability = "none"
	// MCPFlags means the library renders the servers into the launch.
	MCPFlags MCPCapability = "flags"
	// MCPSession means the servers travel in the session-open request.
	MCPSession MCPCapability = "session"
)

// applyMCPServers renders the injected servers for this harness. The user's
// own servers always keep loading: injection is additive unless the caller
// asks for isolation through Raw["strict_mcp"].
func (rt *Runtime) applyMCPServers(req *StartRequest, l *launch) error {
	if len(req.MCPServers) == 0 {
		return nil
	}
	switch req.Harness {
	case Claude:
		dir, err := rt.sessionDir(req, "mcp")
		if err != nil {
			return err
		}
		path := filepath.Join(dir, "mcp.json")
		if err := writeJSONFile(path, map[string]any{"mcpServers": claudeMCPServers(req.MCPServers)}); err != nil {
			return err
		}
		l.tmp = append(l.tmp, dir)
		l.args = append(l.args, "--mcp-config", path)
		if strict, ok := req.RawBool("strict_mcp"); ok && strict {
			l.args = append(l.args, "--strict-mcp-config")
		}
	case Codex:
		l.args = append(l.args, codexMCPArgs(req.MCPServers)...)
	case OpenCode:
		content, err := openCodeConfigContent(req.MCPServers)
		if err != nil {
			return err
		}
		// The config content is part of the server fingerprint, so servers
		// injected here never land on a server started for another set.
		l.env["OPENCODE_CONFIG_CONTENT"] = content
	case Pi:
		if err := requireHome(Pi, req, "MCP injection"); err != nil {
			return err
		}
		if err := writeJSONFile(filepath.Join(req.Home, "mcp.json"),
			map[string]any{"mcpServers": claudeMCPServers(req.MCPServers)}); err != nil {
			return err
		}
	case Copilot, Cursor, Gemini:
		// The ACP driver puts the servers in session/new, session/load or
		// session/resume.
	}
	return nil
}

// claudeMCPServers renders the Claude/pi JSON shape.
func claudeMCPServers(servers []MCPServer) map[string]any {
	out := make(map[string]any, len(servers))
	for _, s := range servers {
		if s.Name == "" {
			continue
		}
		switch {
		case s.URL != "":
			entry := map[string]any{"type": "http", "url": s.URL}
			if len(s.Headers) > 0 {
				entry["headers"] = s.Headers
			}
			out[s.Name] = entry
		case s.Command != "":
			entry := map[string]any{"command": s.Command}
			if len(s.Args) > 0 {
				entry["args"] = s.Args
			}
			if len(s.Env) > 0 {
				entry["env"] = s.Env
			}
			out[s.Name] = entry
		}
	}
	return out
}

// codexMCPArgs renders `-c mcp_servers.<name>...` overrides.
func codexMCPArgs(servers []MCPServer) []string {
	var args []string
	for _, s := range servers {
		if s.Name == "" {
			continue
		}
		prefix := "mcp_servers." + s.Name + "."
		switch {
		case s.URL != "":
			args = append(args, "-c", prefix+`url=`+tomlString(s.URL))
		case s.Command != "":
			args = append(args, "-c", prefix+`command=`+tomlString(s.Command))
			if len(s.Args) > 0 {
				args = append(args, "-c", prefix+"args="+tomlStringList(s.Args))
			}
			if len(s.Env) > 0 {
				args = append(args, "-c", prefix+"env="+tomlStringTable(s.Env))
			}
		}
	}
	return args
}

// openCodeConfigContent renders the OpenCode config fragment that carries the
// injected servers.
func openCodeConfigContent(servers []MCPServer) (string, error) {
	mcp := map[string]any{}
	for _, s := range servers {
		if s.Name == "" {
			continue
		}
		switch {
		case s.URL != "":
			entry := map[string]any{"type": "remote", "url": s.URL}
			if len(s.Headers) > 0 {
				entry["headers"] = s.Headers
			}
			mcp[s.Name] = entry
		case s.Command != "":
			command := append([]string{s.Command}, s.Args...)
			entry := map[string]any{"type": "local", "command": command}
			if len(s.Env) > 0 {
				entry["environment"] = s.Env
			}
			mcp[s.Name] = entry
		}
	}
	b, err := json.Marshal(map[string]any{"mcp": mcp})
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// acpMCPServers renders the ACP session mcpServers parameter. HTTP entries are
// only sent when the agent advertised mcpCapabilities.http.
func acpMCPServers(servers []MCPServer, httpOK bool) []map[string]any {
	out := make([]map[string]any, 0, len(servers))
	for _, s := range servers {
		if s.Name == "" {
			continue
		}
		if s.URL != "" {
			if !httpOK {
				continue
			}
			entry := map[string]any{"name": s.Name, "type": "http", "url": s.URL}
			if len(s.Headers) > 0 {
				headers := make([]map[string]any, 0, len(s.Headers))
				for _, k := range sortedKeys(s.Headers) {
					headers = append(headers, map[string]any{"name": k, "value": s.Headers[k]})
				}
				entry["headers"] = headers
			}
			out = append(out, entry)
			continue
		}
		if s.Command == "" {
			continue
		}
		entry := map[string]any{"name": s.Name, "command": s.Command}
		if len(s.Args) > 0 {
			entry["args"] = s.Args
		}
		if len(s.Env) > 0 {
			env := make([]map[string]any, 0, len(s.Env))
			for _, k := range sortedKeys(s.Env) {
				env = append(env, map[string]any{"name": k, "value": s.Env[k]})
			}
			entry["env"] = env
		}
		out = append(out, entry)
	}
	return out
}

// tomlString renders a TOML basic string. JSON string escaping is a superset
// of the escapes TOML basic strings accept, so a JSON encoder output is valid
// TOML.
func tomlString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func tomlStringList(values []string) string {
	parts := make([]string, 0, len(values))
	for _, v := range values {
		parts = append(parts, tomlString(v))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func tomlStringTable(values map[string]string) string {
	parts := make([]string, 0, len(values))
	for _, k := range sortedKeys(values) {
		parts = append(parts, tomlString(k)+" = "+tomlString(values[k]))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// writeJSONFile writes a mode-0600 JSON file, because an injected server
// environment can hold secrets.
func writeJSONFile(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("agentwire: create config dir: %w", err)
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return fmt.Errorf("agentwire: write %s: %w", path, err)
	}
	return nil
}

// sessionDir creates one temp dir for a session's generated files. It is
// removed when the session ends.
func (rt *Runtime) sessionDir(req *StartRequest, purpose string) (string, error) {
	dir, err := os.MkdirTemp("", "agentwire-"+string(harnessOrDefault(req.Harness))+"-"+purpose+"-")
	if err != nil {
		return "", fmt.Errorf("agentwire: temp dir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}
