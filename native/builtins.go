// The built-in file and shell tools of the native harness: read, write,
// edit, glob, grep, and bash, all rooted at one jail directory
// (plan 2026-09-26 I.3). Every path argument is relative to the root; an
// absolute path or one that escapes the root is refused.
package native

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/trebi-ai/agent-wire"
)

// builtinOptions configures the built-in toolset.
type builtinOptions struct {
	// Root is the jail directory. Paths resolve inside it and bash runs in
	// it. Empty is the process working directory.
	Root string
	// Env is the bash environment. Empty filters the parent environment,
	// so provider keys never leak into a command.
	Env []string
	// BashTimeout bounds one command. Zero uses one minute.
	BashTimeout time.Duration
	// MaxFileBytes bounds a read or a write. Zero uses 10 MiB.
	MaxFileBytes int64
}

// newBuiltinTools returns the six built-in tools.
func newBuiltinTools(opts builtinOptions) []Tool {
	if opts.Root == "" {
		opts.Root = "."
	}
	opts.Root = filepath.Clean(opts.Root)
	if opts.BashTimeout <= 0 {
		opts.BashTimeout = time.Minute
	}
	if opts.MaxFileBytes <= 0 {
		opts.MaxFileBytes = 10 << 20
	}
	return []Tool{
		newFunc(bashSpec(opts), func(ctx context.Context, in bashInput) (string, error) { return runBash(ctx, opts, in.Command) }),
		newFunc(readSpec, func(ctx context.Context, in pathInput) (string, error) {
			data, err := readFile(ctx, opts, in.Path)
			return string(data), err
		}),
		newFunc(writeSpec, func(ctx context.Context, in writeInput) (string, error) {
			return "wrote " + in.Path, writeFile(ctx, opts, in.Path, []byte(in.Content))
		}),
		newFunc(editSpec, func(ctx context.Context, in editInput) (string, error) { return runEdit(ctx, opts, in) }),
		newFunc(globSpec, func(ctx context.Context, in patternInput) (string, error) { return runGlob(opts, in.Pattern) }),
		newFunc(grepSpec, func(ctx context.Context, in grepInput) (string, error) { return runGrep(opts, in.Pattern, in.Path) }),
	}
}

// newFunc adapts a typed function with a string result. An error result is
// a model-visible tool failure.
func newFunc[In any](spec ToolSpec, fn func(context.Context, In) (string, error)) Tool {
	return NewFunc(spec, func(ctx context.Context, in In) (json.RawMessage, error) {
		out, err := fn(ctx, in)
		if err != nil {
			return nil, err
		}
		body, _ := json.Marshal(out)
		return body, nil
	})
}

type bashInput struct {
	Command string `json:"command"`
}

type pathInput struct {
	Path string `json:"path"`
}

type writeInput struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type editInput struct {
	Path      string `json:"path"`
	OldString string `json:"old_string"`
	NewString string `json:"new_string"`
}

type patternInput struct {
	Pattern string `json:"pattern"`
}

type grepInput struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path,omitempty"`
}

const str = `{"type":"string"}`

func schema(props map[string]any, required ...string) json.RawMessage {
	m := map[string]any{"type": "object", "properties": props, "required": required}
	b, _ := json.Marshal(m)
	return b
}

func bashSpec(opts builtinOptions) ToolSpec {
	return ToolSpec{
		Name:        "bash",
		Title:       "Run a command",
		Description: "Run a shell command in the workspace root. No login shell.",
		InputSchema: schema(map[string]any{"command": json.RawMessage(str)}, "command"),
		Annotations: Annotations{Kind: agentwire.ToolExec, Destructive: true, OpenWorld: true},
	}
}

var readSpec = ToolSpec{
	Name:        "read",
	Title:       "Read a file",
	Description: "Read a workspace file.",
	InputSchema: schema(map[string]any{"path": json.RawMessage(str)}, "path"),
	Annotations: Annotations{Kind: agentwire.ToolRead, ReadOnly: true},
}

var writeSpec = ToolSpec{
	Name:        "write",
	Title:       "Write a file",
	Description: "Write a workspace file (create or replace).",
	InputSchema: schema(map[string]any{"path": json.RawMessage(str), "content": json.RawMessage(str)}, "path", "content"),
	Annotations: Annotations{Kind: agentwire.ToolEdit, Destructive: true},
}

var editSpec = ToolSpec{
	Name:        "edit",
	Title:       "Edit a file",
	Description: "Replace one unique occurrence of old_string in a workspace file.",
	InputSchema: schema(map[string]any{
		"path": json.RawMessage(str), "old_string": json.RawMessage(str), "new_string": json.RawMessage(str),
	}, "path", "old_string", "new_string"),
	Annotations: Annotations{Kind: agentwire.ToolEdit, Destructive: true},
}

var globSpec = ToolSpec{
	Name:        "glob",
	Title:       "List files",
	Description: "List workspace files matching a glob pattern.",
	InputSchema: schema(map[string]any{"pattern": json.RawMessage(str)}, "pattern"),
	Annotations: Annotations{Kind: agentwire.ToolSearch, ReadOnly: true},
}

