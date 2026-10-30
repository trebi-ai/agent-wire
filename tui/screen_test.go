package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// fixtureHarnesses must each have at least one recorded scenario. omp runs
// on the wire and shares the pi rules, so it has no frames.
var fixtureHarnesses = []string{"claude", "codex", "copilot", "cursor", "gemini", "grok", "opencode", "opencode2", "pi"}

// frameName matches "<NN>-<state>.txt".
var frameName = regexp.MustCompile(`^(\d+)-([a-z-]+)\.txt$`)

type frame struct {
	path    string
	index   int
	state   string
	content string
}

// frameStates is the state vocabulary. A midSession frame asserts the
// working or idle state instead of readiness.
var frameStates = map[string]struct{ midSession bool }{
	"boot":               {},
	"trust-folder":       {},
	"bypass-permissions": {},
	"hooks-trust":        {},
	"setup":              {},
	"picker":             {},
	"ready":              {},
	"busy":               {midSession: true},
	"idle":               {midSession: true},
}

// setupActions are the expected answers to a "setup" frame: a first-run
// offer that is neither trust nor bypass.
var setupActions = map[string][]Action{
	"copilot": {{Key: "escape"}},
}

func TestScreenFixtures(t *testing.T) {
	t.Parallel()
	walkFixtures(t, filepath.Join("testdata", "screens"), fixtureHarnesses)
}

const (
	selfTrust  = "Is this a project you created or one you trust?\n  1. Yes, I trust this folder\n  2. No"
	selfBypass = "Bypass Permissions mode\n  1. No\n  2. Yes, I accept"
	selfReady  = "❯ \n  Fable 5 | 0.0 min | 0K (0%) | /tmp/x"
)

func TestFixtureWalkerSelfTest(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeScenario(t, filepath.Join(root, "claude", "startup"), map[string]string{
		"01-boot.txt":               "Welcome to Claude Code!\nLoading...",
		"02-trust-folder.txt":       selfTrust,
		"03-trust-folder.txt":       selfTrust,
		"04-bypass-permissions.txt": selfBypass,
		"05-ready.txt":              selfReady,
		"info.txt":                  "synthetic",
	})
	checked, replayed := walkFixtures(t, root, []string{"claude"})
	if checked != 5 {
		t.Errorf("frames checked = %d, want 5", checked)
	}
	if replayed != 1 {
		t.Errorf("scenarios replayed = %d, want 1", replayed)
	}
}

func TestCheckFrameRejectsBadFrames(t *testing.T) {
	t.Parallel()
	for _, f := range []frame{
		{path: "x/05-ready.txt", state: "ready", content: "splash art"},
		{path: "x/01-boot.txt", state: "boot", content: selfReady},
		{path: "x/02-trust-folder.txt", state: "trust-folder", content: "no dialog here"},
	} {
		if err := checkFrame("claude", f); err == nil {
			t.Errorf("%s: want an error", f.path)
		}
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "01-reddy.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadScenario(dir); err == nil {
		t.Error("an unknown state name must fail")
	}
}

func TestReplayNoReadyFrame(t *testing.T) {
	t.Parallel()
	if err := replay("claude", []frame{{path: "01-boot.txt", state: "boot", content: "Loading..."}}); err != nil {
		t.Errorf("a scenario with no ready frame must not get ready: %v", err)
	}
	if err := replay("claude", []frame{{path: "01-boot.txt", state: "boot", content: selfReady}}); err == nil {
		t.Error("want an error when a boot frame is ready")
	}
}

func TestStarterStuckTrustIsWaiting(t *testing.T) {
	t.Parallel()
	screen := []byte("Is this a project you created or one you trust?\n❯ 1. Yes, I trust this folder\n  2. No, exit\nEnter to confirm")
	st := NewStarter("claude")
	var sent []Action
	for range 5 {
		sc, acts := st.Step(screen)
		if sc.State != StateDialog {
			t.Fatalf("state = %s, want dialog", sc.State)
		}
		sent = append(sent, acts...)
		if len(acts) == 0 {
			if sc.Question == nil || sc.Question.Text == "" {
				t.Fatalf("a stuck dialog needs its question: %+v", sc.Question)
			}
			break
		}
	}
	if len(sent) != MaxDialogTries {
		t.Fatalf("sent %d times, want %d: %+v", len(sent), MaxDialogTries, sent)
	}
	for _, a := range sent {
		if a != (Action{Text: "1"}) {
			t.Fatalf("sent %+v, want Text=1", a)
		}
	}
}

