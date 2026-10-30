package tui

import (
	"strings"
	"testing"
)

func TestDialogKeyIgnoresCursor(t *testing.T) {
	t.Parallel()
	yes := "Is this a project you created or one you trust?\n❯ 1. Yes, I trust this folder\n  2. No, exit\nEnter to confirm"
	no := "Is this a project you created or one you trust?\n  1. Yes, I trust this folder\n❯ 2. No, exit\nEnter to confirm"
	act := Action{Text: "1"}
	if DialogKey(yes, act) != DialogKey(no, act) {
		t.Fatal("a cursor move must not change the dialog key")
	}
}

func TestTrustAction(t *testing.T) {
	t.Parallel()
	if got := TrustAction("❯ 1. Yes, I trust this folder\n  2. No, exit"); got != (Action{Text: "1"}) {
		t.Fatalf("numbered: %+v", got)
	}
	if got := TrustAction("Trust project folder?\n→ Trust\n   Do not trust"); got != (Action{Key: "enter"}) {
		t.Fatalf("arrow: %+v", got)
	}
}

func TestTrustYesActions(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, screen string
		want         []Action
	}{
		{"no first", "Is this a project you created or one you trust?\n❯ No, exit\n  Yes, I trust this folder\nEnter to confirm", []Action{{Key: "down"}, {Key: "enter"}}},
		{"no below", "Is this a project you created or one you trust?\n  Yes, I trust this folder\n❯ No, exit\nEnter to confirm", []Action{{Key: "up"}, {Key: "enter"}}},
		{"yes selected", "Is this a project you created or one you trust?\n❯ Yes, I trust this folder\n  No, exit\nEnter to confirm", []Action{{Key: "enter"}}},
		{"numbered", "❯ 1. Yes, I trust this folder\n  2. No, exit", []Action{{Text: "1"}}},
	}
	for _, c := range cases {
		if got := TrustYesActions(c.screen); !equalActions(got, c.want) {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestChoiceDialog(t *testing.T) {
	t.Parallel()
	q, ok := ChoiceDialog("Allow network?\n❯ 1. Yes\n  2. No\nEnter to confirm · Esc to cancel")
	if !ok || !strings.Contains(q, "Allow network?") {
		t.Fatalf("got ok=%v q=%q", ok, q)
	}
	if _, ok := ChoiceDialog("❯ Try \"fix typecheck\"\n  Fable 5 | 0.5 min | 0K (0%)"); ok {
		t.Fatal("an idle composer is not a choice dialog")
	}
	if _, ok := ChoiceDialog("Should I delete prod?"); ok {
		t.Fatal("a bare question is not a choice dialog")
	}
}

func TestChoiceQuestionOptions(t *testing.T) {
	t.Parallel()
	q := choiceQuestion("Pick a mode?\n❯ 1. Fast\n  2. Safe\n  9. Other\nEnter to confirm")
	if q == nil || q.Text != "Pick a mode?" {
		t.Fatalf("question = %+v", q)
	}
	got := q.Items[0].Options
	if len(got) != 2 || got[0].Label != "Fast" || got[1].Label != "Safe" {
		t.Fatalf("options = %+v", got)
	}
	if choiceQuestion("no options here") != nil {
		t.Fatal("want nil with no numbered options")
	}
}
