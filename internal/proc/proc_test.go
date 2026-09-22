package proc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.uber.org/goleak"
)

// TestMain proves that no test leaves a goroutine behind.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// TestChildEnvOverrideReplacesInPlace pins the merge rule: an override wins
// over the parent value, appears once, and keeps the parent position.
func TestChildEnvOverrideReplacesInPlace(t *testing.T) {
	const first = "AGENTWIRE_TEST_ENV_FIRST"
	const second = "AGENTWIRE_TEST_ENV_SECOND"
	t.Setenv(first, "parent")
	t.Setenv(second, "parent")

	env := ChildEnv(map[string]string{first: "override"})

	if got := countEnvKey(env, first); got != 1 {
		t.Fatalf("key %s appears %d times, want 1", first, got)
	}
	if got, ok := lookupEnv(env, first); !ok || got != "override" {
		t.Fatalf("key %s = %q (present=%v), want override", first, got, ok)
	}
	// The parent lists first before second. A replaced key must stay in
	// place, so a late append shows up as a wrong order.
	firstAt, secondAt := indexEnvKey(env, first), indexEnvKey(env, second)
	if firstAt < 0 || secondAt < 0 || firstAt > secondAt {
		t.Fatalf("order changed: %s at %d, %s at %d", first, firstAt, second, secondAt)
	}
}

// TestChildEnvAddsNewKeys pins that an override of an unknown key is added.
func TestChildEnvAddsNewKeys(t *testing.T) {
	const added = "AGENTWIRE_TEST_ENV_ADDED"
	env := ChildEnv(map[string]string{added: "new"})
	if got, ok := lookupEnv(env, added); !ok || got != "new" {
		t.Fatalf("key %s = %q (present=%v), want new", added, got, ok)
	}
}

// TestChildEnvDropsClaudeCodeMarker pins that the parent marker and an
// override of it both disappear.
func TestChildEnvDropsClaudeCodeMarker(t *testing.T) {
	t.Setenv(claudeCodeMarker, "parent-marker")
	env := ChildEnv(map[string]string{claudeCodeMarker: "override-marker"})
	for _, entry := range env {
		if envKeyAt(entry) == claudeCodeMarker {
			t.Fatalf("marker key survived: %q", entry)
		}
		if strings.Contains(entry, "parent-marker") || strings.Contains(entry, "override-marker") {
			t.Fatalf("marker value survived: %q", entry)
		}
	}
}

// TestChildEnvIgnoresEmptyKey pins that an empty key is dropped while an
// empty value is kept.
func TestChildEnvIgnoresEmptyKey(t *testing.T) {
	const emptyValue = "AGENTWIRE_TEST_ENV_EMPTY_VALUE"
	env := ChildEnv(map[string]string{"": "empty-key-value", emptyValue: ""})
	for _, entry := range env {
		if strings.HasPrefix(entry, "=") || strings.Contains(entry, "empty-key-value") {
			t.Fatalf("empty key produced %q", entry)
		}
	}
	if got, ok := lookupEnv(env, emptyValue); !ok || got != "" {
		t.Fatalf("key %s = %q (present=%v), want an empty value", emptyValue, got, ok)
	}
}

// TestFirstLine pins the trim rule and the first-line cut.
func TestFirstLine(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"single line", "hello", "hello"},
		{"trailing whitespace", "hello   ", "hello"},
		{"leading whitespace", "   hello", "hello"},
		{"tabs around", "\thello\t", "hello"},
		{"first of many", "first\nsecond\nthird", "first"},
		{"whitespace around the first line", "  first  \nsecond", "first"},
		{"empty", "", ""},
		{"spaces only", "   ", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := FirstLine(tc.in); got != tc.want {
				t.Fatalf("FirstLine(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestStartRejectsEmptyPath pins the guard on a missing executable.
func TestStartRejectsEmptyPath(t *testing.T) {
	t.Parallel()
	if _, err := Start(context.Background(), Opts{}); err == nil {
		t.Fatal("Start accepted an empty path")
	}
}

// TestStartReportsAMissingBinary pins that a spawn failure names the binary.
func TestStartReportsAMissingBinary(t *testing.T) {
	t.Parallel()
	const path = "/nonexistent/agentwire-missing-binary"
	_, err := Start(context.Background(), Opts{Path: path})
	if err == nil {
		t.Fatal("Start accepted a missing binary")
	}
	if !strings.Contains(err.Error(), "agentwire-missing-binary") {
		t.Fatalf("error does not name the binary: %v", err)
	}
}

// TestStartRefusesACanceledContext pins that a canceled context spawns
// nothing.
func TestStartRefusesACanceledContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Start(ctx, Opts{Path: "sh"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

// envKeyAt returns the key of one "K=V" entry.
func envKeyAt(entry string) string {
	k, _, _ := strings.Cut(entry, "=")
	return k
}

// indexEnvKey returns the position of key in env, or -1.
func indexEnvKey(env []string, key string) int {
	for i, entry := range env {
		if envKeyAt(entry) == key {
			return i
		}
	}
	return -1
}

// countEnvKey counts the entries of key in env.
func countEnvKey(env []string, key string) int {
	n := 0
	for _, entry := range env {
		if envKeyAt(entry) == key {
			n++
		}
	}
	return n
}

// lookupEnv returns the value of key in env.
func lookupEnv(env []string, key string) (string, bool) {
	for _, entry := range env {
		if k, v, ok := strings.Cut(entry, "="); ok && k == key {
			return v, true
		}
	}
	return "", false
}
