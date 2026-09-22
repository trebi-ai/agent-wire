package agentwire

import (
	"slices"
	"testing"
)

// TestPermissionPolicyAnswerAllModes covers the decisions the policy makes
// without the consumer. It is the whole contract of PermissionMode.
func TestPermissionPolicyAnswerAllModes(t *testing.T) {
	cases := []struct {
		name    string
		policy  PermissionPolicy
		kind    ToolKind
		tool    string
		wantOK  bool
		wantAny bool
	}{
		{
			name:   "ask never answers an edit",
			policy: PermissionPolicy{Mode: PermissionAsk},
			kind:   ToolEdit, tool: "Edit",
		},
		{
			name:   "ask never answers an exec",
			policy: PermissionPolicy{Mode: PermissionAsk},
			kind:   ToolExec, tool: "Bash",
		},
		{
			name:   "empty mode is ask",
			policy: PermissionPolicy{},
			kind:   ToolEdit, tool: "Edit",
		},
		{
			// inherit routes requests to the consumer like ask; it only
			// changes which flag the launch builder emits.
			name:   "inherit never answers an edit",
			policy: PermissionPolicy{Mode: PermissionInherit},
			kind:   ToolEdit, tool: "Edit",
		},
		{
			name:   "inherit still answers an allowed tool",
			policy: PermissionPolicy{Mode: PermissionInherit, AllowTools: []string{"Bash"}},
			kind:   ToolExec, tool: "Bash",
			wantOK: true, wantAny: true,
		},
		{
			name:   "ask allows an exact allowed tool",
			policy: PermissionPolicy{Mode: PermissionAsk, AllowTools: []string{"Bash"}},
			kind:   ToolExec, tool: "Bash",
			wantOK: true, wantAny: true,
		},
		{
			name:   "ask allow list is case sensitive",
			policy: PermissionPolicy{Mode: PermissionAsk, AllowTools: []string{"Bash"}},
			kind:   ToolExec, tool: "bash",
		},
		{
			name:   "ask allows a trailing-star prefix",
			policy: PermissionPolicy{Mode: PermissionAsk, AllowTools: []string{"mcp__sling__*"}},
			kind:   ToolMCP, tool: "mcp__sling__foo",
			wantOK: true, wantAny: true,
		},
		{
			name:   "ask trailing-star does not cross the prefix",
			policy: PermissionPolicy{Mode: PermissionAsk, AllowTools: []string{"mcp__sling__*"}},
			kind:   ToolMCP, tool: "mcp__other__foo",
		},
		{
			name:   "ask trailing-star matches the bare prefix",
			policy: PermissionPolicy{Mode: PermissionAsk, AllowTools: []string{"mcp__sling__*"}},
			kind:   ToolMCP, tool: "mcp__sling__",
			wantOK: true, wantAny: true,
		},
		{
			name:   "bare star allows any tool",
			policy: PermissionPolicy{Mode: PermissionAsk, AllowTools: []string{"*"}},
			kind:   ToolOther, tool: "anything",
			wantOK: true, wantAny: true,
		},
		{
			name:   "empty tool name is never allowed",
			policy: PermissionPolicy{Mode: PermissionAsk, AllowTools: []string{"*"}},
			kind:   ToolOther, tool: "",
		},
		{
			name:   "auto_edit answers edits",
			policy: PermissionPolicy{Mode: PermissionAutoEdit},
			kind:   ToolEdit, tool: "Write",
			wantOK: true, wantAny: true,
		},
		{
			name:   "auto_edit does not answer exec",
			policy: PermissionPolicy{Mode: PermissionAutoEdit},
			kind:   ToolExec, tool: "Bash",
		},
		{
			name:   "auto_edit still answers an allowed tool",
			policy: PermissionPolicy{Mode: PermissionAutoEdit, AllowTools: []string{"Bash"}},
			kind:   ToolExec, tool: "Bash",
			wantOK: true, wantAny: true,
		},
		{
			name:   "auto answers everything",
			policy: PermissionPolicy{Mode: PermissionAuto},
			kind:   ToolOther, tool: "Whatever",
			wantOK: true, wantAny: true,
		},
		{
			name:   "auto answers even an empty tool name",
			policy: PermissionPolicy{Mode: PermissionAuto},
			kind:   ToolOther, tool: "",
			wantOK: true, wantAny: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := tc.policy.Answer(tc.kind, tc.tool)
			if ok != tc.wantOK {
				t.Fatalf("Answer(%v, %q) ok = %v, want %v", tc.kind, tc.tool, ok, tc.wantOK)
			}
			if !ok {
				if got.Allow {
					t.Fatalf("Answer(%v, %q) = %+v on an unanswered request", tc.kind, tc.tool, got)
				}
				return
			}
			if got.Allow != tc.wantAny {
				t.Fatalf("Answer(%v, %q) = %+v, want Allow=%v", tc.kind, tc.tool, got, tc.wantAny)
			}
		})
	}
}

