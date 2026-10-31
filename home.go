package agentwire

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
)

// HomeFileKind tells how a user file reaches a harness home.
type HomeFileKind string

const (
	// HomeLink links the home path to the user file or folder.
	HomeLink HomeFileKind = "link"
	// HomeEach links each entry of the user folder into the home folder.
	HomeEach HomeFileKind = "each"
	// HomeKeys copies top-level keys of the user file into the home file.
	HomeKeys HomeFileKind = "keys"
	// HomeRead marks a user file that the vendor reads itself. An own copy
	// lives at Home, and OwnEnv points the vendor at it.
	HomeRead HomeFileKind = "read"
)

// HomeFile is one vendor file or folder that a harness home can share with
// the user's own setup, or keep as an own copy.
type HomeFile struct {
	Key  string       `json:"key"`
	Kind HomeFileKind `json:"kind"`
	// User is the path relative to the user's home folder.
	User string `json:"user"`
	// Home is the path relative to the harness home.
	Home string `json:"home"`
	// Format is json, toml, or markdown for a file, and empty for a folder.
	Format string `json:"format,omitempty"`
	// Keys are the keys that a HomeKeys entry copies. A TOML key can be
	// "table.key". Nil copies each key that no other entry of the same
	// home file names.
	Keys []string `json:"keys,omitempty"`
	// Share is the default: true shares the user file.
	Share bool `json:"share"`
	// Login marks a login file. The auth mode decides its share.
	Login bool `json:"login,omitempty"`
	// Hooks marks a file or a key that holds user hooks.
	Hooks bool `json:"hooks,omitempty"`
	// OwnEnv is the env key that points the vendor at the own copy.
	OwnEnv string `json:"own_env,omitempty"`
}

// HomeKey is one value that a home file must hold.
type HomeKey struct {
	// File is the path relative to the harness home.
	File string `json:"file"`
	// Key is a top-level JSON key, or a TOML key with its table:
	// "table.key".
	Key   string `json:"key"`
	Value any    `json:"value"`
	// Keep keeps a value that the file holds already.
	Keep bool `json:"keep,omitempty"`
}

