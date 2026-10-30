package agentwire

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"time"
)

// authFacts are the login facts of one harness.
type authFacts struct {
	login  []string          // the login command after the binary; nil: log in inside the TUI
	logout []string          // the logout command after the binary; nil: no command
	fixed  map[string]string // login env that does not depend on the home
	// homeEnv are the env keys that point the vendor at a login home.
	homeEnv []string
	// home is the default login home, relative to the user's home folder.
	home string
	// files are the credential files, relative to the login home.
	files []string
	// read parses the credential files of a login home.
	read func(dir string) credFile
	// fileOK reports that a valid credential file is a usable login when
	// the vendor probe cannot answer.
	fileOK bool
}

var authTable = map[Harness]authFacts{
	Claude: {login: []string{"auth", "login"}, logout: []string{"auth", "logout"},
		homeEnv: []string{"CLAUDE_CONFIG_DIR"}, home: ".claude", files: []string{".credentials.json"}, read: readClaudeCred},
	Codex: {login: []string{"login", "--device-auth"}, logout: []string{"logout"},
		homeEnv: []string{"CODEX_HOME"}, home: ".codex", files: []string{"auth.json"}, read: readCodexCred},
	// OpenCode keeps its login in the XDG data folder, not in its config
	// folder. 1.x writes auth.json, 2.x writes opencode.db.
	OpenCode: {login: []string{"auth", "login"}, logout: []string{"auth", "logout"},
		homeEnv: []string{"XDG_DATA_HOME"}, home: filepath.Join(".local", "share"),
		files: []string{filepath.Join("opencode", "auth.json"), filepath.Join("opencode", "opencode.db")}, read: readOpenCodeCred},
	OpenCode2: {login: []string{"auth", "login"}, logout: []string{"auth", "logout"},
		homeEnv: []string{"XDG_DATA_HOME"}, home: filepath.Join(".local", "share"),
		files: []string{filepath.Join("opencode", "auth.json"), filepath.Join("opencode", "opencode.db")}, read: readOpenCodeCred},
	Pi: {homeEnv: []string{"PI_CODING_AGENT_DIR"}, home: filepath.Join(".pi", "agent"),
		files: []string{"auth.json"}, read: readPiCred},
	// omp keeps its login in a SQLite file. The probe gives the state.
	OMP: {homeEnv: []string{"PI_CODING_AGENT_DIR"}, home: filepath.Join(".omp", "agent"),
		files: []string{"agent.db"}, read: readExists("agent.db")},
	Grok: {login: []string{"login", "--device-auth"},
		homeEnv: []string{"GROK_HOME"}, home: ".grok", files: []string{"auth.json"}, read: readGrokCred, fileOK: true},
	Cursor: {login: []string{"login"}, logout: []string{"logout"},
		homeEnv: []string{"CURSOR_CONFIG_DIR", "CURSOR_DATA_DIR"}, home: ".cursor", files: []string{"cli-config.json"}, read: readCursorCred},
	// gemini has no login command: the first start asks. NO_BROWSER prints
	// the URL instead of opening a browser.
	Gemini: {login: []string{}, fixed: map[string]string{"NO_BROWSER": "1"},
		home: ".gemini", files: []string{"oauth_creds.json"}, read: readGeminiCred},
	// copilot keeps its token in the OS keychain and the users in config.json.
	Copilot: {home: ".copilot", files: []string{"config.json"}, read: readCopilotCred},
}

// LoginCommand is the command line that starts an interactive vendor login,
// with the binary first. Nil means that the user logs in inside the TUI with
// /login. Run it with LoginEnv.
func LoginCommand(h Harness) []string {
	f, ok := authTable[h]
	if !ok || f.login == nil {
		return nil
	}
	return append([]string{binName(h)}, f.login...)
}

// LogoutCommand is the vendor logout command line, with the binary first.
// Nil means that the vendor has no logout command.
func LogoutCommand(h Harness) []string {
	f, ok := authTable[h]
	if !ok || f.logout == nil {
		return nil
	}
	return append([]string{binName(h)}, f.logout...)
}

// LoginEnv is the env for LoginCommand and LogoutCommand. A home that is
// not "" points the vendor at that login home. For OpenCode the home is the
// XDG data folder, which is not the StartRequest.Home config folder.
func LoginEnv(h Harness, home string) map[string]string {
	f := authTable[h]
	env := maps.Clone(f.fixed)
	for k, v := range homeEnv(h, home) {
		if env == nil {
			env = map[string]string{}
		}
		env[k] = v
	}
	return env
}

