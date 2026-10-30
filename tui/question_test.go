package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

var formScreens = filepath.Join("testdata", "claude-screens")

func formScreen(t *testing.T, name string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(formScreens, name+".txt"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(string(data), "\n")
}

// expectOf has the shape of the .expect.json files.
func expectOf(t *testing.T, s []string) any {
	t.Helper()
	out := map[string]any{"form": nil, "prompt": nil}
	if f, ok := ParseForm(s); ok {
		form := map[string]any{"headers": f.Headers, "items": f.Items, "review": f.Review}
		if f.Cursor != "" {
			form["cursor"] = f.Cursor
		}
		if len(f.Boxes) > 0 {
			form["boxes"] = f.Boxes
		}
		out["form"] = form
	}
	if p, ok := ParsePrompt(s); ok {
		opts := []map[string]any{}
		for _, o := range p.Options {
			opts = append(opts, map[string]any{"n": o.N, "label": o.Label})
		}
		prompt := map[string]any{"kind": p.Kind, "title": p.Title, "body": p.Body, "question": p.Question, "options": opts, "amending": p.Amending}
		if p.Cursor != "" {
			prompt["cursor"] = p.Cursor
		}
		if p.Typed != 0 {
			prompt["typed"] = p.Typed
		}
		out["prompt"] = prompt
	}
	data, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var back any
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	return back
}

func TestFormScreensMatchExpectFiles(t *testing.T) {
	t.Parallel()
	names, _ := filepath.Glob(filepath.Join(formScreens, "*.txt"))
	if len(names) < 30 {
		t.Fatalf("only %d screens", len(names))
	}
	for _, path := range names {
		name := strings.TrimSuffix(filepath.Base(path), ".txt")
		data, err := os.ReadFile(filepath.Join(formScreens, name+".expect.json"))
		if err != nil {
			t.Fatal(err)
		}
		var want any
		if err := json.Unmarshal(data, &want); err != nil {
			t.Fatal(err)
		}
		if got := expectOf(t, formScreen(t, name)); !reflect.DeepEqual(got, want) {
			g, _ := json.MarshalIndent(got, "", "  ")
			t.Errorf("%s:\n got %s\nwant %s", name, g, data)
		}
	}
}

func TestNoForm(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"no-form-answered", "false-footer"} {
		if _, ok := ParseForm(formScreen(t, name)); ok {
			t.Errorf("%s gives a form", name)
		}
	}
	if _, ok := ParseForm(nil); ok {
		t.Error("an empty screen gives a form")
	}
	if _, ok := ParsePrompt(nil); ok {
		t.Error("an empty screen gives a prompt")
	}
}

func TestPromptItems(t *testing.T) {
	t.Parallel()
	bash, ok := ParsePrompt(formScreen(t, "permission-bash-2.1.308"))
	if !ok {
		t.Fatal("no prompt")
	}
	it := bash.Item(ItemPermission, "Bash", "git -C . status --short > status.txt")
	if it.Header != "Bash" || it.Text != "git -C . status --short > status.txt" || it.Why != "Do you want to proceed?" || len(it.Options) != 4 || !it.AllowsOther {
		t.Errorf("bash item: %+v", it)
	}
	write, _ := ParsePrompt(formScreen(t, "permission-write-2.1.308"))
	if it := write.Item(ItemPermission, "Write", "/private/tmp/formlab/notes.md"); it.Header != "Write" || it.Text != "notes.md" {
		t.Errorf("write item: %+v", it)
	}
	plan, _ := ParsePrompt(formScreen(t, "plan-approval-2.1.308"))
	if it := plan.Item(ItemPlan, "ExitPlanMode", ""); it.Header != "Plan" || it.Text != `Append "world" to notes.md` {
		t.Errorf("plan item: %+v", it)
	}
}

// Classify gives the same form and prompt as the parsers, as a question.
func TestClassifyClaudeQuestions(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		kind QuestionKind
		id   string
	}{
		{"form-single-2.1.308", QuestionForm, "q1"},
		{"form-multi", QuestionForm, "q1"},
		{"form-review", QuestionForm, ReviewID},
		{"permission-bash-2.1.308", QuestionPermission, ItemPermission},
		{"permission-edit-2.1.308", QuestionPermission, ItemPermission},
		{"plan-approval-2.1.308", QuestionPlan, ItemPlan},
	}
	for _, c := range cases {
		data, err := os.ReadFile(filepath.Join(formScreens, c.name+".txt"))
		if err != nil {
			t.Fatal(err)
		}
		sc := Classify("claude", data)
		q := sc.Question
		if sc.State != StateWaiting || q == nil || q.Kind != c.kind || len(q.Items) == 0 || q.Items[0].ID != c.id || q.Text == "" {
			t.Errorf("%s: state=%s question=%+v", c.name, sc.State, q)
			continue
		}
		if (q.Form == nil) != (c.kind != QuestionForm) || (q.Prompt == nil) == (c.kind != QuestionForm) {
			t.Errorf("%s: form=%v prompt=%v", c.name, q.Form != nil, q.Prompt != nil)
		}
	}
	for _, name := range []string{"no-form-answered", "false-footer", "turn-question-2.1.308"} {
		data, err := os.ReadFile(filepath.Join(formScreens, name+".txt"))
		if err != nil {
			t.Fatal(err)
		}
		if sc := Classify("claude", data); sc.State == StateWaiting || sc.Question != nil {
			t.Errorf("%s: state=%s question=%+v", name, sc.State, sc.Question)
		}
	}
}

func TestTurnQuestion(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"notes.md has two lines. Do you want me to add a third line?":     "notes.md has two lines. Do you want me to add a third line?",
		"I fixed the test.\n\nShould I push the branch?\n":                "Should I push the branch?",
		"Done.\n\n**Do you want the PR now?**":                            "**Do you want the PR now?**",
		"Two ways:\n\n```swift\nlet a = 1\n```\n\nWhich one do you want?": "Which one do you want?",
		"":                                    "",
		"All tests pass.":                     "",
		"Is it fast? Yes. The run takes 2 s.": "",
		"The rule:\n\n```\nwhy?\n```":         "",
		"Run this:\n```\nls ?\n```\nThen it works.": "",
		"Should I go on?\n\n```sh\nmake\n```":       "",
	} {
		got, ok := TurnQuestion(in)
		if got != want || ok != (want != "") {
			t.Errorf("TurnQuestion(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
}
