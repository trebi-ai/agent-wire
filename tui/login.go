package tui

import (
	"regexp"
	"strings"
)

// Login is what a vendor login screen shows.
type Login struct {
	URL       string // the address to open
	UserCode  string // the device code to type on that page
	AwaitCode bool   // the screen asks for a pasted code or token
	Token     string // a long-lived token that the screen prints (Claude setup-token)
	Error     string // the vendor login error
	Hint      string // how to fix Error, when known
	// InApp means that the harness logs in only inside its own TUI. The
	// other fields are then empty.
	InApp bool
}

var (
	loginURL   = regexp.MustCompile(`https://[^\s<>"']+`)
	deviceCode = regexp.MustCompile(`\b([A-Z0-9]{4}-[A-Z0-9]{4,6})\b`)
	pasteAsk   = regexp.MustCompile(`(?i)(paste|enter).{0,40}(code|token|authorization)`)
	claudeOAT  = regexp.MustCompile(`sk-ant-oat01-[A-Za-z0-9_-]+`)
	// The Codex device code request fails with a 4xx status when the
	// ChatGPT account has device code login off.
	codexDeviceOff = regexp.MustCompile(`(?i)device code (login|authorization) is (not enabled|disabled|off)|device code request failed with status 40[0-9]`)
	codexLoginErr  = regexp.MustCompile(`(?i)Error logging in with device code:\s*(.+)`)
)

// ParseLogin reads the screen of a login command of harness h.
func ParseLogin(h string, screen []byte) Login {
	switch h {
	case "pi", "copilot", "omp":
		return Login{InApp: true}
	}
	s := string(screen)
	var l Login
	if m := loginURL.FindString(s); m != "" {
		l.URL = strings.TrimRight(m, ".,);")
	}
	l.UserCode = deviceCode.FindString(s)
	l.AwaitCode = pasteAsk.MatchString(s)
	switch h {
	case "claude":
		l.Token = claudeOAT.FindString(s)
	case "codex":
		switch {
		case codexDeviceOff.MatchString(s):
			l.Error = "Device code login is off for your ChatGPT account."
			l.Hint = "In ChatGPT, open Settings → Security and turn on device code login."
		case codexLoginErr.MatchString(s):
			l.Error = strings.TrimSpace(codexLoginErr.FindStringSubmatch(s)[1])
		}
	}
	return l
}