func homeEnv(h Harness, home string) map[string]string {
	f := authTable[h]
	if home == "" || len(f.homeEnv) == 0 {
		return nil
	}
	env := make(map[string]string, len(f.homeEnv))
	for _, k := range f.homeEnv {
		env[k] = home
	}
	return env
}

// CredentialFiles are the vendor credential files, relative to the login
// home. DefaultHome gives the home of the user's own login.
func CredentialFiles(h Harness) []string {
	return slices.Clone(authTable[h].files)
}

// DefaultHome is the login home of the user's own login, such as
// ~/.claude. It is "" for a harness with no login facts.
func DefaultHome(h Harness) string {
	f, ok := authTable[h]
	if !ok {
		return ""
	}
	user, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(user, f.home)
}

// TokenEnvKeys are the env keys whose value is a usable login for h, such
// as an API key.
func TokenEnvKeys(h Harness) []string {
	return slices.Clone(authEnvKeys[h])
}

// AuthStatus tells the login state of h in a login home. A home of "" means
// the user's own login, where a token env key also counts. It asks the
// vendor first, then reads the credential files. A login on file that the
// vendor rejects is AuthExpired.
func AuthStatus(ctx context.Context, h Harness, home string) (AuthState, error) {
	f, ok := authTable[h]
	if !ok {
		return AuthUnknown, fmt.Errorf("%w: no login facts for harness %s", ErrUnsupported, h)
	}
	if err := ctx.Err(); err != nil {
		return AuthUnknown, err
	}
	if home == "" {
		for _, k := range authEnvKeys[h] {
			if os.Getenv(k) != "" {
				return AuthOK, nil
			}
		}
	}
	dir := home
	if dir == "" {
		dir = DefaultHome(h)
	}
	var cred credFile
	if dir != "" && f.read != nil {
		cred = f.read(dir)
	}
	state, conclusive := probeLogin(ctx, h, home, cred)
	if err := ctx.Err(); err != nil {
		return AuthUnknown, err
	}
	if conclusive {
		if state == AuthMissing && cred.login {
			return AuthExpired, nil
		}
		return state, nil
	}
	switch {
	case cred.login && cred.expired(time.Now()) && !cred.refresh:
		return AuthExpired, nil
	case cred.login && f.fileOK:
		return AuthOK, nil
	case cred.exists:
		return AuthUnknown, nil
	}
	return AuthMissing, nil
}

// probeLogin asks the vendor binary for the login state.
func probeLogin(ctx context.Context, h Harness, home string, cred credFile) (AuthState, bool) {
	bin, err := exec.LookPath(binName(h))
	if err != nil {
		return AuthUnknown, false
	}
	var env []string
	for k, v := range homeEnv(h, home) {
		env = append(env, k+"="+v)
	}
	if h == Pi {
		return piAuthCheck(ctx, bin, env)
	}
	if _, ok := authProbes[h]; !ok {
		return AuthUnknown, false
	}
	state, _ := runAuthProbe(ctx, h, bin, env)
	// At the expiry, grok can report "not authenticated" while it refreshes
	// the token. A second call sees the new token.
	if h == Grok && state == AuthMissing && cred.refresh {
		state, _ = runAuthProbe(ctx, h, bin, env)
	}
	return state, state != AuthUnknown
}

// piProviders are the providers that `pi auth check` knows.
var piProviders = []string{"xai", "anthropic", "openai", "google", "openrouter"}

// piAuthCheck asks pi for each provider. One ready provider is a login.
// pi gives no clear "logged out" answer, so no answer is inconclusive.
func piAuthCheck(ctx context.Context, bin string, env []string) (AuthState, bool) {
	env = append(env, "PI_OFFLINE=1")
	for _, p := range piProviders {
		probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		cmd := exec.CommandContext(probeCtx, bin, "auth", "check", "--provider", p, "--json")
		cmd.Env = append(os.Environ(), env...)
		out, err := cmd.Output()
		cancel()
		if err != nil {
			continue
		}
		var payload struct {
			Status string `json:"status"`
		}
		if json.Unmarshal(out, &payload) == nil && payload.Status == "ready" {
			return AuthOK, true
		}
	}
	return AuthUnknown, false
}

// credFile is what the credential files of a login home tell.
type credFile struct {
	exists  bool      // a credential file is there
	login   bool      // the file holds a login
	expires time.Time // the access token expiry; zero when unknown
	refresh bool      // the vendor can refresh the access token
}

func (c credFile) expired(now time.Time) bool {
	return !c.expires.IsZero() && c.expires.Before(now)
}

