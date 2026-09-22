package agentwire

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// injTestLaunch builds a bare launch for direct applyInstructions calls.
func injTestLaunch() *launch {
	return &launch{bin: "/bin/sh", env: map[string]string{}}
}

// TestInstructionsRendering checks the per-harness instruction rendering.
func TestInstructionsRendering(t *testing.T) {
	ctx := context.Background()
	const text = "be brief and precise"
	tests := []struct {
		harness  Harness
		flag     bool
		wantKept bool
	}{
		{Claude, true, false},
		{Codex, false, true},
		{OpenCode, false, true},
		{OpenCode2, false, true},
		{Copilot, false, true},
		{Cursor, false, true},
		{Gemini, false, true},
	}
	for _, tt := range tests {
		t.Run(string(tt.harness), func(t *testing.T) {
			rt := injTestRuntime(t)
			req := StartRequest{Harness: tt.harness, Binary: "/bin/sh", Instructions: text}
			l, err := rt.prepare(ctx, &req, false)
			if err != nil {
				t.Fatalf("prepare: %v", err)
			}
			defer l.cleanup()

			if tt.flag {
				if !injTestArgPair(l.args, "--append-system-prompt", text) {
					t.Fatalf("--append-system-prompt %q missing from %q", text, l.args)
				}
			} else if slices.Contains(l.args, "--append-system-prompt") {
				t.Fatalf("instructions leaked into the args: %q", l.args)
			}

			if tt.wantKept {
				if l.instructions != text {
					t.Fatalf("l.instructions = %q, want %q", l.instructions, text)
				}
			} else if l.instructions != "" {
				t.Fatalf("l.instructions = %q, want empty", l.instructions)
			}
		})
	}
}

// TestInstructionsJoinOrder checks that the skill index follows the consumer's
// text.
func TestInstructionsJoinOrder(t *testing.T) {
	if got := joinInstructions("consumer text", "skill index"); got != "consumer text\n\nskill index" {
		t.Fatalf("joinInstructions = %q", got)
	}
	if got := joinInstructions("consumer text", ""); got != "consumer text" {
		t.Fatalf("joinInstructions with no index = %q", got)
	}
	if got := joinInstructions("", "skill index"); got != "skill index" {
		t.Fatalf("joinInstructions with no text = %q", got)
	}

	ctx := context.Background()
	rt := injTestRuntime(t)
	l := injTestLaunch()
	req := StartRequest{Harness: Codex}
	joined := joinInstructions("consumer text", "skill index")
	if err := rt.applyInstructions(ctx, &req, l, joined); err != nil {
		t.Fatalf("applyInstructions: %v", err)
	}
	if l.instructions != joined {
		t.Fatalf("l.instructions = %q, want %q", l.instructions, joined)
	}
	if i, j := strings.Index(l.instructions, "consumer text"), strings.Index(l.instructions, "skill index"); i < 0 || j < 0 || i > j {
		t.Fatalf("index does not follow the consumer text: %q", l.instructions)
	}
}

// TestInstructionsBlock checks the first-prompt wrapper.
func TestInstructionsBlock(t *testing.T) {
	if got := instructionBlock(""); got != "" {
		t.Fatalf("instructionBlock(\"\") = %q, want empty", got)
	}
	if got := instructionBlock("   \n "); got != "" {
		t.Fatalf("instructionBlock(blank) = %q, want empty", got)
	}
	want := "<session-instructions>\nhi\n</session-instructions>"
	if got := instructionBlock("hi"); got != want {
		t.Fatalf("instructionBlock = %q, want %q", got, want)
	}
	if got := withInstructions("hi", "prompt", true); got != want+"\n\nprompt" {
		t.Fatalf("withInstructions first turn = %q", got)
	}
	if got := withInstructions("hi", "prompt", false); got != "prompt" {
		t.Fatalf("withInstructions later turn = %q, want prompt", got)
	}
	if got := withInstructions("", "prompt", true); got != "prompt" {
		t.Fatalf("withInstructions with no instructions = %q, want prompt", got)
	}
}