var grepSpec = ToolSpec{
	Name:        "grep",
	Title:       "Search files",
	Description: "Search workspace files for a regular expression.",
	InputSchema: schema(map[string]any{"pattern": json.RawMessage(str), "path": json.RawMessage(str)}, "pattern"),
	Annotations: Annotations{Kind: agentwire.ToolSearch, ReadOnly: true},
}

func runBash(ctx context.Context, opts builtinOptions, command string) (string, error) {
	if strings.TrimSpace(command) == "" {
		return "", errors.New("empty command")
	}
	cctx, cancel := context.WithTimeout(ctx, opts.BashTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "sh", "-c", command)
	cmd.Dir = opts.Root
	cmd.Env = opts.Env
	if len(cmd.Env) == 0 {
		cmd.Env = filteredEnv()
	}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	out := buf.String()
	if err != nil {
		return truncate(fmt.Sprintf("%s\n%v", out, err), 8000), nil
	}
	return truncate(out, 8000), nil
}

func runEdit(ctx context.Context, opts builtinOptions, in editInput) (string, error) {
	data, err := readFile(ctx, opts, in.Path)
	if err != nil {
		return "", err
	}
	s := string(data)
	switch n := strings.Count(s, in.OldString); n {
	case 0:
		return "", errors.New("old_string not found")
	case 1:
	default:
		return "", fmt.Errorf("old_string matched %d times; must be unique", n)
	}
	if err := writeFile(ctx, opts, in.Path, []byte(strings.Replace(s, in.OldString, in.NewString, 1))); err != nil {
		return "", err
	}
	return "edited " + in.Path, nil
}

func runGlob(opts builtinOptions, pattern string) (string, error) {
	if pattern == "" {
		pattern = "**/*"
	}
	var matches []string
	err := filepath.WalkDir(opts.Root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, rerr := filepath.Rel(opts.Root, path)
		if rerr != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		ok, _ := filepath.Match(pattern, rel)
		if !ok {
			ok, _ = filepath.Match(pattern, filepath.Base(rel))
		}
		if ok {
			matches = append(matches, rel)
		}
		if len(matches) >= 200 {
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "(no matches)", nil
	}
	return strings.Join(matches, "\n"), nil
}

func runGrep(opts builtinOptions, pattern, sub string) (string, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "", err
	}
	root := opts.Root
	if sub != "" {
		joined, jerr := joinRoot(opts.Root, sub)
		if jerr != nil {
			return "", jerr
		}
		root = joined
	}
	var hits []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			if d != nil && d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil || len(data) > 1<<20 {
			return nil
		}
		rel, _ := filepath.Rel(opts.Root, path)
		for i, line := range strings.Split(string(data), "\n") {
			if re.MatchString(line) {
				hits = append(hits, fmt.Sprintf("%s:%d:%s", rel, i+1, truncate(line, 200)))
				if len(hits) >= 100 {
					return fs.SkipAll
				}
			}
		}
		return nil
	})
	if len(hits) == 0 {
		return "(no matches)", nil
	}
	return strings.Join(hits, "\n"), nil
}

var (
	errEscapesRoot = errors.New("path escapes the workspace root")
	errTooLarge    = errors.New("file exceeds the size limit")
)

func readFile(ctx context.Context, opts builtinOptions, rel string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	joined, err := joinRoot(opts.Root, rel)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(joined)
	if err != nil {
		return nil, err
	}
	if st.Size() > opts.MaxFileBytes {
		return nil, errTooLarge
	}
	return os.ReadFile(joined)
}

func writeFile(ctx context.Context, opts builtinOptions, rel string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if int64(len(data)) > opts.MaxFileBytes {
		return errTooLarge
	}
	joined, err := joinRoot(opts.Root, rel)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(joined), 0o755); err != nil {
		return err
	}
	return os.WriteFile(joined, data, 0o644)
}

// joinRoot resolves rel inside root and refuses every escape.
func joinRoot(root, rel string) (string, error) {
	if strings.TrimSpace(rel) == "" {
		return "", errEscapesRoot
	}
	cleaned := filepath.Clean(rel)
	if filepath.IsAbs(cleaned) || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(os.PathSeparator)) {
		return "", errEscapesRoot
	}
	root = filepath.Clean(root)
	joined := filepath.Join(root, cleaned)
	if joined != root && !strings.HasPrefix(joined, root+string(os.PathSeparator)) {
		return "", errEscapesRoot
	}
	if joined == root {
		return "", errEscapesRoot
	}
	return joined, nil
}

// filteredEnv is the parent environment without provider keys.
func filteredEnv() []string {
	deny := map[string]bool{"ANTHROPIC_API_KEY": true, "OPENAI_API_KEY": true}
	var out []string
	for _, e := range os.Environ() {
		k, _, _ := strings.Cut(e, "=")
		if deny[k] {
			continue
		}
		out = append(out, e)
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
