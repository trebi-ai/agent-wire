package agentwire

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// injTestRuntime builds a runtime with no state, so a launch never touches a
// shared cache or a ledger.
func injTestRuntime(t *testing.T) *Runtime {
	t.Helper()
	return New(Options{})
}

// injTestArgPair reports whether args holds key immediately before value.
func injTestArgPair(args []string, key, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == key && args[i+1] == value {
			return true
		}
	}
	return false
}

// injTestArgValue returns the value that follows the first key in args.
func injTestArgValue(args []string, key string) (string, bool) {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == key {
			return args[i+1], true
		}
	}
	return "", false
}

func injTestStdioServer() MCPServer {
	return MCPServer{
		Name:    "local",
		Command: "cmd",
		Args:    []string{"a", "b"},
		Env:     map[string]string{"K": "V"},
	}
}

func injTestHTTPServer() MCPServer {
	return MCPServer{
		Name:    "remote",
		URL:     "https://example.test/mcp",
		Headers: map[string]string{"Authorization": "token"},
	}
}

// TestMCPClaudeGolden checks the generated Claude config file, its permissions
// and the args that point at it.
func TestMCPClaudeGolden(t *testing.T) {
	ctx := context.Background()
	rt := injTestRuntime(t)
	req := StartRequest{
		Harness:    Claude,
		Binary:     "/bin/sh",
		MCPServers: []MCPServer{injTestStdioServer(), injTestHTTPServer()},
	}
	l, err := rt.prepare(ctx, &req, false)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	defer l.cleanup()

	path, ok := injTestArgValue(l.args, "--mcp-config")
	if !ok {
		t.Fatalf("no --mcp-config in args %q", l.args)
	}
	if slices.Contains(l.args, "--strict-mcp-config") {
		t.Fatalf("strict flag present without Raw[strict_mcp]: %q", l.args)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("mcp.json mode = %o, want 600", perm)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode %s: %v\n%s", path, err, raw)
	}
	local, ok := doc.MCPServers["local"]
	if !ok {
		t.Fatalf("no local server in %s", raw)
	}
	if local["command"] != "cmd" {
		t.Fatalf("local.command = %v, want cmd", local["command"])
	}
	if got, want := local["args"], []any{"a", "b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("local.args = %v, want %v", got, want)
	}
	env, _ := local["env"].(map[string]any)
	if env["K"] != "V" {
		t.Fatalf("local.env = %v, want K=V", local["env"])
	}
	remote, ok := doc.MCPServers["remote"]
	if !ok {
		t.Fatalf("no remote server in %s", raw)
	}
	if remote["type"] != "http" || remote["url"] != "https://example.test/mcp" {
		t.Fatalf("remote = %v, want type http and the url", remote)
	}

	dir := filepath.Dir(path)
	l.cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("temp dir %s survived cleanup: %v", dir, err)
	}
}

// TestMCPClaudeStrictOptIn checks that Raw[strict_mcp] adds the isolation flag.
func TestMCPClaudeStrictOptIn(t *testing.T) {
	ctx := context.Background()
	rt := injTestRuntime(t)
	req := StartRequest{
		Harness:    Claude,
		Binary:     "/bin/sh",
		MCPServers: []MCPServer{injTestStdioServer()},
		Raw:        map[string]any{"strict_mcp": true},
	}
	l, err := rt.prepare(ctx, &req, false)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	defer l.cleanup()
	if !slices.Contains(l.args, "--strict-mcp-config") {
		t.Fatalf("strict_mcp=true did not add --strict-mcp-config: %q", l.args)
	}
}

// TestMCPCodexArgs checks the exact `-c` override strings.
func TestMCPCodexArgs(t *testing.T) {
	ctx := context.Background()
	rt := injTestRuntime(t)
	req := StartRequest{
		Harness:    Codex,
		Binary:     "/bin/sh",
		MCPServers: []MCPServer{injTestStdioServer(), injTestHTTPServer()},
	}
	l, err := rt.prepare(ctx, &req, false)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	defer l.cleanup()

	want := []string{
		"-c", `mcp_servers.local.command="cmd"`,
		"-c", `mcp_servers.local.args=["a", "b"]`,
		"-c", `mcp_servers.local.env_vars=["K"]`,
		"-c", `mcp_servers.remote.url="https://example.test/mcp"`,
		"-c", `mcp_servers.remote.env_http_headers={"Authorization" = "AGENTWIRE_MCP_REMOTE_AUTHORIZATION"}`,
	}
	if !slices.Equal(l.args, want) {
		t.Fatalf("codex args =\n%q\nwant\n%q", l.args, want)
	}
	if l.env["K"] != "V" || l.env["AGENTWIRE_MCP_REMOTE_AUTHORIZATION"] != "token" {
		t.Fatalf("codex env = %v, want K and the header var", l.env)
	}
}