func TestUnknownChoiceIsWaiting(t *testing.T) {
	t.Parallel()
	sc := Classify("claude", []byte("Allow outbound network?\n❯ 1. Yes\n  2. No\nEnter to confirm · Esc to cancel"))
	if sc.State != StateWaiting || len(sc.Actions) != 0 {
		t.Fatalf("got %+v", sc)
	}
	q := sc.Question
	if q == nil || q.Kind != QuestionChoice || q.Text != "Allow outbound network?" {
		t.Fatalf("question = %+v", q)
	}
	if got := q.Items[0].Options; len(got) != 2 || got[0].Label != "Yes" || got[1].Label != "No" {
		t.Fatalf("options = %+v", got)
	}
}

func TestBlockers(t *testing.T) {
	t.Parallel()
	cases := []struct {
		screen string
		want   Blocker
	}{
		{"", ""},
		{"Hello working on task", ""},
		{"Not logged in · Please run /login", BlockerAuth},
		{"  Not logged in · Run /login  ", BlockerAuth},
		{"checking login status in docs", ""},
		{"Login required with prompt=login (MFA flow)", ""},
		{"Authentication required to view bill", ""},
		{"please log in to the portal", ""},
		{"You've hit your session limit · resets 12:20pm", BlockerLimit},
		{"/upgrade to increase your usage limit.", BlockerLimit},
		{"respect the API rate limit when calling", ""},
		{"usage limit on their plan docs", ""},
	}
	for _, c := range cases {
		sc := Classify("claude", []byte(c.screen))
		if sc.Blocker != c.want {
			t.Errorf("%q: blocker = %q, want %q", c.screen, sc.Blocker, c.want)
		}
		if c.want != "" && (sc.State != StateWaiting || sc.Marker == "" || sc.Ready) {
			t.Errorf("%q: got %+v", c.screen, sc)
		}
	}
	// Login and limit chrome keep the composer, so it looks ready without
	// the blocker check.
	for _, s := range []string{
		"Not logged in · Please run /login\n❯ \n  Opus 5 | 0.3 min | 0K (0%) | ~/proj\n",
		"You've hit your session limit · resets 12:20pm (America/Sao_Paulo)\n/upgrade to increase your usage limit.\n❯ \n  Opus 5 | 0.3 min | 0K (0%) | ~/proj\n",
	} {
		if sc := Classify("claude", []byte(s)); sc.Ready || sc.Blocker == "" {
			t.Errorf("%q: got %+v", s, sc)
		}
	}
}