var homeFiles = map[Harness][]HomeFile{
	Claude: {
		{Key: "skills", Kind: HomeLink, User: ".claude/skills", Home: "skills", Share: true},
		{Key: "plugins", Kind: HomeLink, User: ".claude/plugins", Home: "plugins", Share: true},
		{Key: "user_memory", Kind: HomeLink, User: ".claude/CLAUDE.md", Home: "CLAUDE.md", Format: "markdown", Share: true},
		{Key: "settings_prefs", Kind: HomeKeys, User: ".claude/settings.json", Home: "settings.json", Format: "json", Share: true},
		{Key: "hooks", Kind: HomeKeys, User: ".claude/settings.json", Home: "settings.json", Format: "json", Keys: []string{"hooks"}, Share: true, Hooks: true},
		{Key: "plugin_enablement", Kind: HomeKeys, User: ".claude/settings.json", Home: "settings.json", Format: "json", Keys: []string{"enabledPlugins"}, Share: true},
		{Key: "status_line", Kind: HomeKeys, User: ".claude/settings.json", Home: "settings.json", Format: "json", Keys: []string{"statusLine"}},
		{Key: "settings_local", Kind: HomeLink, User: ".claude/settings.local.json", Home: "settings.local.json", Format: "json", Share: true},
		{Key: "commands", Kind: HomeLink, User: ".claude/commands", Home: "commands", Share: true},
		{Key: "agents", Kind: HomeLink, User: ".claude/agents", Home: "agents", Share: true},
		{Key: "rules", Kind: HomeLink, User: ".claude/rules", Home: "rules", Share: true},
		{Key: "keybindings", Kind: HomeLink, User: ".claude/keybindings.json", Home: "keybindings.json", Format: "json"},
		{Key: "mcp", Kind: HomeLink, User: ".claude/mcp.json", Home: "mcp.json", Format: "json", Share: true},
		{Key: "mcp_servers", Kind: HomeKeys, User: ".claude.json", Home: ".claude.json", Format: "json", Keys: []string{"mcpServers"}, Share: true},
		{Key: "appearance", Kind: HomeKeys, User: ".claude.json", Home: ".claude.json", Format: "json", Keys: []string{"theme", "userTheme", "preferredTheme"}, Share: true},
		{Key: "login", Kind: HomeLink, User: ".claude/.credentials.json", Home: ".credentials.json", Format: "json", Login: true},
		// The account picks the org that the keychain token bills.
		{Key: "login_account", Kind: HomeKeys, User: ".claude.json", Home: ".claude.json", Format: "json", Keys: []string{"oauthAccount"}, Login: true},
	},
	Codex: {
		{Key: "config_prefs", Kind: HomeKeys, User: ".codex/config.toml", Home: "config.toml", Format: "toml",
			Keys: []string{"permission_mode", "auto_compact_threshold_percent", "model", "models.default"}, Share: true},
		{Key: "hooks", Kind: HomeKeys, User: ".codex/config.toml", Home: "config.toml", Format: "toml", Keys: []string{"notify"}, Share: true, Hooks: true},
		{Key: "skills", Kind: HomeLink, User: ".codex/skills", Home: "skills", Share: true},
		{Key: "prompts", Kind: HomeLink, User: ".codex/prompts", Home: "prompts", Share: true},
		{Key: "mcp", Kind: HomeLink, User: ".codex/mcp.json", Home: "mcp.json", Format: "json", Share: true},
		{Key: "user_memory", Kind: HomeLink, User: ".codex/AGENTS.md", Home: "AGENTS.md", Format: "markdown", Share: true},
		{Key: "login", Kind: HomeLink, User: ".codex/auth.json", Home: "auth.json", Format: "json", Login: true},
	},
	Grok: {
		{Key: "skills", Kind: HomeLink, User: ".grok/skills", Home: "skills", Share: true},
		{Key: "config_prefs", Kind: HomeKeys, User: ".grok/config.toml", Home: "config.toml", Format: "toml",
			Keys: []string{"permission_mode", "auto_compact_threshold_percent", "model", "models.default"}, Share: true},
		{Key: "plugins", Kind: HomeLink, User: ".grok/installed-plugins", Home: "installed-plugins", Share: true},
		{Key: "user_memory", Kind: HomeLink, User: ".grok/AGENTS.md", Home: "AGENTS.md", Format: "markdown", Share: true},
		{Key: "login", Kind: HomeLink, User: ".grok/auth.json", Home: "auth.json", Format: "json", Login: true},
	},
	OpenCode: {
		{Key: "plugins", Kind: HomeEach, User: ".config/opencode/plugins", Home: "plugins", Share: true},
		{Key: "skills", Kind: HomeLink, User: ".config/opencode/skills", Home: "skills", Share: true},
		// OpenCode reads the user config and adds the home config dir; an own
		// copy replaces the user file.
		{Key: "config", Kind: HomeRead, User: ".config/opencode/opencode.json", Home: "opencode.own.json", Format: "json", Share: true, OwnEnv: "OPENCODE_CONFIG"},
		{Key: "user_memory", Kind: HomeRead, User: ".config/opencode/AGENTS.md", Home: "AGENTS.md", Format: "markdown", Share: true},
	},
	Cursor: {
		{Key: "mcp", Kind: HomeLink, User: ".cursor/mcp.json", Home: "mcp.json", Format: "json", Share: true},
		{Key: "rules", Kind: HomeLink, User: ".cursor/rules", Home: "rules", Share: true},
		{Key: "skills", Kind: HomeLink, User: ".cursor/skills-cursor", Home: "skills-cursor", Share: true},
	},
	Pi: {
		{Key: "skills", Kind: HomeLink, User: ".pi/agent/skills", Home: "skills", Share: true},
		{Key: "prompts", Kind: HomeLink, User: ".pi/agent/prompts", Home: "prompts", Share: true},
		{Key: "themes", Kind: HomeLink, User: ".pi/agent/themes", Home: "themes", Share: true},
		{Key: "models", Kind: HomeLink, User: ".pi/agent/models.json", Home: "models.json", Format: "json", Share: true},
		{Key: "login", Kind: HomeLink, User: ".pi/agent/auth.json", Home: "auth.json", Format: "json", Login: true},
	},
}

