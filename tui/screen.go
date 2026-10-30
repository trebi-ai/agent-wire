// Package tui reads the screen of a vendor TUI that runs in a terminal. It
// tells if the agent works, waits, or is idle, which startup dialog is open
// and the keys that answer it, and which question the agent asks.
//
// The rules come from recorded frames in testdata. Each rule looks for
// strong chrome text only, so model output on the screen does not change
// the result.
package tui

import (
	"strings"
)

// State is the coarse state of a TUI screen.
type State string

const (
	// StateUnknown means that no rule matched, for example a boot splash.
	StateUnknown State = "unknown"
	// StateWorking means that a turn runs.
	StateWorking State = "working"
	// StateIdle means that the composer shows and no turn runs.
	StateIdle State = "idle"
	// StateDialog means that a known startup dialog blocks the input.
	// Screen.Actions answers it.
	StateDialog State = "dialog"
	// StateWaiting means that the agent waits for the user: a question, a
	// permission prompt, an unknown choice dialog, or a blocker.
	StateWaiting State = "waiting"
)

// Blocker is chrome that stops all work until the user acts outside the TUI.
type Blocker string

const (
	// BlockerAuth is a login prompt.
	BlockerAuth Blocker = "auth"
	// BlockerLimit is a usage or session limit.
	BlockerLimit Blocker = "limit"
)

// Action is one input to the TUI: a named key, literal text, or both.
type Action struct {
	Key  string // a named key, such as "enter", "escape", "down"
	Text string // literal text
}

// Screen is the result of Classify.
type Screen struct {
	State State
	// Ready reports that the composer shows and takes typed text. A working
	// screen can also be ready.
	Ready bool
	// Actions answer the open startup dialog. Set only for StateDialog.
	Actions []Action
	// Blocker is set when login or limit chrome shows. State is then
	// StateWaiting.
	Blocker Blocker
	// Marker is the blocker text that matched.
	Marker string
	// Question is the open question, permission prompt, or choice dialog.
	Question *Question
}

// Classify reads one screen of harness h. The harness names are the
// agent-wire names: "claude", "codex", "copilot", "cursor", "gemini",
// "grok", "opencode", "opencode2", "pi", "omp". An unknown harness gets the
// generic rules only.
func Classify(h string, screen []byte) Screen {
	s := string(screen)
	out := Screen{State: StateUnknown}
	if m := match(s, authMarkers); m != "" {
		out.State, out.Blocker, out.Marker = StateWaiting, BlockerAuth, m
		return out
	}
	if m := match(s, limitMarkers); m != "" {
		out.State, out.Blocker, out.Marker = StateWaiting, BlockerLimit, m
		return out
	}
	r := rulesFor(h)
	out.Ready = r.ready != nil && r.ready(s)
	if r.startup != nil {
		if acts := r.startup(s); len(acts) > 0 {
			out.State, out.Actions = StateDialog, acts
			out.Question = choiceQuestion(s)
			return out
		}
	}
	if h == "claude" {
		rows := strings.Split(s, "\n")
		if f, ok := ParseForm(rows); ok {
			out.State, out.Question = StateWaiting, f.question()
			return out
		}
		if p, ok := ParsePrompt(rows); ok {
			out.State, out.Question = StateWaiting, p.question()
			return out
		}
	}
	// A dialog takes the place of the composer. With the composer on the
	// screen, numbered rows are model output.
	if _, ok := ChoiceDialog(s); ok && !out.Ready {
		out.State, out.Question = StateWaiting, choiceQuestion(s)
		return out
	}
	out.State = classifyChrome(s, r.busy, r.idle)
	return out
}

// rules are the screen facts of one harness.
type rules struct {
	ready   func(string) bool
	startup func(string) []Action
	busy    []string // chrome markers of a running turn
	idle    []string // chrome markers of an idle composer
}