func TestReady(t *testing.T) {
	t.Parallel()
	cases := []struct {
		h, screen string
		want      bool
	}{
		{"claude", "❯ Try \"fix typecheck errors\"\n  Fable 5 | 0.5 min | 0K (0%) | /tmp/x", true},
		{"claude", "❯ \n  ⏵⏵ auto mode on (shift+tab to cycle) · ← for agents", true},
		{"claude", "❯ only", false},
		{"claude", "Fable 5 | 0.5 min | 0K (0%) without glyph", false},
		{"claude", "❯ 1. Yes, I trust this folder\n  2. No, exit", false},
		{"codex", "› Implement {feature}\n\n  gpt-5.5 medium · /tmp/x", true},
		{"codex", "Do you trust the contents of this directory? …\n› 1. Yes, continue\n  Press enter to continue", false},
		{"codex", "loading...", false},
		{"copilot", "❯\n────\n v1.0.71 available · run /update · / commands · ? help", true},
		{"copilot", "╻▄▄▄\n┃\n╹▀▀▀\n / commands · ? help · → next tab", true},
		{"copilot", "Do you trust the files in this folder?\n❯ 1. Yes\n  2. Yes, and remember this folder", false},
		{"copilot", "Would you like to add this key binding to your terminal configuration?\n┃ ❯ 1. Yes", false},
		{"copilot", ">", false},
		{"gemini", "│ >   Type your message or @path/to/file │\n /tmp/x  no sandbox  auto", true},
		{"gemini", "│ ⠹ Waiting for auth... (Press ESC or CTRL+C to cancel) │", false},
		{"gemini", "gemini >", false},
		{"opencode", "┃  Ask anything... \"Fix broken tests\"\n┃  Build · Grok 4.3 xAI", true},
		{"opencode", "something > here", false},
		{"opencode", "┃ typed text, placeholder gone", false},
		{"grok", "│ ❯ \n Grok 4.5 (high) · always-approve", true},
		{"grok", "┌─ Resume session ─┐\n│ ❯", false},
		{"grok", "idle spinner", false},
		{"unknown", "❯ \n  Fable 5 | 0.0 min", false},
	}
	for _, c := range cases {
		if got := Classify(c.h, []byte(c.screen)).Ready; got != c.want {
			t.Errorf("%s %q: ready = %v, want %v", c.h, c.screen, got, c.want)
		}
	}
}

func TestClaudeStartupActions(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, screen string
		want         []Action
	}{
		{"trust", "Is this a project you created or one you trust?\n❯ 1. Yes, I trust this folder\n  2. No, exit", []Action{{Text: "1"}}},
		{"trust cursor on no", "Is this a project you created or one you trust?\n  1. Yes, I trust this folder\n❯ 2. No, exit\nEnter to confirm", []Action{{Text: "1"}}},
		{"bypass", "Bypass Permissions mode\nYes, I accept", []Action{{Text: "2"}}},
		{"resume summary", "This session is 3h 3m old and 235k tokens.\n❯ 1. Resume from summary (recommended)\n  2. Resume full session as-is\n  3. Don't ask me again", []Action{{Text: "2"}}},
		{"theme", "Let's get started.\n\n Choose the text style that looks best with your terminal\n To change this later, run /theme\n\n   1. Auto (match terminal)\n ❯ 2. Dark mode ✔\n   3. Light mode\n", []Action{{Key: "enter"}}},
		{"chrome", "Claude in Chrome extension detected\n\nClaude will use your Chrome browser by default\n\n  ❯ 1. Yes, use my browser\n    2. No, keep browser tools off\n\n  Enter to confirm · Esc to keep browser tools off\n", []Action{{Text: "2"}}},
		{"normal", "normal screen", nil},
	}
	for _, c := range cases {
		sc := Classify("claude", []byte(c.screen))
		if !equalActions(sc.Actions, c.want) {
			t.Errorf("%s: actions = %+v, want %+v", c.name, sc.Actions, c.want)
		}
		if sc.Ready {
			t.Errorf("%s: ready, want not ready", c.name)
		}
		if c.want != nil && sc.State != StateDialog {
			t.Errorf("%s: state = %s, want dialog", c.name, sc.State)
		}
	}
}

func TestClaudeGoldenDialogs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		path string
		want []Action
	}{
		{"claude/startup-trust-no-default/01-trust-folder.txt", []Action{{Key: "down"}, {Key: "enter"}}},
		{"claude/resume-summary-prompt.txt", []Action{{Text: "2"}}},
	}
	for _, c := range cases {
		data, err := os.ReadFile(filepath.Join("testdata", "screens", c.path))
		if err != nil {
			t.Fatal(err)
		}
		sc := Classify("claude", data)
		if sc.Ready || !equalActions(sc.Actions, c.want) {
			t.Errorf("%s: ready=%v actions=%+v, want %+v", c.path, sc.Ready, sc.Actions, c.want)
		}
	}
}