var homeDefaults = map[Harness][]HomeKey{
	// A home with no onboarding mark opens the first-run wizard.
	Claude: {
		{File: ".claude.json", Key: "hasCompletedOnboarding", Value: true},
		{File: ".claude.json", Key: "lastOnboardingVersion", Value: "1.0.9", Keep: true},
	},
	Grok: {{File: "config.toml", Key: "cli.auto_update", Value: false}},
}

// homeMCP is the home file that holds the MCP servers of a harness.
var homeMCP = map[Harness]string{
	Claude: ".claude.json",
	Grok:   "config.toml",
}

// HomeFiles lists the files and folders of a harness home that can share
// the user's own setup. Nil means that the harness has no home layout.
func HomeFiles(h Harness) []HomeFile {
	out := slices.Clone(homeFiles[h])
	for i := range out {
		out[i].Keys = slices.Clone(out[i].Keys)
	}
	return out
}

// HomeDefaults are the values that a harness home needs to start with no
// first-run dialog and no self-update.
func HomeDefaults(h Harness) []HomeKey {
	return slices.Clone(homeDefaults[h])
}

// SharesUserHome reports that a harness runs in the user's own home when it
// shares the user login. Codex cold-starts its plugin sync in a new home,
// omp keeps its login in a database, and OpenCode 2 reads only the user
// config folders.
func SharesUserHome(h Harness) bool {
	switch h {
	case Codex, OMP, OpenCode2:
		return true
	}
	return false
}

// LoginDir is the login home inside a harness home, relative to it. Pass
// it to LoginEnv, AuthStatus, and the credential files. OpenCode keeps its
// login in an XDG data folder, apart from its config folder.
func LoginDir(h Harness) string {
	switch h {
	case OpenCode, OpenCode2:
		return "data"
	}
	return ""
}

// SharedLoginEnv is the env that makes a harness home use the user's own
// login. On macOS, Claude keeps the login in the keychain; an empty
// CLAUDE_SECURESTORAGE_CONFIG_DIR keeps the default keychain entry while
// CLAUDE_CONFIG_DIR points at the home. OpenCode reads the user data folder.
func SharedLoginEnv(h Harness) map[string]string {
	switch h {
	case Claude:
		if runtime.GOOS == "darwin" {
			return map[string]string{"CLAUDE_SECURESTORAGE_CONFIG_DIR": ""}
		}
	case OpenCode, OpenCode2:
		if d := DefaultHome(h); d != "" {
			return map[string]string{"XDG_DATA_HOME": d}
		}
	}
	return nil
}

// HomeMCPFile is the home file that holds the MCP servers of a harness, or
// "" when the harness reads them from elsewhere.
func HomeMCPFile(h Harness) string { return homeMCP[h] }

// SetHomeMCP writes servers into the MCP file of a harness home and gives
// the new content. It replaces a server of the same name.
func SetHomeMCP(h Harness, data []byte, servers []MCPServer) ([]byte, error) {
	switch h {
	case Claude:
		m, err := jsonObject(data)
		if err != nil {
			return nil, err
		}
		cur, _ := m["mcpServers"].(map[string]any)
		if cur == nil {
			cur = map[string]any{}
		}
		for name, v := range claudeMCPServers(servers) {
			if e, ok := v.(map[string]any); ok && e["type"] == nil {
				e["type"] = "stdio"
			}
			cur[name] = v
		}
		m["mcpServers"] = cur
		return jsonBytes(m)
	case Grok:
		out := string(data)
		for _, s := range servers {
			if s.Name == "" {
				continue
			}
			header := "mcp_servers." + s.Name
			out = removeTOMLTable(out, header)
			var b strings.Builder
			b.WriteString("[" + header + "]\n")
			switch {
			case s.URL != "":
				b.WriteString("url = " + tomlString(s.URL) + "\n")
				if len(s.Headers) > 0 {
					b.WriteString("http_headers = " + tomlStringTable(s.Headers) + "\n")
				}
			case s.Command != "":
				b.WriteString("command = " + tomlString(s.Command) + "\n")
				if len(s.Args) > 0 {
					b.WriteString("args = " + tomlStringList(s.Args) + "\n")
				}
				if len(s.Env) > 0 {
					b.WriteString("env = " + tomlStringTable(s.Env) + "\n")
				}
			default:
				continue
			}
			out = appendBlock(out, b.String())
		}
		return []byte(out), nil
	}
	return nil, fmt.Errorf("%w: no MCP file in the home of %s", ErrUnsupported, h)
}