// TestCodexMCPSecretsInEnvOnly checks that no secret value reaches the args
// and that the env carries each one.
func TestCodexMCPSecretsInEnvOnly(t *testing.T) {
	servers := []MCPServer{
		{Name: "gh", Command: "gh-mcp", Env: map[string]string{"GH_TOKEN": "s3cret-env"}},
		{Name: "api-x", URL: "https://x.test/mcp", Headers: map[string]string{
			"Authorization": "Bearer s3cret-hdr",
			"X-Org":         "acme",
		}},
	}
	args := CodexMCPArgs(servers)
	joined := strings.Join(args, " ")
	for _, secret := range []string{"s3cret-env", "s3cret-hdr", "acme"} {
		if strings.Contains(joined, secret) {
			t.Fatalf("args hold %q: %q", secret, args)
		}
	}
	if !injTestArgPair(args, "-c", `mcp_servers.gh.env_vars=["GH_TOKEN"]`) {
		t.Fatalf("missing env_vars: %q", args)
	}
	wantHeaders := `mcp_servers.api-x.env_http_headers={"Authorization" = "AGENTWIRE_MCP_API_X_AUTHORIZATION", "X-Org" = "AGENTWIRE_MCP_API_X_X_ORG"}`
	if !injTestArgPair(args, "-c", wantHeaders) {
		t.Fatalf("missing env_http_headers: %q", args)
	}

	env, err := CodexMCPEnv(servers)
	if err != nil {
		t.Fatalf("CodexMCPEnv: %v", err)
	}
	want := map[string]string{
		"GH_TOKEN":                          "s3cret-env",
		"AGENTWIRE_MCP_API_X_AUTHORIZATION": "Bearer s3cret-hdr",
		"AGENTWIRE_MCP_API_X_X_ORG":         "acme",
	}
	if !reflect.DeepEqual(env, want) {
		t.Fatalf("env = %v, want %v", env, want)
	}
}

// TestCodexMCPEnvConflict checks that one key with two values is an error,
// and that one key with one value is not.
func TestCodexMCPEnvConflict(t *testing.T) {
	same := []MCPServer{
		{Name: "a", Command: "a", Env: map[string]string{"TOKEN": "x"}},
		{Name: "b", Command: "b", Env: map[string]string{"TOKEN": "x"}},
	}
	if _, err := CodexMCPEnv(same); err != nil {
		t.Fatalf("same value: %v", err)
	}
	diff := []MCPServer{
		{Name: "a", Command: "a", Env: map[string]string{"TOKEN": "x"}},
		{Name: "b", Command: "b", Env: map[string]string{"TOKEN": "y"}},
	}
	if _, err := CodexMCPEnv(diff); err == nil {
		t.Fatal("two values for TOKEN: want an error")
	}

	rt := injTestRuntime(t)
	req := StartRequest{
		Harness:    Codex,
		Binary:     "/bin/sh",
		Env:        map[string]string{"TOKEN": "job"},
		MCPServers: same,
	}
	if _, err := rt.prepare(context.Background(), &req, false); err == nil {
		t.Fatal("MCP env over a different session env value: want an error")
	}
}

