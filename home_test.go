package agentwire

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestHomeFiles(t *testing.T) {
	t.Parallel()
	for _, h := range []Harness{Claude, Codex, Grok, OpenCode, Cursor, Pi} {
		files := HomeFiles(h)
		if len(files) == 0 {
			t.Fatalf("%s: no home files", h)
		}
		seen := map[string]bool{}
		for _, f := range files {
			if f.Key == "" || f.Home == "" || f.User == "" {
				t.Errorf("%s: incomplete entry %+v", h, f)
			}
			if seen[f.Key] {
				t.Errorf("%s: key %s twice", h, f.Key)
			}
			seen[f.Key] = true
			if f.Kind == HomeKeys && f.Format == "" {
				t.Errorf("%s/%s: a keys entry needs a format", h, f.Key)
			}
		}
	}
	files := HomeFiles(Claude)
	files[3].Keys = append(files[3].Keys, "x")
	if slices.Contains(HomeFiles(Claude)[3].Keys, "x") {
		t.Error("HomeFiles shares its slices")
	}
	if HomeFiles(Fake) != nil {
		t.Error("fake has a home layout")
	}
}

func TestSharesUserHome(t *testing.T) {
	t.Parallel()
	if !SharesUserHome(Codex) || SharesUserHome(Claude) {
		t.Fatal("wrong user home facts")
	}
	if LoginDir(OpenCode) != "data" || LoginDir(Claude) != "" {
		t.Fatal("wrong login dirs")
	}
}

func TestMergeKeysJSON(t *testing.T) {
	t.Parallel()
	own := []byte(`{"model":"a","keep":1,"hooks":{"x":1}}`)
	user := []byte(`{"model":"b","hooks":{"y":2},"statusLine":"s","extra":true}`)
	out, err := MergeKeys("json", own, user, nil, []string{"hooks", "statusLine"})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["model"] != "b" || m["keep"] != float64(1) || m["extra"] != true || m["statusLine"] != nil {
		t.Fatalf("merge: %v", m)
	}
	if h := m["hooks"].(map[string]any); h["x"] != float64(1) {
		t.Fatalf("hooks changed: %v", m["hooks"])
	}
	out, err = MergeKeys("json", out, user, []string{"statusLine"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"statusLine": "s"`) {
		t.Fatalf("listed key: %s", out)
	}
	if _, err := MergeKeys("json", []byte("{"), user, nil, nil); err == nil {
		t.Fatal("bad own file: no error")
	}
}

func TestMergeKeysTOML(t *testing.T) {
	t.Parallel()
	own := "# own\nmodel = \"a\"\n\n[cli]\nauto_update = false\n"
	user := "model = \"b\"\nnotify = [\"x\"]\n\n[models]\ndefault = \"m\"\n"
	out, err := MergeKeys("toml", []byte(own), []byte(user), []string{"model", "models.default"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := tomlValues(string(out))
	if got["model"] != `"b"` || got["models.default"] != `"m"` || got["cli.auto_update"] != "false" || got["notify"] != "" {
		t.Fatalf("merge: %q", out)
	}
	out, err = DropKeys("toml", out, []string{"models.default", "model"})
	if err != nil {
		t.Fatal(err)
	}
	if got := tomlValues(string(out)); got["model"] != "" || got["models.default"] != "" {
		t.Fatalf("drop: %q", out)
	}
}

func TestSetKeys(t *testing.T) {
	t.Parallel()
	out, err := SetKeys(".claude.json", []byte(`{"lastOnboardingVersion":"2"}`), HomeDefaults(Claude))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["hasCompletedOnboarding"] != true || m["lastOnboardingVersion"] != "2" {
		t.Fatalf("defaults: %v", m)
	}
	// A vendor can append a second [cli] table; the set folds it.
	in := "[cli]\ntheme = \"x\"\n\n[harness]\na = 1\n\n[cli]\nauto_update = true\n"
	out, err = SetKeys("config.toml", []byte(in), []HomeKey{
		{File: "config.toml", Key: "cli.auto_update", Value: false},
		{File: "config.toml", Key: "compat.claude.hooks", Value: false},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(out), "[cli]") != 1 {
		t.Fatalf("no fold: %q", out)
	}
	got := tomlValues(string(out))
	if got["cli.auto_update"] != "false" || got["cli.theme"] != `"x"` || got["harness.a"] != "1" || got["compat.claude.hooks"] != "false" {
		t.Fatalf("set: %q", out)
	}
	if _, err := SetKeys("x.yaml", nil, nil); err == nil {
		t.Fatal("yaml: no error")
	}
}

func TestSetHomeMCP(t *testing.T) {
	t.Parallel()
	servers := []MCPServer{{Name: "tool", Command: "/bin/tool", Args: []string{"mcp", "serve"}}}
	out, err := SetHomeMCP(Claude, []byte(`{"mcpServers":{"other":{"command":"o"}},"x":1}`), servers)
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
		X          int                       `json:"x"`
	}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m.X != 1 || m.MCPServers["other"] == nil || m.MCPServers["tool"]["type"] != "stdio" || m.MCPServers["tool"]["command"] != "/bin/tool" {
		t.Fatalf("claude: %s", out)
	}
	toml := "[mcp_servers.tool]\nurl = \"http://old\"\n\n[mcp_servers.tool.env]\nA = \"1\"\n\n[cli]\nauto_update = false\n"
	out, err = SetHomeMCP(Grok, []byte(toml), servers)
	if err != nil {
		t.Fatal(err)
	}
	got := tomlValues(string(out))
	if got["mcp_servers.tool.command"] != `"/bin/tool"` || got["mcp_servers.tool.url"] != "" || got["mcp_servers.tool.env.A"] != "" || got["cli.auto_update"] != "false" {
		t.Fatalf("grok: %q", out)
	}
	if HomeMCPFile(Grok) != "config.toml" {
		t.Fatal("grok MCP file")
	}
	if _, err := SetHomeMCP(Pi, nil, servers); err == nil {
		t.Fatal("pi: no error")
	}
}