// MergeKeys copies keys of user into own and gives the new own content. A
// nil keys copies each key not in skip. A key that user does not hold
// leaves own as it is. format is json or toml.
func MergeKeys(format string, own, user []byte, keys, skip []string) ([]byte, error) {
	switch format {
	case "json":
		o, err := jsonObject(own)
		if err != nil {
			return nil, fmt.Errorf("own file: %w", err)
		}
		u, err := jsonObject(user)
		if err != nil {
			return nil, fmt.Errorf("user file: %w", err)
		}
		for k, v := range u {
			if (keys == nil || slices.Contains(keys, k)) && !slices.Contains(skip, k) {
				o[k] = v
			}
		}
		return jsonBytes(o)
	case "toml":
		vals := tomlValues(string(user))
		out := string(own)
		for _, k := range slices.Sorted(maps.Keys(vals)) {
			if (keys == nil || slices.Contains(keys, k)) && !slices.Contains(skip, k) {
				out = setTOMLRaw(out, k, vals[k])
			}
		}
		return []byte(out), nil
	}
	return nil, fmt.Errorf("%w: format %q", ErrUnsupported, format)
}

// DropKeys removes keys from a home file and gives the new content.
func DropKeys(format string, data []byte, keys []string) ([]byte, error) {
	switch format {
	case "json":
		m, err := jsonObject(data)
		if err != nil {
			return nil, err
		}
		n := len(m)
		for _, k := range keys {
			delete(m, k)
		}
		if len(m) == n {
			return data, nil
		}
		return jsonBytes(m)
	case "toml":
		out := string(data)
		for _, k := range keys {
			out = removeTOMLKey(out, k)
		}
		return []byte(out), nil
	}
	return nil, fmt.Errorf("%w: format %q", ErrUnsupported, format)
}

// SetKeys writes values into a home file and gives the new content. The
// format comes from the file extension.
func SetKeys(path string, data []byte, keys []HomeKey) ([]byte, error) {
	switch filepath.Ext(path) {
	case ".json":
		m, err := jsonObject(data)
		if err != nil {
			return nil, err
		}
		for _, k := range keys {
			if _, ok := m[k.Key]; ok && k.Keep {
				continue
			}
			m[k.Key] = k.Value
		}
		return jsonBytes(m)
	case ".toml":
		out := string(data)
		have := tomlValues(out)
		for _, k := range keys {
			if _, ok := have[k.Key]; ok && k.Keep {
				continue
			}
			v, err := tomlValue(k.Value)
			if err != nil {
				return nil, fmt.Errorf("key %s: %w", k.Key, err)
			}
			out = setTOMLRaw(out, k.Key, v)
		}
		return []byte(out), nil
	}
	return nil, fmt.Errorf("%w: file %q", ErrUnsupported, path)
}