// TestPermissionClaudeModeMapping pins the three values Claude's
// --permission-mode accepts.
func TestPermissionClaudeModeMapping(t *testing.T) {
	cases := []struct {
		in   PermissionPolicy
		want string
	}{
		{PermissionPolicy{}, "default"},
		{PermissionPolicy{Mode: PermissionAsk}, "default"},
		{PermissionPolicy{Mode: PermissionAutoEdit}, "acceptEdits"},
		{PermissionPolicy{Mode: PermissionAuto}, "bypassPermissions"},
	}
	for _, tc := range cases {
		if got := claudePermissionMode(tc.in); got != tc.want {
			t.Fatalf("claudePermissionMode(%+v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestPermissionCodexApprovalPolicy pins the mapping. The sandbox stays
// workspace-write in every mode, so only the approval policy moves.
func TestPermissionCodexApprovalPolicy(t *testing.T) {
	cases := []struct {
		in   PermissionPolicy
		want string
	}{
		{PermissionPolicy{}, "on-request"},
		{PermissionPolicy{Mode: PermissionAsk}, "on-request"},
		{PermissionPolicy{Mode: PermissionAutoEdit}, "on-request"},
		{PermissionPolicy{Mode: PermissionAuto}, "never"},
	}
	for _, tc := range cases {
		if got := codexApprovalPolicy(tc.in); got != tc.want {
			t.Fatalf("codexApprovalPolicy(%+v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestPermissionTrustFlags asserts that a standing grant is derived from the
// policy only in auto mode.
func TestPermissionTrustFlags(t *testing.T) {
	cases := []struct {
		name   string
		h      Harness
		policy PermissionPolicy
		want   []string
	}{
		{name: "cursor auto trusts", h: Cursor, policy: PermissionPolicy{Mode: PermissionAuto}, want: []string{"--trust"}},
		{name: "cursor ask does not trust", h: Cursor, policy: PermissionPolicy{Mode: PermissionAsk}},
		{name: "cursor auto_edit does not trust", h: Cursor, policy: PermissionPolicy{Mode: PermissionAutoEdit}},
		{name: "cursor inherit does not trust", h: Cursor, policy: PermissionPolicy{Mode: PermissionInherit}},
		{name: "pi auto approves", h: Pi, policy: PermissionPolicy{Mode: PermissionAuto}, want: []string{"--approve"}},
		{name: "pi ask does not approve", h: Pi, policy: PermissionPolicy{Mode: PermissionAsk}},
		{name: "claude has no trust flag", h: Claude, policy: PermissionPolicy{Mode: PermissionAuto}},
		{name: "copilot has no trust flag", h: Copilot, policy: PermissionPolicy{Mode: PermissionAuto}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := trustFlags(tc.h, tc.policy)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("trustFlags(%s, %+v) = %q, want %q", tc.h, tc.policy, got, tc.want)
			}
		})
	}
}
