package agentwire

import (
	"slices"
	"testing"
)

// TestSplitArgsWordRules covers the POSIX word-splitting rules the launch
// builder needs: whitespace, quotes, backslash escapes and the degenerate
// inputs a consumer can write.
func TestSplitArgsWordRules(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{name: "empty", in: "", want: nil},
		{name: "only whitespace", in: " \t\n ", want: nil},
		{name: "plain words", in: "a b c", want: []string{"a", "b", "c"}},
		{name: "runs of whitespace", in: "  a\t\tb\n c  ", want: []string{"a", "b", "c"}},
		{name: "single quotes keep spaces", in: `'a b' c`, want: []string{"a b", "c"}},
		{name: "single quotes are literal", in: `'a\tb'`, want: []string{`a\tb`}},
		{name: "double quotes keep spaces", in: `"a b" c`, want: []string{"a b", "c"}},
		{name: "double quote escapes quote", in: `"a\"b"`, want: []string{`a"b`}},
		{name: "double quote escapes backslash", in: `"a\\b"`, want: []string{`a\b`}},
		{name: "double quote keeps other backslashes", in: `"a\nb"`, want: []string{`a\nb`}},
		{name: "backslash outside quotes escapes space", in: `a\ b`, want: []string{"a b"}},
		{name: "backslash outside quotes escapes quote", in: `a\"b`, want: []string{`a"b`}},
		{name: "backslash escapes backslash", in: `a\\b`, want: []string{`a\b`}},
		{name: "empty double quoted argument", in: `a ""`, want: []string{"a", ""}},
		{name: "empty single quoted argument", in: `a ''`, want: []string{"a", ""}},
		{name: "unterminated double quote keeps the rest", in: `"a b`, want: []string{"a b"}},
		{name: "unterminated single quote keeps the rest", in: `'a b`, want: []string{"a b"}},
		{name: "trailing backslash is literal", in: `a\`, want: []string{`a\`}},
		{name: "quoted path with spaces", in: `--append-system-prompt "/tmp/a b/c.md"`, want: []string{"--append-system-prompt", "/tmp/a b/c.md"}},
		{name: "flag and empty value", in: `-c ""`, want: []string{"-c", ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SplitArgs(tc.in)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("SplitArgs(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