// Chrome text in the body, above the bottom chrome, must not set a state.
func TestChromeSpoofResistance(t *testing.T) {
	t.Parallel()
	body := strings.Repeat("model output quoting chrome: esc to interrupt "+
		"Esc to cancel esc to cancel Type your message ? help ›  min | \n", 5)
	screen := body + strings.Repeat("plain output line with no markers\n", chromeLines+2)
	for _, h := range []string{"claude", "codex", "copilot", "gemini", "cursor", "grok", "pi"} {
		if got := Classify(h, []byte(screen)).State; got != StateUnknown {
			t.Errorf("%s: state = %s, want unknown", h, got)
		}
	}
}

// The terminal pads the pane with blank rows. They must not push the
// chrome out of the read part.
func TestChromeTrailingBlanks(t *testing.T) {
	t.Parallel()
	screen := "filler\n• Working (5s • esc to interrupt)\n› \n" + strings.Repeat("\n", 30)
	if got := Classify("codex", []byte(screen)).State; got != StateWorking {
		t.Errorf("state = %s, want working", got)
	}
}

// The composer stays visible while a turn runs, so busy wins over idle.
func TestChromeBusyWinsOverIdle(t *testing.T) {
	t.Parallel()
	cases := []struct{ h, screen string }{
		{"codex", "• Working (0s • esc to interrupt)\n\n› Implement {feature}\n footer"},
		{"copilot", "◎ Thinking (Esc to cancel)\n❯\n / commands · ? help"},
		{"gemini", "⠧ waiting (esc to cancel, 0s)\n│ >   Type your message or @path/to/file │\n footer"},
		{"claude", "✳ Deliberating… (esc to interrupt)\n❯ \n  Fable 5 | 2.0 min | 10K (5%) | /tmp/x"},
	}
	for _, c := range cases {
		if got := Classify(c.h, []byte(c.screen)).State; got != StateWorking {
			t.Errorf("%s: state = %s, want working", c.h, got)
		}
	}
}

// checkFrame asserts the readiness and the startup actions of one frame.
func checkFrame(h string, f frame) error {
	sc := Classify(h, []byte(f.content))
	if f.state != "ready" && sc.Ready {
		return fmt.Errorf("%s: %s frame is ready", f.path, f.state)
	}
	switch f.state {
	case "ready":
		if !sc.Ready || len(sc.Actions) != 0 {
			return fmt.Errorf("%s: ready=%v actions=%+v, want ready and no actions", f.path, sc.Ready, sc.Actions)
		}
	case "boot", "picker":
		if len(sc.Actions) != 0 {
			return fmt.Errorf("%s: %s frame has actions %+v", f.path, f.state, sc.Actions)
		}
	case "trust-folder":
		if !trustYes(sc.Actions) {
			return fmt.Errorf("%s: trust actions = %+v", f.path, sc.Actions)
		}
	case "bypass-permissions":
		if !equalActions(sc.Actions, []Action{{Text: "2"}}) {
			return fmt.Errorf("%s: bypass actions = %+v, want [{Text:2}]", f.path, sc.Actions)
		}
	case "setup":
		want, ok := setupActions[h]
		if !ok {
			return fmt.Errorf("%s: setup frame but no setupActions entry for %s", f.path, h)
		}
		if !equalActions(sc.Actions, want) {
			return fmt.Errorf("%s: setup actions = %+v, want %+v", f.path, sc.Actions, want)
		}
	}
	return nil
}

// checkMidSession asserts the state of a busy or idle frame.
func checkMidSession(h string, f frame) error {
	sc := Classify(h, []byte(f.content))
	if len(sc.Actions) != 0 {
		return fmt.Errorf("%s: mid-session frame has actions %+v", f.path, sc.Actions)
	}
	want := map[string]State{"busy": StateWorking, "idle": StateIdle}[f.state]
	if sc.State != want {
		return fmt.Errorf("%s: state = %s, want %s", f.path, sc.State, want)
	}
	return nil
}

func trustYes(acts []Action) bool {
	if len(acts) == 2 && (acts[0].Key == "down" || acts[0].Key == "up") && acts[1].Key == "enter" {
		return true
	}
	return len(acts) == 1 && (acts[0] == Action{Text: "1"} || acts[0] == Action{Key: "enter"})
}

