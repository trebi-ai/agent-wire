package agentwire

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestLoginCommand(t *testing.T) {
	t.Parallel()
	cases := map[Harness]struct{ login, logout []string }{
		Claude:    {[]string{"claude", "auth", "login"}, []string{"claude", "auth", "logout"}},
		Codex:     {[]string{"codex", "login", "--device-auth"}, []string{"codex", "logout"}},
		OpenCode:  {[]string{"opencode", "auth", "login"}, []string{"opencode", "auth", "logout"}},
		OpenCode2: {[]string{"opencode2", "auth", "login"}, []string{"opencode2", "auth", "logout"}},
		Grok:      {[]string{"grok", "login", "--device-auth"}, nil},
		Cursor:    {[]string{"cursor-agent", "login"}, []string{"cursor-agent", "logout"}},
		Gemini:    {[]string{"gemini"}, nil},
		Pi:        {nil, nil},
		Copilot:   {nil, nil},
		OMP:       {nil, nil},
		Fake:      {nil, nil},
		Native:    {nil, nil},
	}
	for h, want := range cases {
		if got := LoginCommand(h); !slices.Equal(got, want.login) || (got == nil) != (want.login == nil) {
			t.Errorf("LoginCommand(%s) = %q, want %q", h, got, want.login)
		}
		if got := LogoutCommand(h); !slices.Equal(got, want.logout) {
			t.Errorf("LogoutCommand(%s) = %q, want %q", h, got, want.logout)
		}
	}
}

func TestLoginEnv(t *testing.T) {
	t.Parallel()
	cases := []struct {
		h    Harness
		home string
		want map[string]string
	}{
		{Claude, "/h", map[string]string{"CLAUDE_CONFIG_DIR": "/h"}},
		{Codex, "/h", map[string]string{"CODEX_HOME": "/h"}},
		{OpenCode, "/h", map[string]string{"XDG_DATA_HOME": "/h"}},
		{Grok, "/h", map[string]string{"GROK_HOME": "/h"}},
		{Cursor, "/h", map[string]string{"CURSOR_CONFIG_DIR": "/h", "CURSOR_DATA_DIR": "/h"}},
		{Pi, "/h", map[string]string{"PI_CODING_AGENT_DIR": "/h"}},
		{OMP, "/h", map[string]string{"PI_CODING_AGENT_DIR": "/h"}},
		{Gemini, "/h", map[string]string{"NO_BROWSER": "1"}},
		{Gemini, "", map[string]string{"NO_BROWSER": "1"}},
		{Claude, "", nil},
		{Copilot, "/h", nil},
		{Fake, "/h", nil},
	}
	for _, c := range cases {
		if got := LoginEnv(c.h, c.home); !maps.Equal(got, c.want) {
			t.Errorf("LoginEnv(%s, %q) = %v, want %v", c.h, c.home, got, c.want)
		}
	}
	LoginEnv(Gemini, "")["NO_BROWSER"] = "0"
	if LoginEnv(Gemini, "")["NO_BROWSER"] != "1" {
		t.Fatal("LoginEnv leaks the shared table")
	}
}

func TestCredentialFiles(t *testing.T) {
	user := t.TempDir()
	t.Setenv("HOME", user)
	cases := []struct {
		h     Harness
		home  string
		files []string
	}{
		{Claude, ".claude", []string{".credentials.json"}},
		{Codex, ".codex", []string{"auth.json"}},
		{OpenCode, ".local/share", []string{"opencode/auth.json", "opencode/opencode.db"}},
		{Pi, ".pi/agent", []string{"auth.json"}},
		{OMP, ".omp/agent", []string{"agent.db"}},
		{Grok, ".grok", []string{"auth.json"}},
		{Cursor, ".cursor", []string{"cli-config.json"}},
		{Gemini, ".gemini", []string{"oauth_creds.json"}},
		{Copilot, ".copilot", []string{"config.json"}},
	}
	for _, c := range cases {
		want := make([]string, len(c.files))
		for i, f := range c.files {
			want[i] = filepath.FromSlash(f)
		}
		if got := CredentialFiles(c.h); !slices.Equal(got, want) {
			t.Errorf("CredentialFiles(%s) = %q, want %q", c.h, got, want)
		}
		if got := DefaultHome(c.h); got != filepath.Join(user, filepath.FromSlash(c.home)) {
			t.Errorf("DefaultHome(%s) = %q", c.h, got)
		}
	}
	if CredentialFiles(Fake) != nil || DefaultHome(Fake) != "" {
		t.Error("Fake has login facts")
	}
}

