package agentwire

import (
	"slices"
	"testing"
)

func TestStaticModels(t *testing.T) {
	cases := []struct {
		h     Harness
		first string
		empty bool
	}{
		{Claude, "sonnet", false},
		{Codex, "gpt-5.6-sol", false},
		{Copilot, "claude-sonnet-4.6", false},
		{Grok, "", true},
		{OpenCode, "", true},
		{Fake, "", true},
	}
	for _, c := range cases {
		got := StaticModels(c.h)
		if c.empty {
			if got != nil {
				t.Errorf("%s: %v, want nil", c.h, got)
			}
			continue
		}
		if len(got) == 0 || got[0].ID != c.first {
			t.Errorf("%s: %v, want first %q", c.h, got, c.first)
		}
		got[0].ID = "changed"
		if StaticModels(c.h)[0].ID != c.first {
			t.Errorf("%s: StaticModels returns the table, not a copy", c.h)
		}
	}
}

func TestEfforts(t *testing.T) {
	cases := []struct {
		h    Harness
		want []string
	}{
		{Claude, []string{"low", "medium", "high", "xhigh", "max"}},
		{Codex, []string{"none", "minimal", "low", "medium", "high", "xhigh"}},
		{Grok, []string{"low", "medium", "high", "xhigh"}},
		{OMP, []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}},
		{Gemini, []string{}},
		{Cursor, []string{}},
		{OpenCode, nil},
		{OpenCode2, nil},
	}
	for _, c := range cases {
		got := Efforts(c.h, "any")
		if !slices.Equal(got, c.want) || (got == nil) != (c.want == nil) {
			t.Errorf("%s: %q, want %q", c.h, got, c.want)
		}
	}
	if DefaultEffort(Codex) != "medium" || DefaultEffort(Claude) != "" {
		t.Error("DefaultEffort: want medium for Codex only")
	}
}

// Each listed effort level renders a flag through ModelArgs.
func TestEffortsMatchModelArgs(t *testing.T) {
	for h := range effortLevels {
		for _, e := range Efforts(h, "") {
			if args := ModelArgs(h, "", e); len(args) == 0 {
				t.Errorf("%s: effort %q gives no flag", h, e)
			}
		}
	}
	for _, h := range []Harness{Gemini, Cursor} {
		if args := ModelArgs(h, "", "high"); len(args) != 0 {
			t.Errorf("%s has no effort, but ModelArgs gives %q", h, args)
		}
	}
}
