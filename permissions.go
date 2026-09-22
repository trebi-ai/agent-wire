package agentwire

import "strings"

// PermissionMode is the one policy that every harness maps onto its own
// mechanism.
type PermissionMode string

const (
	// PermissionAsk routes every request to the consumer as an
	// EventPermission. It is the default.
	PermissionAsk PermissionMode = "ask"
	// PermissionAutoEdit answers edit requests in the library and routes the
	// rest to the consumer.
	PermissionAutoEdit PermissionMode = "auto_edit"
	// PermissionAuto answers every request in the library.
	PermissionAuto PermissionMode = "auto"
	// PermissionInherit adds no permission-mode flag, so the operator's own
	// harness configuration decides. It is the mode for a consumer that must
	// not override a setting the operator made in the harness itself. The
	// host prompt callback is still installed, and requests the harness sends
	// are routed to the consumer, like ask.
	PermissionInherit PermissionMode = "inherit"
)

// PermissionPolicy is the normalized permission policy.
type PermissionPolicy struct {
	Mode PermissionMode
	// AllowTools lists extra tools that are allowed without asking, in "ask"
	// and "auto_edit" mode. Patterns may end in "*", for example
	// "mcp__sling__*".
	AllowTools []string
}

// Normalized returns the policy with a concrete mode.
func (p PermissionPolicy) Normalized() PermissionPolicy {
	if p.Mode == "" {
		p.Mode = PermissionAsk
	}
	return p
}

// Answer returns the decision the policy makes without the consumer. ok=false
// means the request becomes an EventPermission.
func (p PermissionPolicy) Answer(kind ToolKind, tool string) (Decision, bool) {
	p = p.Normalized()
	switch p.Mode {
	case PermissionAuto:
		return Decision{Allow: true}, true
	case PermissionAutoEdit:
		if kind == ToolEdit {
			return Decision{Allow: true}, true
		}
	}
	if p.allows(tool) {
		return Decision{Allow: true}, true
	}
	return Decision{}, false
}

// allows reports whether tool matches AllowTools.
func (p PermissionPolicy) allows(tool string) bool {
	if tool == "" {
		return false
	}
	for _, pattern := range p.AllowTools {
		if matchTool(pattern, tool) {
			return true
		}
	}
	return false
}

// matchTool matches a tool name against a pattern with an optional trailing
// "*". It is deliberately not a full glob: tool names carry "__" and ":" but
// never a path separator, and a trailing star is the only shape consumers use.
func matchTool(pattern, tool string) bool {
	if pattern == "" {
		return false
	}
	if pattern == "*" {
		return true
	}
	if prefix, ok := strings.CutSuffix(pattern, "*"); ok {
		return strings.HasPrefix(tool, prefix)
	}
	return pattern == tool
}

// TrustFlags are the harness flags that turn a permission mode into a
// standing grant. They are derived from the policy, never always-on.
func trustFlags(h Harness, p PermissionPolicy) []string {
	if p.Normalized().Mode != PermissionAuto {
		return nil
	}
	switch h {
	case Cursor:
		return []string{"--trust"}
	case Pi:
		return []string{"--approve"}
	}
	return nil
}

// claudePermissionMode maps the policy onto Claude's --permission-mode.
func claudePermissionMode(p PermissionPolicy) string {
	switch p.Normalized().Mode {
	case PermissionAuto:
		return "bypassPermissions"
	case PermissionAutoEdit:
		return "acceptEdits"
	}
	return "default"
}

// codexApprovalPolicy maps the policy onto the Codex approval policy. The
// sandbox stays workspace-write in every mode: an unattended library must not
// hand a child the whole disk.
func codexApprovalPolicy(p PermissionPolicy) string {
	if p.Normalized().Mode == PermissionAuto {
		return "never"
	}
	return "on-request"
}
