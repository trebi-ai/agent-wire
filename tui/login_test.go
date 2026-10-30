package tui

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseLoginGoldens(t *testing.T) {
	t.Parallel()
	cases := []struct {
		path, h  string
		wantCode string
	}{
		{"claude/login/paste/01-url.txt", "claude", ""},
		{"codex/login/device/01-url.txt", "codex", "ABCD-EFGH"},
	}
	for _, c := range cases {
		data, err := os.ReadFile(filepath.Join("testdata", "screens", c.path))
		if err != nil {
			t.Fatal(err)
		}
		l := ParseLogin(c.h, data)
		if l.URL == "" || l.UserCode != c.wantCode || !l.AwaitCode || l.InApp {
			t.Errorf("%s: %+v", c.path, l)
		}
	}
}

func TestParseLogin(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, h, screen string
		want            Login
	}{
		{"claude url", "claude", "Visit https://claude.ai/oauth/authorize?x=1.\nEnter the authorization code:\n",
			Login{URL: "https://claude.ai/oauth/authorize?x=1", AwaitCode: true}},
		{"codex device", "codex", "To authenticate, visit:\nhttps://auth.openai.com/codex/device\nEnter code: ABCD-EFGH\n",
			Login{URL: "https://auth.openai.com/codex/device", UserCode: "ABCD-EFGH", AwaitCode: true}},
		{"claude token", "claude", "Your token: sk-ant-oat01-abcDEF123\n", Login{Token: "sk-ant-oat01-abcDEF123"}},
		{"codex device off", "codex", "Error: device code request failed with status 403\n",
			Login{Error: "Device code login is off for your ChatGPT account.", Hint: "In ChatGPT, open Settings → Security and turn on device code login."}},
		{"codex error", "codex", "Error logging in with device code: timed out\n", Login{Error: "timed out"}},
		{"token only on claude", "grok", "sk-ant-oat01-abc", Login{}},
		{"pi", "pi", "anything", Login{InApp: true}},
		{"copilot", "copilot", "https://github.com/login/device", Login{InApp: true}},
	}
	for _, c := range cases {
		if got := ParseLogin(c.h, []byte(c.screen)); got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
}