func rulesFor(h string) rules {
	switch h {
	case "claude":
		// Older CLIs show "esc to interrupt". Newer ones show an active
		// "<spinner> <Verb>… (Ns · …)" line; the finished line has no "… (".
		return rules{ready: claudeReady, startup: claudeStartup,
			busy: []string{"esc to interrupt", "… ("}, idle: []string{" min | ", "auto mode on"}}
	case "codex":
		return rules{ready: codexReady, startup: codexStartup,
			busy: []string{"esc to interrupt"}, idle: []string{"› "}}
	case "copilot":
		return rules{ready: copilotReady, startup: copilotStartup,
			busy: []string{"Esc to cancel"}, idle: []string{"? help"}}
	case "gemini":
		return rules{ready: geminiReady, startup: geminiStartup,
			busy: []string{"esc to cancel"}, idle: []string{"Type your message"}}
	case "cursor":
		return rules{ready: cursorReady, startup: cursorStartup,
			busy: []string{"ctrl+c to stop", "Working", "Generating", "Thinking", "Esc to cancel"},
			idle: []string{"→ Plan, search, build anything", "→ Add a follow-up"}}
	case "grok":
		return rules{ready: grokReady, startup: grokStartup,
			busy: []string{"Thinking", "Running", "esc to interrupt"}, idle: []string{"│ ❯"}}
	case "opencode", "opencode2":
		// The "┃" gutter with the idle placeholder is a focused, empty input.
		return rules{ready: func(s string) bool {
			return strings.Contains(s, "┃") && strings.Contains(s, "Ask anything")
		}}
	case "pi", "omp":
		return rules{ready: piReady, startup: piStartup,
			busy: []string{"Working...", "Working"}, idle: []string{"•"}}
	}
	return rules{}
}

func claudeReady(s string) bool {
	if strings.Contains(s, "Yes, I trust this folder") || strings.Contains(s, "Bypass Permissions mode") {
		return false
	}
	if !strings.Contains(s, "❯") {
		return false
	}
	return strings.Contains(s, " min | ") || strings.Contains(s, "auto mode on")
}

func claudeStartup(s string) []Action {
	var acts []Action
	// The first-run theme picker preselects a theme. Enter accepts it.
	if strings.Contains(s, "Choose the text style that looks best") ||
		(strings.Contains(s, "Let's get started") && strings.Contains(s, "Dark mode") && strings.Contains(s, "Light mode")) {
		acts = append(acts, Action{Key: "enter"})
	}
	if strings.Contains(s, "Is this a project you created or one you trust?") &&
		strings.Contains(s, "Yes, I trust this folder") {
		acts = append(acts, TrustYesActions(s)...)
	}
	// Option 2 accepts bypass permissions mode.
	if strings.Contains(s, "Bypass Permissions mode") && strings.Contains(s, "Yes, I accept") {
		acts = append(acts, Action{Text: "2"})
	}
	// Option 2 resumes the full session, not a summary.
	if strings.Contains(s, "Resume full session as-is") && strings.Contains(s, "Resume from summary") {
		acts = append(acts, Action{Text: "2"})
	}
	// Option 2 keeps the browser tools off.
	if strings.Contains(s, "Claude in Chrome extension detected") ||
		(strings.Contains(s, "use my browser") && strings.Contains(s, "keep browser tools off")) {
		acts = append(acts, Action{Text: "2"})
	}
	return acts
}

// codexTrust marks the folder trust dialog. Its text starts with "> You are
// in ...", so check it before the composer glyph.
const codexTrust = "Do you trust the contents of this directory"

func codexReady(s string) bool {
	// "› " also shows while "model: loading"; the TUI buffers typed text.
	return !strings.Contains(s, codexTrust) && strings.Contains(s, "› ")
}

func codexStartup(s string) []Action {
	if strings.Contains(s, codexTrust) {
		return []Action{{Text: "1"}}
	}
	return nil
}

func copilotReady(s string) bool {
	// The composer is "❯" up to 1.0.27 and a "┃" gutter from 1.0.71. Startup
	// dialogs show a cursor but never the "? help" footer.
	if !strings.Contains(s, "? help") {
		return false
	}
	return strings.Contains(s, "❯") || strings.Contains(s, "┃")
}