// readJSON reads one JSON file. ok=false means that the file is not there.
// A file that does not parse leaves v as it is.
func readJSON(path string, v any) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	_ = json.Unmarshal(b, v)
	return true
}

func readExists(name string) func(dir string) credFile {
	return func(dir string) credFile {
		_, err := os.Stat(filepath.Join(dir, name))
		return credFile{exists: err == nil}
	}
}

// readClaudeCred reads .credentials.json. On macOS the token is in the
// Keychain, and the file can hold only the expiry.
func readClaudeCred(dir string) credFile {
	var f struct {
		OAuth struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    any    `json:"expiresAt"`
		} `json:"claudeAiOauth"`
	}
	c := credFile{exists: readJSON(filepath.Join(dir, ".credentials.json"), &f)}
	c.login = f.OAuth.AccessToken != ""
	if c.login {
		c.expires = coerceTime(f.OAuth.ExpiresAt)
		c.refresh = f.OAuth.RefreshToken != ""
	}
	return c
}

func readCodexCred(dir string) credFile {
	var f struct {
		APIKey string `json:"OPENAI_API_KEY"`
		Tokens struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		} `json:"tokens"`
		ExpiresAt any `json:"expires_at"`
	}
	c := credFile{exists: readJSON(filepath.Join(dir, "auth.json"), &f)}
	c.login = f.APIKey != "" || f.Tokens.AccessToken != "" || f.Tokens.RefreshToken != ""
	c.expires = coerceTime(f.ExpiresAt)
	c.refresh = f.Tokens.RefreshToken != ""
	return c
}

// readGrokCred reads auth.json: a map of "<issuer>::<client>" to an account.
// The first key in sort order is the active account.
func readGrokCred(dir string) credFile {
	var f map[string]json.RawMessage
	c := credFile{exists: readJSON(filepath.Join(dir, "auth.json"), &f)}
	keys := make([]string, 0, len(f))
	for k := range f {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		var a struct {
			ExpiresAt    any    `json:"expires_at"`
			RefreshToken string `json:"refresh_token"`
		}
		if json.Unmarshal(f[k], &a) != nil {
			continue
		}
		c.login = true
		c.expires = coerceTime(a.ExpiresAt)
		c.refresh = a.RefreshToken != ""
		break
	}
	return c
}

// readPiCred reads auth.json. Each key is a provider with a login.
func readPiCred(dir string) credFile {
	var f map[string]any
	c := credFile{exists: readJSON(filepath.Join(dir, "auth.json"), &f)}
	c.login = len(f) > 0
	return c
}

func readOpenCodeCred(dir string) credFile {
	c := readPiCred(filepath.Join(dir, "opencode"))
	if !c.exists {
		c = readExists(filepath.Join("opencode", "opencode.db"))(dir)
	}
	return c
}

func readGeminiCred(dir string) credFile {
	var f struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiryDate   any    `json:"expiry_date"`
	}
	c := credFile{exists: readJSON(filepath.Join(dir, "oauth_creds.json"), &f)}
	c.login = f.AccessToken != "" || f.RefreshToken != ""
	c.expires = coerceTime(f.ExpiryDate)
	c.refresh = f.RefreshToken != ""
	return c
}

func readCopilotCred(dir string) credFile {
	var f struct {
		LoggedInUsers    []json.RawMessage `json:"loggedInUsers"`
		LastLoggedInUser json.RawMessage   `json:"lastLoggedInUser"`
	}
	c := credFile{exists: readJSON(filepath.Join(dir, "config.json"), &f)}
	c.login = len(f.LoggedInUsers) > 0 || (len(f.LastLoggedInUser) > 0 && string(f.LastLoggedInUser) != "null")
	return c
}

func readCursorCred(dir string) credFile {
	var f struct {
		AuthInfo json.RawMessage `json:"authInfo"`
	}
	c := credFile{exists: readJSON(filepath.Join(dir, "cli-config.json"), &f)}
	c.login = len(f.AuthInfo) > 0 && string(f.AuthInfo) != "null"
	return c
}

// coerceTime reads an expiry: milliseconds or seconds since the epoch, or
// an RFC 3339 string. It gives zero when the value is not a time.
func coerceTime(v any) time.Time {
	switch n := v.(type) {
	case float64:
		if n > 1e12 {
			return time.UnixMilli(int64(n)).UTC()
		}
		if n > 1e9 {
			return time.Unix(int64(n), 0).UTC()
		}
	case string:
		if t, err := time.Parse(time.RFC3339Nano, n); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}