func TestTokenEnvKeys(t *testing.T) {
	t.Parallel()
	cases := map[Harness][]string{
		Claude:  {"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"},
		Copilot: {"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"},
		Cursor:  {"CURSOR_API_KEY"},
		OMP:     nil,
	}
	for h, want := range cases {
		if got := TokenEnvKeys(h); !slices.Equal(got, want) {
			t.Errorf("TokenEnvKeys(%s) = %q, want %q", h, got, want)
		}
	}
}

// writeFile writes data under dir and makes the parent folders.
func writeFile(t *testing.T, dir, name, data string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

// isolateAuth gives the test an empty PATH, a temp user home, and no token
// env keys.
func isolateAuth(t *testing.T) (bin, user string) {
	t.Helper()
	bin, user = t.TempDir(), t.TempDir()
	t.Setenv("PATH", bin)
	t.Setenv("HOME", user)
	for _, keys := range authEnvKeys {
		for _, k := range keys {
			t.Setenv(k, "")
		}
	}
	return bin, user
}

// stub writes a vendor binary that runs a shell script body.
func stub(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestAuthStatusFiles(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(time.Hour)
	ms := func(t time.Time) string { return strconv.FormatInt(t.UnixMilli(), 10) }
	cases := []struct {
		name  string
		h     Harness
		files map[string]string
		want  AuthState
	}{
		{"claude none", Claude, nil, AuthMissing},
		{"claude stub", Claude, map[string]string{".credentials.json": `{"claudeAiOauth":{"expiresAt":` + ms(future) + `}}`}, AuthUnknown},
		{"claude valid", Claude, map[string]string{".credentials.json": `{"claudeAiOauth":{"accessToken":"a","refreshToken":"r","expiresAt":` + ms(future) + `}}`}, AuthUnknown},
		{"claude expired", Claude, map[string]string{".credentials.json": `{"claudeAiOauth":{"accessToken":"a","expiresAt":` + ms(past) + `}}`}, AuthExpired},
		{"claude expired refresh", Claude, map[string]string{".credentials.json": `{"claudeAiOauth":{"accessToken":"a","refreshToken":"r","expiresAt":` + ms(past) + `}}`}, AuthUnknown},
		{"codex tokens", Codex, map[string]string{"auth.json": `{"tokens":{"access_token":"a","refresh_token":"r"}}`}, AuthUnknown},
		{"codex none", Codex, nil, AuthMissing},
		{"grok valid", Grok, map[string]string{"auth.json": `{"b::c":{"expires_at":"` + past.Format(time.RFC3339) + `"},"a::c":{"expires_at":"` + future.Format(time.RFC3339) + `"}}`}, AuthOK},
		{"grok expired", Grok, map[string]string{"auth.json": `{"a::c":{"expires_at":"` + past.Format(time.RFC3339) + `"}}`}, AuthExpired},
		{"grok expired refresh", Grok, map[string]string{"auth.json": `{"a::c":{"expires_at":"` + past.Format(time.RFC3339) + `","refresh_token":"r"}}`}, AuthOK},
		{"grok empty", Grok, map[string]string{"auth.json": `{}`}, AuthUnknown},
		{"pi login", Pi, map[string]string{"auth.json": `{"anthropic":{"type":"oauth"}}`}, AuthUnknown},
		{"pi none", Pi, nil, AuthMissing},
		{"omp db", OMP, map[string]string{"agent.db": "x"}, AuthUnknown},
		{"opencode 1.x", OpenCode, map[string]string{"opencode/auth.json": `{"openai":{"type":"api","key":"k"}}`}, AuthUnknown},
		{"opencode 2.x", OpenCode2, map[string]string{"opencode/opencode.db": "x"}, AuthUnknown},
		{"opencode none", OpenCode, nil, AuthMissing},
		{"gemini expired", Gemini, map[string]string{"oauth_creds.json": `{"access_token":"a","expiry_date":` + ms(past) + `}`}, AuthExpired},
		{"gemini valid", Gemini, map[string]string{"oauth_creds.json": `{"access_token":"a","expiry_date":` + ms(future) + `}`}, AuthUnknown},
		{"copilot users", Copilot, map[string]string{"config.json": `{"loggedInUsers":[{"login":"a"}]}`}, AuthUnknown},
		{"copilot none", Copilot, nil, AuthMissing},
		{"cursor", Cursor, map[string]string{"cli-config.json": `{"authInfo":{"email":"a@b.c"}}`}, AuthUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, user := isolateAuth(t)
			for _, home := range []string{"", filepath.Join(user, "own")} {
				dir := home
				if dir == "" {
					dir = DefaultHome(c.h)
				}
				for name, data := range c.files {
					writeFile(t, dir, name, data)
				}
				got, err := AuthStatus(context.Background(), c.h, home)
				if err != nil || got != c.want {
					t.Errorf("home %q: %s, %v; want %s", home, got, err, c.want)
				}
			}
		})
	}
}

func TestAuthStatusTokenEnv(t *testing.T) {
	_, user := isolateAuth(t)
	t.Setenv("ANTHROPIC_API_KEY", "k")
	if got, _ := AuthStatus(context.Background(), Claude, ""); got != AuthOK {
		t.Errorf("own login: %s, want ok", got)
	}
	// An own login home does not see the env keys of the parent.
	if got, _ := AuthStatus(context.Background(), Claude, filepath.Join(user, "own")); got != AuthMissing {
		t.Errorf("own home: %s, want missing", got)
	}
}

func TestAuthStatusProbe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stubs")
	}
	cases := []struct {
		name  string
		h     Harness
		bin   string
		body  string
		files map[string]string
		want  AuthState
	}{
		{"claude in", Claude, "claude", `[ "$CLAUDE_CONFIG_DIR" = "$WANT_HOME" ] || exit 3; echo '{"loggedIn": true, "email": "a@b.c"}'`, nil, AuthOK},
		{"claude out", Claude, "claude", `echo '{"loggedIn": false}'; exit 1`, nil, AuthMissing},
		{"claude ended", Claude, "claude", `echo '{"loggedIn": false}'; exit 1`,
			map[string]string{".credentials.json": `{"claudeAiOauth":{"accessToken":"a"}}`}, AuthExpired},
		{"codex in", Codex, "codex", `[ "$CODEX_HOME" = "$WANT_HOME" ] || exit 3; echo "Logged in using ChatGPT"`, nil, AuthOK},
		{"codex out", Codex, "codex", `echo "Not logged in"; exit 1`, nil, AuthMissing},
		{"opencode none", OpenCode, "opencode", `[ "$XDG_DATA_HOME" = "$WANT_HOME" ] || exit 3; printf '\033[90m└\033[0m  0 credentials\n'`, nil, AuthMissing},
		{"opencode one", OpenCode, "opencode", `echo "└  1 credentials"`, nil, AuthOK},
		{"opencode2 rows", OpenCode2, "opencode2", `printf 'Provider   Status\nAnthropic  stored\n'`, nil, AuthOK},
		{"cursor in", Cursor, "cursor-agent", `[ "$CURSOR_DATA_DIR" = "$WANT_HOME" ] || exit 3; echo "✓ Logged in as a@b.c"`, nil, AuthOK},
		{"cursor out", Cursor, "cursor-agent", `echo "Not logged in"`, nil, AuthMissing},
		{"omp models", OMP, "omp", `echo '{"models":[{"id":"m"}]}'`, nil, AuthOK},
		{"pi ready", Pi, "pi", `[ "$PI_OFFLINE" = 1 ] || exit 3; [ "$4" = anthropic ] && echo '{"status":"ready"}' || echo '{"status":"missing"}'`, nil, AuthOK},
		{"pi none", Pi, "pi", `echo '{"status":"missing"}'`, nil, AuthMissing},
		{"probe fails", Codex, "codex", `exit 2`,
			map[string]string{"auth.json": `{"tokens":{"access_token":"a"}}`}, AuthUnknown},
		// The first call races the token refresh. The second call sees it.
		{"grok refresh", Grok, "grok", `f="$GROK_HOME/n"; if [ -e "$f" ]; then echo "Logged in"; else : > "$f"; echo "Not authenticated"; fi`,
			map[string]string{"auth.json": `{"a::c":{"expires_at":"2000-01-01T00:00:00Z","refresh_token":"r"}}`}, AuthOK},
		{"grok ended", Grok, "grok", `echo "Not authenticated"`,
			map[string]string{"auth.json": `{"a::c":{"refresh_token":"r"}}`}, AuthExpired},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bin, user := isolateAuth(t)
			home := filepath.Join(user, "own")
			if err := os.MkdirAll(home, 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("WANT_HOME", home)
			stub(t, bin, c.bin, c.body)
			for name, data := range c.files {
				writeFile(t, home, name, data)
			}
			got, err := AuthStatus(context.Background(), c.h, home)
			if err != nil || got != c.want {
				t.Errorf("%s, %v; want %s", got, err, c.want)
			}
		})
	}
}

