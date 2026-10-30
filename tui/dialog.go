package tui

import (
	"regexp"
	"strconv"
	"strings"
)

// MaxDialogTries is how many times a Starter sends the same answer to one
// dialog. After that the dialog counts as stuck, and the user must answer.
const MaxDialogTries = 2

// TrustAction picks option 1 of a numbered dialog, else Enter (an arrow list
// whose first row is already selected).
func TrustAction(screen string) Action {
	acts := TrustYesActions(screen)
	if len(acts) == 1 {
		return acts[0]
	}
	return Action{Key: "enter"}
}

// TrustYesActions accept a folder trust dialog. A numbered list gets "1". An
// unnumbered list moves the cursor onto the trust row, then presses Enter.
func TrustYesActions(screen string) []Action {
	if option1Line.MatchString(screen) {
		return []Action{{Text: "1"}}
	}
	lines := strings.Split(screen, "\n")
	cursor, yes := -1, -1
	for i, line := range lines {
		if strings.Contains(line, "❯") || strings.Contains(line, "›") {
			cursor = i
		}
		low := strings.ToLower(line)
		if strings.Contains(low, "yes") && strings.Contains(low, "trust") {
			yes = i
		}
	}
	switch {
	case cursor < 0 || yes < 0 || cursor == yes:
		return []Action{{Key: "enter"}}
	case yes > cursor:
		return []Action{{Key: "down"}, {Key: "enter"}}
	}
	return []Action{{Key: "up"}, {Key: "enter"}}
}

// ChoiceDialog reports a numbered choice dialog with confirm chrome, and
// returns its question. An idle composer or a "?" in the transcript never
// matches.
func ChoiceDialog(screen string) (question string, ok bool) {
	if screen == "" || !option1Line.MatchString(screen) {
		return "", false
	}
	low := strings.ToLower(flat(screen))
	for _, p := range []string{"enter to confirm", "press enter to continue", "enter to select", "enter select"} {
		if strings.Contains(low, p) {
			return choiceText(screen), true
		}
	}
	return "", false
}

// DialogKey names a dialog and one answer. A cursor move does not change the
// key, so a redraw with the cursor on another row is the same dialog.
func DialogKey(screen string, act Action) string {
	n := strings.NewReplacer("❯", " ", "›", " ", "→", " ", "●", " ").Replace(screen)
	n = flat(n)
	if len(n) > 160 {
		n = n[:160]
	}
	return act.Key + "\x00" + act.Text + "\x00" + n
}

// Starter answers the startup dialogs of one session. It sends the same
// answer to one dialog at most MaxDialogTries times. It is not safe for
// concurrent use.
type Starter struct {
	harness string
	tries   map[string]int
}

// NewStarter returns a Starter for harness h.
func NewStarter(h string) *Starter {
	return &Starter{harness: h, tries: map[string]int{}}
}

// Step classifies one screen and returns the actions to send now. When the
// screen is a dialog and no action is left to send, the dialog is stuck: the
// caller treats the session as waiting.
func (s *Starter) Step(screen []byte) (Screen, []Action) {
	sc := Classify(s.harness, screen)
	var send []Action
	for _, act := range sc.Actions {
		key := DialogKey(string(screen), act)
		if s.tries[key] >= MaxDialogTries {
			continue
		}
		s.tries[key]++
		send = append(send, act)
	}
	return sc, send
}

var (
	option1Line = regexp.MustCompile(`(?m)^\s*[❯›●→ ]*1\.`)
	optionRow   = regexp.MustCompile(`^\s*[│┃]?\s*[❯›●→ ]*(\d+)\.\s+(.+)$`)
)

// choiceText is the last row with a "?" above option 1, else the last text
// row above it.
func choiceText(screen string) string {
	var buf []string
	for _, line := range strings.Split(screen, "\n") {
		if option1Line.MatchString(line) {
			break
		}
		t := strings.Trim(strings.TrimSpace(line), "│╭╮╰╯─ ")
		if t == "" || strings.HasPrefix(t, "─") {
			continue
		}
		buf = append(buf, t)
	}
	for i := len(buf) - 1; i >= 0; i-- {
		if strings.Contains(buf[i], "?") {
			return buf[i]
		}
	}
	if n := len(buf); n > 0 {
		return buf[n-1]
	}
	return ""
}

// choiceQuestion reads a numbered dialog as one single choice item. It
// returns nil when the screen has no numbered options.
func choiceQuestion(screen string) *Question {
	if !option1Line.MatchString(screen) {
		return nil
	}
	item := Item{ID: "q1", Text: choiceText(screen), Kind: ItemSingle, Options: []Option{}}
	next := 1
	for _, line := range strings.Split(screen, "\n") {
		m := optionRow.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if n, _ := strconv.Atoi(m[1]); n != next {
			continue
		}
		next++
		label := strings.TrimSpace(strings.TrimRight(m[2], "│┃ "))
		item.Options = append(item.Options, Option{Label: label})
	}
	if item.Text == "" && len(item.Options) == 0 {
		return nil
	}
	return &Question{Kind: QuestionChoice, Text: item.Text, Items: []Item{item}}
}