func jsonObject(data []byte) (map[string]any, error) {
	m := map[string]any{}
	if len(bytes.TrimSpace(data)) == 0 {
		return m, nil
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

func jsonBytes(m map[string]any) ([]byte, error) {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func tomlValue(v any) (string, error) {
	switch x := v.(type) {
	case string:
		return tomlString(x), nil
	case bool:
		return strconv.FormatBool(x), nil
	case int:
		return strconv.Itoa(x), nil
	case int64:
		return strconv.FormatInt(x, 10), nil
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), nil
	case []string:
		return tomlStringList(x), nil
	}
	return "", fmt.Errorf("%w: TOML value %T", ErrUnsupported, v)
}

// tomlValues gives the raw value of each single-line key, by "table.key".
// A top-level key has no table.
func tomlValues(s string) map[string]string {
	out := map[string]string{}
	table := ""
	for line := range strings.SplitSeq(s, "\n") {
		trim := strings.TrimSpace(line)
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		if h, ok := tomlHeader(trim); ok {
			table = h
			continue
		}
		k, v, ok := strings.Cut(trim, "=")
		if !ok {
			continue
		}
		key := strings.TrimSpace(k)
		if table != "" {
			key = table + "." + key
		}
		out[key] = strings.TrimSpace(v)
	}
	return out
}

func tomlHeader(trim string) (string, bool) {
	if !strings.HasPrefix(trim, "[") || strings.HasPrefix(trim, "[[") || !strings.HasSuffix(trim, "]") {
		return "", false
	}
	return strings.TrimSpace(trim[1 : len(trim)-1]), true
}

// splitTOMLKey splits "table.key" at the last dot. A key with no dot is
// top-level.
func splitTOMLKey(k string) (table, key string) {
	if i := strings.LastIndexByte(k, '.'); i >= 0 {
		return k[:i], k[i+1:]
	}
	return "", k
}

// tomlLines gives the lines and the index range [start, end) of each
// table body. The top-level body has the name "".
type tomlLines struct {
	lines  []string
	bodies map[string][2]int
	order  []string
}

func parseTOMLLines(s string) tomlLines {
	t := tomlLines{lines: strings.Split(s, "\n"), bodies: map[string][2]int{}}
	cur, start := "", 0
	closeBody := func(end int) {
		if _, ok := t.bodies[cur]; !ok {
			t.bodies[cur] = [2]int{start, end}
			t.order = append(t.order, cur)
		}
	}
	for i, line := range t.lines {
		trim := strings.TrimSpace(line)
		if h, ok := tomlHeader(trim); ok {
			closeBody(i)
			cur, start = h, i+1
		} else if strings.HasPrefix(trim, "[[") {
			closeBody(i)
			cur, start = "\x00"+strconv.Itoa(i), i+1
		}
	}
	closeBody(len(t.lines))
	return t
}

// setTOMLRaw sets "table.key" to a raw TOML value. It folds a repeated
// table into the first one, because a vendor can append a second copy.
func setTOMLRaw(s, dotted, raw string) string {
	table, key := splitTOMLKey(dotted)
	if table != "" {
		s = foldTOMLTable(s, table)
	}
	t := parseTOMLLines(s)
	line := key + " = " + raw
	body, ok := t.bodies[table]
	if !ok {
		return appendBlock(s, "["+table+"]\n"+line+"\n")
	}
	for i := body[0]; i < body[1]; i++ {
		k, _, ok := strings.Cut(strings.TrimSpace(t.lines[i]), "=")
		if ok && strings.TrimSpace(k) == key {
			t.lines[i] = line
			return strings.Join(t.lines, "\n")
		}
	}
	at := body[1]
	// Put the key after the last line of the body that is not blank.
	for at > body[0] && strings.TrimSpace(t.lines[at-1]) == "" {
		at--
	}
	t.lines = slices.Insert(t.lines, at, line)
	return strings.Join(t.lines, "\n")
}

func removeTOMLKey(s, dotted string) string {
	table, key := splitTOMLKey(dotted)
	t := parseTOMLLines(s)
	body, ok := t.bodies[table]
	if !ok {
		return s
	}
	for i := body[0]; i < body[1]; i++ {
		k, _, ok := strings.Cut(strings.TrimSpace(t.lines[i]), "=")
		if ok && strings.TrimSpace(k) == key {
			return strings.Join(slices.Delete(t.lines, i, i+1), "\n")
		}
	}
	return s
}

// removeTOMLTable removes a table, its sub-tables, and their bodies.
func removeTOMLTable(s, table string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	skip := false
	for _, line := range lines {
		trim := strings.TrimSpace(line)
		if h, ok := tomlHeader(trim); ok {
			skip = h == table || strings.HasPrefix(h, table+".")
		} else if strings.HasPrefix(trim, "[[") {
			skip = false
		}
		if !skip {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// foldTOMLTable moves the bodies of repeated [table] headers into the first.
func foldTOMLTable(s, table string) string {
	var out, moved []string
	first, n, in := -1, 0, false
	for line := range strings.SplitSeq(s, "\n") {
		trim := strings.TrimSpace(line)
		if h, ok := tomlHeader(trim); ok || strings.HasPrefix(trim, "[[") {
			in = ok && h == table
			if in {
				n++
				if n > 1 {
					continue
				}
			}
		}
		if in && n > 1 {
			if trim != "" {
				moved = append(moved, line)
			}
			continue
		}
		out = append(out, line)
		if in && trim != "" {
			first = len(out)
		}
	}
	if n < 2 {
		return s
	}
	return strings.Join(slices.Insert(out, first, moved...), "\n")
}

func appendBlock(s, block string) string {
	if s != "" && !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	if s != "" {
		s += "\n"
	}
	return s + block
}