func TestAuthStatusErrors(t *testing.T) {
	t.Parallel()
	for _, h := range []Harness{Fake, Native, "nope"} {
		if _, err := AuthStatus(context.Background(), h, ""); !errors.Is(err, ErrUnsupported) {
			t.Errorf("%s: %v, want ErrUnsupported", h, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := AuthStatus(ctx, Claude, t.TempDir()); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled: %v", err)
	}
}

func TestParseOpenCodeAuthList(t *testing.T) {
	t.Parallel()
	cases := []struct {
		out  string
		err  error
		want AuthState
		ok   bool
	}{
		{"┌  Credentials ~/.local/share/opencode/auth.json\n│\n●  Anthropic oauth\n│\n└  1 credentials\n", nil, AuthOK, true},
		{"\x1b[90m└\x1b[0m  0 credentials", nil, AuthMissing, true},
		{"Provider     Status\nAnthropic    stored\nOpenAI       stored\n", nil, AuthOK, true},
		{"", nil, AuthMissing, true},
		{"something else", nil, AuthUnknown, false},
		{"1 credentials", errors.New("exit 1"), AuthUnknown, false},
	}
	for _, c := range cases {
		got, _, ok := parseOpenCodeAuthList(c.out, c.err)
		if got != c.want || ok != c.ok {
			t.Errorf("%q: %s %v, want %s %v", c.out, got, ok, c.want, c.ok)
		}
	}
}

func TestCoerceTime(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	cases := []struct {
		in   any
		want time.Time
	}{
		{float64(at.UnixMilli()), at},
		{float64(at.Unix()), at},
		{"2026-01-02T03:04:05Z", at},
		{float64(12), time.Time{}},
		{"soon", time.Time{}},
		{nil, time.Time{}},
	}
	for _, c := range cases {
		if got := coerceTime(c.in); !got.Equal(c.want) {
			t.Errorf("coerceTime(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestLoginCommandFor(t *testing.T) {
	t.Parallel()
	got, err := LoginCommandFor(Claude, LoginOptions{Separate: true})
	if err != nil || strings.Join(got, " ") != "claude setup-token" {
		t.Fatalf("claude separate: %v %v", got, err)
	}
	got, err = LoginCommandFor(OpenCode, LoginOptions{Provider: "openai"})
	if err != nil || got[len(got)-1] != "ChatGPT Pro/Plus (headless)" {
		t.Fatalf("opencode: %v %v", got, err)
	}
	if _, err := LoginCommandFor(OpenCode, LoginOptions{}); err == nil {
		t.Fatal("opencode with no provider: no error")
	}
	if got, _ := LoginCommandFor(Codex, LoginOptions{Separate: true}); strings.Join(got, " ") != "codex login --device-auth" {
		t.Fatalf("codex: %v", got)
	}
	if LoginTokenEnv(Claude) != "CLAUDE_CODE_OAUTH_TOKEN" || LoginTokenEnv(Codex) != "OPENAI_API_KEY" || LoginTokenEnv(OpenCode) != "" {
		t.Fatal("token env keys")
	}
}