// TestMCPOpenCodeConfig checks the OPENCODE_CONFIG_CONTENT fragment.
func TestMCPOpenCodeConfig(t *testing.T) {
	ctx := context.Background()
	rt := injTestRuntime(t)
	req := StartRequest{
		Harness:    OpenCode,
		Binary:     "/bin/sh",
		MCPServers: []MCPServer{injTestStdioServer(), injTestHTTPServer()},
	}
	l, err := rt.prepare(ctx, &req, false)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	defer l.cleanup()

	content, ok := l.env["OPENCODE_CONFIG_CONTENT"]
	if !ok {
		t.Fatalf("no OPENCODE_CONFIG_CONTENT in env %v", l.env)
	}
	var cfg struct {
		MCP map[string]map[string]any `json:"mcp"`
	}
	if err := json.Unmarshal([]byte(content), &cfg); err != nil {
		t.Fatalf("decode %q: %v", content, err)
	}
	local, ok := cfg.MCP["local"]
	if !ok {
		t.Fatalf("no local entry in %q", content)
	}
	if local["type"] != "local" {
		t.Fatalf("local.type = %v, want local", local["type"])
	}
	if got, want := local["command"], []any{"cmd", "a", "b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("local.command = %v, want %v", got, want)
	}
	env, _ := local["environment"].(map[string]any)
	if env["K"] != "V" {
		t.Fatalf("local.environment = %v, want K=V", local["environment"])
	}
	remote, ok := cfg.MCP["remote"]
	if !ok {
		t.Fatalf("no remote entry in %q", content)
	}
	if remote["type"] != "remote" || remote["url"] != "https://example.test/mcp" {
		t.Fatalf("remote = %v, want type remote and the url", remote)
	}
}

// TestMCPPiHome checks the pi error without Home and the pi mcp.json with it.
func TestMCPPiHome(t *testing.T) {
	ctx := context.Background()
	rt := injTestRuntime(t)
	req := StartRequest{
		Harness:    Pi,
		Binary:     "/bin/sh",
		MCPServers: []MCPServer{injTestStdioServer()},
	}
	if _, err := rt.prepare(ctx, &req, false); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("pi without Home: err = %v, want ErrUnsupported", err)
	}

	req.Home = t.TempDir()
	l, err := rt.prepare(ctx, &req, false)
	if err != nil {
		t.Fatalf("pi with Home: prepare: %v", err)
	}
	defer l.cleanup()

	path := filepath.Join(req.Home, "mcp.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	local, ok := doc.MCPServers["local"]
	if !ok {
		t.Fatalf("no local server in %s", raw)
	}
	if local["command"] != "cmd" {
		t.Fatalf("local.command = %v, want cmd", local["command"])
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("mcp.json mode = %o, want 600", perm)
	}
}

// TestMCPWireHarnessesRenderNothing checks that the ACP harnesses keep the
// servers off the command line: they travel in session/new.
func TestMCPWireHarnessesRenderNothing(t *testing.T) {
	ctx := context.Background()
	rt := injTestRuntime(t)
	for _, h := range []Harness{Copilot, Cursor, Gemini} {
		req := StartRequest{
			Harness:    h,
			Binary:     "/bin/sh",
			MCPServers: []MCPServer{injTestStdioServer(), injTestHTTPServer()},
		}
		l, err := rt.prepare(ctx, &req, false)
		if err != nil {
			t.Fatalf("%s: prepare: %v", h, err)
		}
		if len(l.env) != 0 {
			t.Fatalf("%s: env = %v, want no MCP env", h, l.env)
		}
		for _, a := range l.args {
			low := strings.ToLower(a)
			if strings.Contains(low, "mcp") || strings.Contains(a, "local") || strings.Contains(a, "remote") || strings.Contains(a, "cmd") {
				t.Fatalf("%s: MCP leaked into args: %q", h, l.args)
			}
		}
	}
}

// TestMCPACPServers checks the ACP session parameter rendering, including the
// HTTP capability gate.
func TestMCPACPServers(t *testing.T) {
	stdio := injTestStdioServer()
	http := injTestHTTPServer()
	stdioWant := map[string]any{
		"name":    "local",
		"command": "cmd",
		"args":    []string{"a", "b"},
		"env":     []map[string]any{{"name": "K", "value": "V"}},
	}
	httpWant := map[string]any{
		"name":    "remote",
		"type":    "http",
		"url":     "https://example.test/mcp",
		"headers": []map[string]any{{"name": "Authorization", "value": "token"}},
	}

	tests := []struct {
		name    string
		servers []MCPServer
		httpOK  bool
		want    []map[string]any
	}{
		{"stdio", []MCPServer{stdio}, true, []map[string]any{stdioWant}},
		{"http allowed", []MCPServer{http}, true, []map[string]any{httpWant}},
		{"http refused", []MCPServer{http}, false, nil},
		{"http refused keeps stdio", []MCPServer{http, stdio}, false, []map[string]any{stdioWant}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := acpMCPServers(tt.servers, tt.httpOK)
			if tt.want == nil {
				if len(got) != 0 {
					t.Fatalf("got %v, want no servers", got)
				}
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}
