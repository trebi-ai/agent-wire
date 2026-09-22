package agentwire

import (
	"context"
	"strings"
)

// applyInstructions renders per-session instructions where the harness takes
// them as a launch flag.
//
// A harness that takes the text on the wire keeps it in l.instructions for the
// driver: Codex sends it as thread/start developerInstructions, OpenCode as
// system on the first prompt, OpenCode2 as a per-session instruction entry, and
// the ACP agents get a fenced block prepended to the first prompt.
func (rt *Runtime) applyInstructions(ctx context.Context, req *StartRequest, l *launch, text string) error {
	if strings.TrimSpace(text) == "" {
		l.instructions = ""
		return nil
	}
	// A harness that takes the text on the wire keeps it here for the driver.
	l.instructions = text
	switch req.Harness {
	case Claude:
		// The text travels as one argv element. Claude Code's
		// --append-system-prompt takes text, not a file, so a caller with a
		// very large instruction set should ship it as a skill instead.
		l.args = append(l.args, "--append-system-prompt", text)
		l.instructions = ""
	case Pi:
		if rt.supportsFlag(ctx, l.bin, "--append-system-prompt") {
			l.args = append(l.args, "--append-system-prompt", text)
			l.instructions = ""
		}
	case Fake:
		l.instructions = ""
	}
	return nil
}

// instructionBlock renders instructions for a protocol with no field for them,
// so they are prepended to the first prompt.
func instructionBlock(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	return "<session-instructions>\n" + text + "\n</session-instructions>"
}

// withInstructions prepends the session instructions to the first prompt text.
// Later turns carry the user's text alone.
func withInstructions(instructions, text string, first bool) string {
	if !first {
		return text
	}
	block := instructionBlock(instructions)
	if block == "" {
		return text
	}
	return block + "\n\n" + text
}
