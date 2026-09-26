package native

import (
	"context"
	"fmt"
	"strings"
)

// Compactor folds a full message list into one that fits the context
// window. The loop runs it before a model call when the list has grown past
// the configured token budget.
type Compactor interface {
	Compact(ctx context.Context, m Model, msgs []Message) ([]Message, error)
}

// CompactorFunc adapts a function to Compactor.
type CompactorFunc func(ctx context.Context, m Model, msgs []Message) ([]Message, error)

func (f CompactorFunc) Compact(ctx context.Context, m Model, msgs []Message) ([]Message, error) {
	return f(ctx, m, msgs)
}

// SummaryCompactor keeps the last keepLast messages verbatim and replaces
// everything before them with one summary the model itself writes. It is
// the trebi compact rule: trigger around 60% of the context budget
// (plan 2026-09-26 I.5).
func SummaryCompactor(keepLast int) Compactor {
	if keepLast < 1 {
		keepLast = 1
	}
	return CompactorFunc(func(ctx context.Context, m Model, msgs []Message) ([]Message, error) {
		if len(msgs) <= keepLast+1 {
			return msgs, nil
		}
		cut := len(msgs) - keepLast
		head, tail := msgs[:cut], msgs[cut:]

		var b strings.Builder
		b.WriteString("Summarize the conversation so far for a continuation. ")
		b.WriteString("Keep the task, the decisions, the file paths, and the open questions. Be brief.\n\n")
		for _, m := range head {
			b.WriteString(roleLabel(m.Role))
			b.WriteString(": ")
			b.WriteString(flatText(m))
			b.WriteString("\n")
		}
		sum, err := callText(ctx, m, Request{Messages: []Message{{Role: RoleUser, Parts: []Part{{Text: &TextPart{Text: b.String()}}}}}})
		if err != nil {
			return nil, fmt.Errorf("native: compaction summary: %w", err)
		}
		out := []Message{{Role: RoleUser, Parts: []Part{{Text: &TextPart{
			Text: "Summary of the earlier conversation:\n" + sum,
		}}}}}
		return append(out, tail...), nil
	})
}

func roleLabel(r Role) string {
	switch r {
	case RoleUser:
		return "user"
	case RoleAssistant:
		return "assistant"
	case RoleTool:
		return "tool"
	}
	return string(r)
}

// flatText renders one message as plain text, tool shapes included.
func flatText(m Message) string {
	var b strings.Builder
	for _, p := range m.Parts {
		switch {
		case p.Text != nil:
			b.WriteString(p.Text.Text)
		case p.ToolCall != nil:
			fmt.Fprintf(&b, "[tool call %s(%s)]", p.ToolCall.Name, truncateBytes(p.ToolCall.Input, 200))
		case p.ToolResult != nil:
			texts := make([]string, 0, len(p.ToolResult.Content))
			for _, c := range p.ToolResult.Content {
				if c.Text != nil {
					texts = append(texts, c.Text.Text)
				}
			}
			fmt.Fprintf(&b, "[tool %s -> %s]", p.ToolResult.Name, truncateStr(strings.Join(texts, " "), 300))
		case p.Reasoning != nil:
			// Reasoning is the model's own scratch space; a summary skips it.
		}
		b.WriteString(" ")
	}
	return strings.TrimSpace(b.String())
}

// callText runs one no-tools model call and returns the text.
func callText(ctx context.Context, m Model, req Request) (string, error) {
	req.Tools = nil
	var b strings.Builder
	for part, err := range m.Stream(ctx, req) {
		if err != nil {
			return "", err
		}
		if part.Kind == StreamTextDelta && part.Delta != "" {
			b.WriteString(part.Delta)
		}
		if part.Kind == StreamToolCallDone {
			return "", fmt.Errorf("native: compaction summary called a tool")
		}
	}
	return b.String(), nil
}

// truncateBytes bounds a byte slice for a summary line.
func truncateBytes(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}

// truncateStr bounds a string for a summary line.
func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