func copilotStartup(s string) []Action {
	if strings.Contains(s, "Do you trust the files in this folder") {
		return []Action{{Text: "1"}}
	}
	// Decline the offer to write a key binding into the terminal config.
	if strings.Contains(s, "Would you like to add this key binding to your terminal configuration?") {
		return []Action{{Key: "escape"}}
	}
	return nil
}

func geminiReady(s string) bool {
	return strings.Contains(s, "Type your message or @path/to/file")
}

func geminiStartup(s string) []Action {
	// Both trust dialogs preselect option 1.
	if strings.Contains(s, "Do you trust the") {
		return []Action{{Text: "1"}}
	}
	return nil
}

func cursorReady(s string) bool {
	if strings.Contains(s, "→ Plan, search, build anything") || strings.Contains(s, "→ Add a follow-up") {
		return true
	}
	// Typed text hides the placeholder: header, input arrow, and footer.
	return strings.Contains(s, "Cursor Agent") && strings.Contains(s, "→ ") &&
		(strings.Contains(s, "Run Everything") || strings.Contains(s, "Auto"))
}

func cursorStartup(s string) []Action {
	// The launch command line can show "--trust", so match the dialog text.
	low := strings.ToLower(s)
	if strings.Contains(low, "do you trust") || strings.Contains(low, "trust this workspace") ||
		strings.Contains(low, "trust this folder") {
		return []Action{TrustAction(s)}
	}
	return nil
}

func grokReady(s string) bool {
	// The resume picker still shows the input edge. Never type into it.
	if strings.Contains(s, "─ Resume session ─") {
		return false
	}
	return strings.Contains(s, "│ ❯")
}

func grokStartup(s string) []Action {
	if strings.Contains(s, "trust") && strings.Contains(s, "Yes") {
		return []Action{{Key: "enter"}}
	}
	return nil
}

func piReady(s string) bool {
	if strings.Contains(s, "Trust project folder?") {
		return false
	}
	// The footer has the cost or usage and "model • thinking level".
	if !strings.Contains(s, "•") {
		return false
	}
	return strings.Contains(s, "$") || strings.Contains(s, "%/")
}

func piStartup(s string) []Action {
	if strings.Contains(s, "Trust project folder?") {
		return []Action{TrustAction(s)}
	}
	return nil
}

// authMarkers are login chrome. Vague phrases such as "login required" are
// left out, because agents write them about web sites.
var authMarkers = []string{
	"please run /login",
	"not logged in",
	"run /login",
}

// limitMarkers are usage and session limit chrome. Vague "rate limit" prose
// is left out.
var limitMarkers = []string{
	"you've hit your session limit",
	"hit your session limit",
	"/upgrade to increase your usage limit",
	"to increase your usage limit",
	"you've hit your usage limit",
	"hit your usage limit",
	"you've hit your limit",
	"usage limit exceeded",
	"rate limit exceeded",
	"quota exceeded",
	"quota exhausted",
	"out of credits",
	"insufficient credits",
	"you've run out of credits",
	"monthly usage limit",
	"you've reached your limit",
	"limit reached — try again",
	"limit reached - try again",
}

// match returns the first marker in the flattened, lower case screen.
func match(screen string, markers []string) string {
	if screen == "" {
		return ""
	}
	low := strings.ToLower(flat(screen))
	for _, m := range markers {
		if strings.Contains(low, m) {
			return m
		}
	}
	return ""
}

// chromeLines is the size of the bottom chrome (composer and status bar).
// Only this part is read, so model output above it cannot spoof a state.
const chromeLines = 10

func bottomChrome(screen string) string {
	lines := strings.Split(screen, "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > chromeLines {
		lines = lines[len(lines)-chromeLines:]
	}
	return strings.Join(lines, "\n")
}

// classifyChrome checks the busy markers first: the composer stays visible
// while a turn runs.
func classifyChrome(screen string, busy, idle []string) State {
	chrome := bottomChrome(screen)
	for _, m := range busy {
		if strings.Contains(chrome, m) {
			return StateWorking
		}
	}
	for _, m := range idle {
		if strings.Contains(chrome, m) {
			return StateIdle
		}
	}
	return StateUnknown
}