func equalActions(a, b []Action) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// replay feeds the startup frames through a Starter, the way a launcher
// polls the pane: each read gives the next frame, and the last frame
// repeats. The loop must stop on the first ready frame, or never get ready
// when the scenario has none. Each dialog gets its answer at most
// MaxDialogTries times.
func replay(h string, frames []frame) error {
	readyIdx := -1
	for i, f := range frames {
		if f.state == "ready" {
			readyIdx = i
			break
		}
	}
	st := NewStarter(h)
	reads, sent, ready := 0, 0, false
	for range len(frames) + 20 {
		i := min(reads, len(frames)-1)
		reads++
		sc, acts := st.Step([]byte(frames[i].content))
		if sc.Blocker != "" {
			return fmt.Errorf("%s: blocker %s", frames[i].path, sc.Blocker)
		}
		sent += len(acts)
		if len(acts) == 0 && (sc.State == StateDialog || sc.State == StateWaiting) {
			break
		}
		if sc.Ready {
			ready = true
			break
		}
	}
	if readyIdx < 0 {
		if ready {
			return fmt.Errorf("the scenario has no ready frame, but the loop got ready")
		}
		return nil
	}
	if !ready {
		return fmt.Errorf("the loop never got ready")
	}
	if reads != readyIdx+1 {
		return fmt.Errorf("the loop stopped after %d reads, want %d (%s)", reads, readyIdx+1, frames[readyIdx].path)
	}
	want := map[string]int{}
	total := 0
	for _, f := range frames[:readyIdx+1] {
		for _, act := range Classify(h, []byte(f.content)).Actions {
			k := DialogKey(f.content, act)
			if want[k] < MaxDialogTries {
				want[k]++
				total++
			}
		}
	}
	if sent != total {
		return fmt.Errorf("sent %d actions, want %d", sent, total)
	}
	return nil
}

func loadScenario(dir string) ([]frame, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var frames []frame
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || name[0] == '.' || name == "info.txt" {
			continue
		}
		full := filepath.Join(dir, name)
		m := frameName.FindStringSubmatch(name)
		if m == nil {
			return nil, fmt.Errorf("%s: name must be <NN>-<state>.txt", full)
		}
		if _, ok := frameStates[m[2]]; !ok {
			return nil, fmt.Errorf("%s: unknown state %q", full, m[2])
		}
		idx, _ := strconv.Atoi(m[1])
		content, err := os.ReadFile(full)
		if err != nil {
			return nil, err
		}
		frames = append(frames, frame{path: full, index: idx, state: m[2], content: string(content)})
	}
	if len(frames) == 0 {
		return nil, fmt.Errorf("%s: no frames", dir)
	}
	sort.Slice(frames, func(i, j int) bool { return frames[i].index < frames[j].index })
	return frames, nil
}

func scenarioDirs(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && e.Name()[0] != '_' && e.Name()[0] != '.' && e.Name() != "login" {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(out)
	return out
}

// walkFixtures checks every frame and replays every scenario under root.
// It returns the counts, so the self-test can prove that the checks ran.
func walkFixtures(t *testing.T, root string, harnesses []string) (checked, replayed int) {
	t.Helper()
	for _, h := range harnesses {
		dirs := scenarioDirs(filepath.Join(root, h))
		if len(dirs) == 0 {
			t.Errorf("%s: no screen fixtures under %s", h, root)
			continue
		}
		for _, dir := range dirs {
			frames, err := loadScenario(dir)
			if err != nil {
				t.Error(err)
				continue
			}
			var pre []frame
			for _, f := range frames {
				var err error
				if frameStates[f.state].midSession {
					err = checkMidSession(h, f)
				} else {
					err = checkFrame(h, f)
					pre = append(pre, f)
				}
				if err != nil {
					t.Error(err)
				}
				checked++
			}
			if len(pre) > 0 {
				if err := replay(h, pre); err != nil {
					t.Errorf("%s: replay: %v", dir, err)
				}
				replayed++
			}
		}
	}
	return checked, replayed
}

func writeScenario(t *testing.T, dir string, frames map[string]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range frames {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
