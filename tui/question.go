package tui

import (
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// QuestionKind tells where a Question comes from.
type QuestionKind string

const (
	// QuestionForm is a Claude AskUserQuestion form. Question.Form has it.
	QuestionForm QuestionKind = "form"
	// QuestionPermission is a Claude tool permission prompt. Question.Prompt
	// has it.
	QuestionPermission QuestionKind = "permission"
	// QuestionPlan is the Claude plan approval. Question.Prompt has it.
	QuestionPlan QuestionKind = "plan"
	// QuestionChoice is any other numbered choice dialog.
	QuestionChoice QuestionKind = "choice"
)

// Question is the open question of a screen.
type Question struct {
	Kind QuestionKind
	// Text is the question row.
	Text string
	// Items are the questions to answer. A form page and a prompt have one
	// item; the review page of a form has one item for each answer.
	Items  []Item
	Form   *Form
	Prompt *Prompt
}

// ItemKind is the answer shape of an Item.
type ItemKind string

const (
	ItemSingle ItemKind = "single"
	ItemMulti  ItemKind = "multi"
	ItemFree   ItemKind = "free"
)

// Item is one question with its choices.
type Item struct {
	ID          string   `json:"id"`
	Header      string   `json:"header,omitempty"`
	Text        string   `json:"text"`
	Why         string   `json:"why,omitempty"`
	Kind        ItemKind `json:"kind"`
	Options     []Option `json:"options"`
	AllowsOther bool     `json:"allowsOther"`
}

// Option is one choice of an Item.
type Option struct {
	Label  string `json:"label"`
	Detail string `json:"detail,omitempty"`
}

// Item ids of the questions that Classify makes from one screen.
const (
	// ReviewID is the id of the item that submits or cancels the review page
	// of a form.
	ReviewID = "review"
	// ItemPermission is the id of the item of a permission prompt.
	ItemPermission = "permission"
	// ItemPlan is the id of the item of a plan approval.
	ItemPlan = "plan"
)

// Form is the AskUserQuestion form of Claude Code, read from the screen as a
// best effort.
type Form struct {
	Headers []string // the tab bar: one header for each question
	Items   []Item
	Review  bool
	Cursor  string       // the row under the cursor, with no marker: "4. [✔] perf", "Next"
	Boxes   map[int]bool // the box of each numbered option of a multi question: true when it is on
}

// Key names the form when the transcript does not give its id.
func (f Form) Key() string {
	if len(f.Headers) > 0 {
		return strings.Join(f.Headers, "|")
	}
	if len(f.Items) > 0 {
		return f.Items[0].Text
	}
	return ""
}

// Page names this page of the form. A form from the screen is answered one
// page at a time.
func (f Form) Page() string {
	if f.Review {
		return f.Key() + "\n" + ReviewID
	}
	if len(f.Items) > 0 {
		return f.Key() + "\n" + f.Items[0].Text
	}
	return f.Key() + "\n"
}

// ReviewItem is the review page as one choice.
func (f Form) ReviewItem() Item {
	why := make([]string, len(f.Items))
	for i, it := range f.Items {
		why[i] = it.Text + " → " + it.Why
	}
	return Item{ID: ReviewID, Header: "Review", Text: "Ready to submit your answers?", Why: strings.Join(why, "\n"),
		Kind: ItemSingle, Options: []Option{{Label: "Submit answers"}, {Label: "Cancel"}}}
}

func (f Form) question() *Question {
	q := &Question{Kind: QuestionForm, Items: f.Items, Form: &f}
	if f.Review {
		q.Text = "Ready to submit your answers?"
		q.Items = []Item{f.ReviewItem()}
	} else if len(f.Items) > 0 {
		q.Text = f.Items[0].Text
	}
	return q
}

// ParseForm returns false when no form is open at the bottom of the screen.
func ParseForm(screen []string) (Form, bool) {
	rows := lines(screen)
	var filled []string
	for _, r := range rows {
		if t := trim(r); t != "" {
			filled = append(filled, t)
		}
	}
	if len(filled) == 0 {
		return Form{}, false
	}
	last := filled[len(filled)-1]
	review := false
	for _, t := range filled[max(0, len(filled)-4):] {
		review = review || strings.Contains(t, "Ready to submit your answers?")
	}
	if !review && !(strings.Contains(last, "Enter to select") && strings.Contains(last, "Esc to cancel")) {
		return Form{}, false
	}

	// The form starts below the second to last rule. "Chat about this" is
	// below the last rule.
	var rules []int
	for i, r := range rows {
		if separator(r) {
			rules = append(rules, i)
		}
	}
	top := -1
	if len(rules) >= 2 {
		top = rules[len(rules)-2]
	} else if len(rules) == 1 {
		top = rules[0]
	}
	bar := -1
	for i := len(rows) - 1; i >= 0; i-- {
		if t := trim(rows[i]); strings.HasPrefix(t, "←") && strings.Contains(t, "Submit") {
			bar = i
			break
		}
	}
	if bar < 0 && top >= 0 && top+1 < len(rows) && boxed(trim(rows[top+1])) {
		bar = top + 1
	}
	headers := []string{}
	if bar >= 0 {
		for _, h := range strings.Split(rows[bar], "  ") {
			h = trim(h)
			if boxed(h) {
				h = dropRunes(h, 2)
			}
			if h != "" && h != "←" && h != "→" && h != "Submit" {
				headers = append(headers, h)
			}
		}
	}
	start := 0
	if bar >= 0 {
		start = bar + 1
	} else if top >= 0 {
		start = top + 1
	}
	firstOption := -1
	cursor := ""
	for i := start; i < len(rows); i++ {
		if _, _, ok := option(rows[i]); ok && firstOption < 0 {
			firstOption = i
		}
		if t := trim(rows[i]); strings.HasPrefix(t, "❯") {
			cursor = trim(dropRunes(t, 1))
		}
	}
	if review {
		return Form{Headers: headers, Items: reviewItems(rows[start:]), Review: true, Cursor: cursor}, true
	}
	if firstOption < 0 {
		return Form{Headers: headers, Items: []Item{}, Cursor: cursor}, true
	}

	var text []string
	for _, r := range rows[start:firstOption] {
		t := trim(r)
		if strings.HasPrefix(t, "│") {
			t = trim(strings.TrimPrefix(t, "│"))
		}
		if t != "" {
			text = append(text, t)
		}
	}
	type opt struct {
		label  string
		detail []string
	}
	var options []opt
	multi, other, keep := false, false, false
	boxes := map[int]bool{}
	for _, r := range rows[firstOption:] {
		if separator(r) || strings.HasPrefix(trim(r), "Enter to select") {
			break
		}
		if n, label, ok := option(r); ok {
			for _, box := range []string{"[ ] ", "[✔] ", "[x] ", "[×] "} {
				if strings.HasPrefix(label, box) {
					multi = true
					boxes[n] = box != "[ ] "
					label = strings.TrimPrefix(label, box)
				}
			}
			keep = false
			if strings.HasPrefix(label, "Type something") {
				other = true
			} else if label != "Chat about this" {
				options = append(options, opt{label: label})
				keep = true
			}
		} else if keep && trim(r) != "" && len(options) > 0 {
			options[len(options)-1].detail = append(options[len(options)-1].detail, trim(r))
		}
	}
	if len(text) == 0 {
		return Form{Headers: headers, Items: []Item{}, Cursor: cursor}, true
	}
	item := Item{ID: "q1", Text: strings.Join(text, " "), Kind: ItemSingle, Options: []Option{}, AllowsOther: other && len(options) > 0}
	if len(headers) == 1 {
		item.Header = headers[0]
	}
	switch {
	case len(options) == 0:
		item.Kind = ItemFree
	case multi:
		item.Kind = ItemMulti
	}
	for _, o := range options {
		item.Options = append(item.Options, Option{Label: o.label, Detail: strings.Join(o.detail, " ")})
	}
	return Form{Headers: headers, Items: []Item{item}, Cursor: cursor, Boxes: boxes}, true
}

// reviewItems reads the review page: "● question" starts an item, "→ answer"
// ends it. The Why of each item is its answer.
func reviewItems(rows []string) []Item {
	type text struct{ text, answer string }
	var texts []text
	open := false
	for _, r := range rows {
		t := trim(r)
		if strings.HasPrefix(t, "│") {
			t = trim(strings.TrimPrefix(t, "│"))
		}
		switch {
		case strings.HasPrefix(t, "● "):
			texts = append(texts, text{text: strings.TrimPrefix(t, "● ")})
			open = true
		case strings.HasPrefix(t, "→") && len(texts) > 0:
			texts[len(texts)-1].answer = trim(strings.TrimPrefix(t, "→"))
			open = false
		case strings.HasPrefix(t, "Ready to submit"):
			open = false
		case open && t != "":
			texts[len(texts)-1].text += " " + t
		}
	}
	out := make([]Item, len(texts))
	for i, t := range texts {
		out[i] = Item{ID: "q" + strconv.Itoa(i+1), Text: t.text, Why: t.answer, Kind: ItemFree, Options: []Option{}}
	}
	return out
}

// PromptKind is the kind of a Prompt.
type PromptKind string

const (
	PromptPermission PromptKind = "permission"
	PromptPlan       PromptKind = "plan"
)

// PromptOption is one numbered choice of a Prompt.
type PromptOption struct {
	N     int
	Label string
}

// Prompt is a tool permission prompt or the plan approval of Claude Code,
// read from the screen. A number picks the choice and sends it at once.
type Prompt struct {
	Kind     PromptKind
	Title    string   // the first row: "Bash command", "Edit file", "Ready to code?"
	Body     []string // the rows between the title and the question
	Question string   // the row above the choices
	Options  []PromptOption
	Cursor   string // the row under the cursor, with no marker
	Typed    int    // the number of the choice that takes a text, or 0
	Amending bool   // the "No" choice of a permission takes a text after Tab
}

// Key names the prompt when the transcript does not give the tool call.
func (p Prompt) Key() string {
	return strings.Join(append(append([]string{p.Title}, p.Body...), p.Question), "\n")
}

// Labels returns the label of each choice.
func (p Prompt) Labels() []string {
	out := make([]string, len(p.Options))
	for i, o := range p.Options {
		out[i] = o.Label
	}
	return out
}

func (p Prompt) question() *Question {
	kind, id := QuestionPermission, ItemPermission
	if p.Kind == PromptPlan {
		kind, id = QuestionPlan, ItemPlan
	}
	return &Question{Kind: kind, Text: p.Question, Items: []Item{p.Item(id, "", "")}, Prompt: &p}
}

// ParsePrompt returns false when no prompt is open at the bottom of the
// screen.
func ParsePrompt(screen []string) (Prompt, bool) {
	rows := lines(screen)
	if len(rows) == 0 {
		return Prompt{}, false
	}
	footer := trim(rows[len(rows)-1])
	var kind PromptKind
	switch {
	case strings.HasPrefix(footer, "ctrl+g to edit"):
		kind = PromptPlan
	case strings.HasPrefix(footer, "Esc to cancel"):
		kind = PromptPermission
	default:
		return Prompt{}, false
	}

	// The question is the first row above the choices that is not to the
	// right of the numbers.
	indent := func(r string) int {
		n := 0
		for _, c := range r {
			if c != ' ' && c != '❯' {
				break
			}
			n++
		}
		return n
	}
	column, skipped, q := -1, 0, -1
	for i := len(rows) - 2; i >= 0; i-- {
		t := trim(rows[i])
		if _, _, ok := option(rows[i]); ok {
			column = indent(rows[i])
		} else if column >= 0 && t != "" && indent(rows[i]) <= column {
			q = i
			break
		} else if column < 0 {
			// A hint row below the choices: "shift+tab to approve with this feedback".
			if skipped++; skipped > 2 {
				return Prompt{}, false
			}
		}
	}
	if q < 0 || !strings.HasSuffix(trim(rows[q]), "?") {
		return Prompt{}, false
	}

	// A choice can wrap. A row that fills the width wraps in a word.
	width := 0
	for _, r := range rows {
		width = max(width, utf8.RuneCountInString(r))
	}
	var options []PromptOption
	for i := q + 1; i < len(rows)-1; i++ {
		t := trim(rows[i])
		if n, label, ok := option(rows[i]); ok {
			options = append(options, PromptOption{N: n, Label: label})
		} else if len(options) > 0 && t != "" && !strings.HasPrefix(t, "shift+tab") {
			sep := " "
			if utf8.RuneCountInString(rows[i-1]) >= width-1 {
				sep = ""
			}
			options[len(options)-1].Label += sep + t
		}
	}
	if len(options) < 2 || options[0].N != 1 {
		return Prompt{}, false
	}

	// The prompt starts below the last rule above the question. A plan has
	// one more rule below its text.
	var rules []int
	for i := 0; i < q; i++ {
		if separator(rows[i]) {
			rules = append(rules, i)
		}
	}
	if len(rules) == 0 {
		return Prompt{}, false
	}
	top := rules[len(rules)-1]
	if kind == PromptPlan && top+1 == q && len(rules) >= 2 {
		top = rules[len(rules)-2]
	}
	var inside []string
	for _, r := range rows[top+1 : q] {
		t := trim(r)
		if t != "" && strings.Trim(t, "╌") != "" && !strings.HasPrefix(t, "Tip:") {
			inside = append(inside, t)
		}
	}
	if len(inside) == 0 {
		return Prompt{}, false
	}
	cursor := ""
	for _, r := range rows[q+1:] {
		if t := trim(r); strings.HasPrefix(t, "❯") {
			cursor = trim(dropRunes(t, 1))
		}
	}
	amending := kind == PromptPermission && !strings.Contains(footer, "Tab to amend")
	last := options[len(options)-1]
	typed := 0
	if kind == PromptPlan || strings.HasPrefix(last.Label, "No") && (strings.Contains(footer, "Tab to amend") || amending) {
		typed = last.N
	}
	return Prompt{Kind: kind, Title: inside[0], Body: append([]string{}, inside[1:]...), Question: trim(rows[q]),
		Options: options, Cursor: cursor, Typed: typed, Amending: amending}, true
}

// fileTools show only the file name of their input.
var fileTools = map[string]bool{"Write": true, "Edit": true, "MultiEdit": true, "Read": true, "NotebookEdit": true}

// Item is the prompt as one choice item. The header names the tool, and the
// text is a short form of its input: "Bash" and "git push". Empty tool and
// input fall back to the title of the prompt.
func (p Prompt) Item(id, tool, input string) Item {
	header := tool
	if header == "" {
		header = p.Title
	}
	text := inputText(tool, input)
	if p.Kind == PromptPlan {
		header, text = "Plan", "Approve the plan"
		for _, b := range p.Body {
			if strings.HasPrefix(b, "Plan: ") {
				text = strings.TrimPrefix(b, "Plan: ")
				break
			}
		}
	}
	if text == "" {
		text = p.Title
	}
	opts := make([]Option, len(p.Options))
	for i, o := range p.Options {
		opts[i] = Option{Label: o.Label}
	}
	return Item{ID: id, Header: header, Text: short(trim(text), 120), Why: p.Question,
		Kind: ItemSingle, Options: opts, AllowsOther: p.Typed != 0}
}

// inputText is a short form of a tool input: its first line, and only the
// file name for a file tool.
func inputText(tool, input string) string {
	if input == "" {
		return ""
	}
	text := strings.SplitN(strings.ReplaceAll(input, "\r\n", "\n"), "\n", 2)[0]
	if fileTools[tool] {
		text = filepath.Base(text)
	}
	return short(trim(text), 120)
}

// TurnQuestion returns the last paragraph of a reply whose last sentence is
// a question. A "?" in a code block does not count.
func TurnQuestion(message string) (string, bool) {
	var prose []string
	code := false
	for _, line := range strings.Split(message, "\n") {
		switch {
		case strings.HasPrefix(strings.TrimSpace(line), "```"):
			code = !code
			prose = append(prose, "")
		case code:
			prose = append(prose, "")
		default:
			prose = append(prose, line)
		}
	}
	if code {
		return "", false
	}
	var last string
	for _, p := range strings.Split(strings.Join(prose, "\n"), "\n\n") {
		if p = strings.TrimSpace(p); p != "" {
			last = p
		}
	}
	if last == "" {
		return "", false
	}
	r := []rune(last)
	if !strings.HasSuffix(strings.TrimSpace(message), string(r[len(r)-1])) {
		return "", false
	}
	if strings.HasSuffix(strings.Trim(last, "*_\"')”’ "), "?") {
		return last, true
	}
	return "", false
}

// lines returns the screen with no empty rows at the bottom. A rule with a
// name, drawn below the terminal by some hosts, is also dropped.
func lines(screen []string) []string {
	rows := make([]string, len(screen))
	for i, r := range screen {
		rows[i] = strings.ReplaceAll(r, " ", " ")
	}
	for len(rows) > 0 {
		r := trim(rows[len(rows)-1])
		if r != "" && !(strings.HasPrefix(r, "─") && !separator(r)) {
			break
		}
		rows = rows[:len(rows)-1]
	}
	return rows
}

// option returns the number and text of a numbered option row: "❯ 1.
// SQLite" gives 1 and "SQLite".
func option(row string) (int, string, bool) {
	t := trim(row)
	if strings.HasPrefix(t, "❯") {
		t = trim(strings.TrimPrefix(t, "❯"))
	}
	dot := strings.IndexByte(t, '.')
	if dot <= 0 || !strings.HasPrefix(t[dot+1:], " ") {
		return 0, "", false
	}
	for _, c := range t[:dot] {
		if c < '0' || c > '9' {
			return 0, "", false
		}
	}
	n, err := strconv.Atoi(t[:dot])
	if err != nil {
		return 0, "", false
	}
	return n, trim(t[dot+1:]), true
}

// boxed tells if t is a tab of the bar: "☐ Store", "☒ Tests", "✔ Submit".
func boxed(t string) bool {
	return strings.HasPrefix(t, "☐ ") || strings.HasPrefix(t, "☒ ") || strings.HasPrefix(t, "✔ ")
}

func separator(row string) bool {
	t := trim(row)
	return t != "" && strings.Trim(t, "─") == ""
}

func trim(s string) string {
	return strings.TrimFunc(s, func(r rune) bool { return r != '\n' && unicode.IsSpace(r) })
}

func dropRunes(s string, n int) string {
	for i := 0; i < n && s != ""; i++ {
		_, size := utf8.DecodeRuneInString(s)
		s = s[size:]
	}
	return s
}

// short cuts s to n characters, with "…" at the end.
func short(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// flat joins the words of s with one space.
func flat(s string) string { return strings.Join(strings.Fields(s), " ") }
