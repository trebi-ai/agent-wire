package agentwire

import (
	"reflect"
	"testing"
)

func TestHooks(t *testing.T) {
	tests := []struct {
		h      Harness
		ok     bool
		config string
		trust  string
		events []HookEvent
		quest  []string
		plan   string
	}{
		{
			h: Claude, ok: true, config: "settings.json",
			events: []HookEvent{
				{"SessionStart", ""},
				{"UserPromptSubmit", ""},
				{"PreToolUse", "AskUserQuestion|ExitPlanMode"},
				{"PostToolUse", "AskUserQuestion|ExitPlanMode"},
				{"PermissionRequest", ""},
				{"Notification", ""},
				{"Stop", ""},
				{"StopFailure", ""},
				{"SessionEnd", ""},
			},
			quest: []string{"AskUserQuestion"}, plan: "ExitPlanMode",
		},
		{
			h: Codex, ok: true, config: "hooks.json", trust: "config.toml",
			events: []HookEvent{
				{"SessionStart", ""},
				{"UserPromptSubmit", ""},
				{"PreToolUse", "request_user_input"},
				{"PermissionRequest", ""},
				{"Stop", ""},
			},
			quest: []string{"request_user_input"},
		},
		{h: OpenCode},
	}
	for _, tt := range tests {
		t.Run(string(tt.h), func(t *testing.T) {
			r, ok := Hooks(tt.h)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if !ok {
				return
			}
			want := HookRecipe{Harness: tt.h, Config: tt.config, Trust: tt.trust, Events: tt.events, Questions: tt.quest, Plan: tt.plan}
			if !reflect.DeepEqual(r, want) {
				t.Errorf("recipe = %+v, want %+v", r, want)
			}
		})
	}
}

func TestHooksReturnsACopy(t *testing.T) {
	r, _ := Hooks(Claude)
	r.Events[0].Name = "changed"
	r.Questions[0] = "changed"
	again, _ := Hooks(Claude)
	if again.Events[0].Name != HookSessionStart || again.Questions[0] != "AskUserQuestion" {
		t.Errorf("table changed: %+v", again)
	}
}

func TestHookHarnesses(t *testing.T) {
	got := HookHarnesses()
	want := []Harness{Claude, Codex}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("harnesses = %v, want %v", got, want)
	}
}

func TestHookRecipeQuestion(t *testing.T) {
	r, _ := Hooks(Claude)
	if !r.Question("AskUserQuestion") {
		t.Error("AskUserQuestion is not a question")
	}
	if r.Question("ExitPlanMode") {
		t.Error("ExitPlanMode is a question")
	}
}
