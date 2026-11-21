package agentwire

import (
	"slices"
	"sort"
)

// Vendor hook event names. Claude Code and Codex use the same names.
const (
	HookSessionStart      = "SessionStart"
	HookUserPromptSubmit  = "UserPromptSubmit"
	HookPreToolUse        = "PreToolUse"
	HookPostToolUse       = "PostToolUse"
	HookPermissionRequest = "PermissionRequest"
	HookNotification      = "Notification"
	HookStop              = "Stop"
	HookStopFailure       = "StopFailure"
	HookSessionEnd        = "SessionEnd"
)

// Vendor hook payload values.
const (
	// HookSourceCompact is the source of a SessionStart after a compaction.
	HookSourceCompact = "compact"
	// HookNotificationPermission is the notification_type of a permission
	// prompt.
	HookNotificationPermission = "permission_prompt"
)

// HookEvent is one vendor hook event. Matcher is a tool name pattern, or empty
// for all tools.
type HookEvent struct{ Name, Matcher string }

// HookRecipe holds the hook facts of one vendor CLI.
type HookRecipe struct {
	Harness Harness
	// Config is the hook config file, relative to the vendor home.
	Config string
	// Trust is the file that keeps the hook trust, or empty when the vendor
	// runs a new hook with no trust step.
	Trust string
	// Events are the events to hook, in install order.
	Events []HookEvent
	// Questions are the tools that ask the user a question.
	Questions []string
	// Plan is the tool that asks the user to approve a plan, or empty.
	Plan string
}

// Question reports whether tool asks the user a question.
func (r HookRecipe) Question(tool string) bool {
	return slices.Contains(r.Questions, tool)
}

//nolint:gochecknoglobals // a constant table of vendor facts
var hookRecipes = map[Harness]HookRecipe{
	Claude: {
		Harness: Claude,
		Config:  "settings.json",
		Events: []HookEvent{
			{Name: HookSessionStart},
			{Name: HookUserPromptSubmit},
			{Name: HookPreToolUse, Matcher: "AskUserQuestion|ExitPlanMode"},
			{Name: HookPostToolUse, Matcher: "AskUserQuestion|ExitPlanMode"},
			{Name: HookPermissionRequest},
			{Name: HookNotification},
			{Name: HookStop},
			{Name: HookStopFailure},
			{Name: HookSessionEnd},
		},
		Questions: []string{"AskUserQuestion"},
		Plan:      "ExitPlanMode",
	},
	// Codex runs a new hook only after the user trusts it in Codex.
	Codex: {
		Harness: Codex,
		Config:  "hooks.json",
		Trust:   "config.toml",
		Events: []HookEvent{
			{Name: HookSessionStart},
			{Name: HookUserPromptSubmit},
			{Name: HookPreToolUse, Matcher: "request_user_input"},
			{Name: HookPermissionRequest},
			{Name: HookStop},
		},
		Questions: []string{"request_user_input"},
	},
}

// Hooks gives the hook facts of h. The result is a copy. ok is false when h
// has no hook support.
func Hooks(h Harness) (HookRecipe, bool) {
	r, ok := hookRecipes[h]
	if !ok {
		return HookRecipe{}, false
	}
	r.Events = slices.Clone(r.Events)
	r.Questions = slices.Clone(r.Questions)
	return r, true
}

// HookHarnesses lists the harnesses with hook support, sorted by name.
func HookHarnesses() []Harness {
	out := make([]Harness, 0, len(hookRecipes))
	for h := range hookRecipes {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
