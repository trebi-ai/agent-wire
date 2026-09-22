package agentwire

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Skill is one extra skill made visible to a session.
type Skill struct {
	// Name overrides the name in the SKILL.md frontmatter.
	Name string
	// Dir is the absolute path of the directory that holds SKILL.md.
	Dir string
}

// SkillSupport reports how a harness takes extra skills for one session.
type SkillSupport string

const (
	// SkillNone means the harness gets no extra skills.
	SkillNone SkillSupport = "none"
	// SkillNative means the harness loads the skills itself, from a directory
	// the library generates or points at.
	SkillNative SkillSupport = "native"
	// SkillOverlay means the library links the skills into a per-session
	// config directory the harness already reads.
	SkillOverlay SkillSupport = "overlay"
	// SkillInstructions means the skills are described in the session
	// instructions and the agent reads them from disk.
	SkillInstructions SkillSupport = "instructions"
)

// applySkills makes the requested skills visible and returns the instruction
// index. The index is non-empty only for the instructions fallback; a harness
// that loads the skills itself does not need the agent told about them.
func (rt *Runtime) applySkills(ctx context.Context, req *StartRequest, l *launch) (string, error) {
	if len(req.Skills) == 0 {
		return "", nil
	}
	loaded, err := loadSkills(req.Skills)
	if err != nil {
		return "", err
	}
	switch req.Harness {
	case Claude:
		dir, err := rt.sessionDir(req, "plugin")
		if err != nil {
			return "", err
		}
		l.tmp = append(l.tmp, dir)
		if err := writeClaudePlugin(dir, loaded); err != nil {
			return "", err
		}
		l.args = append(l.args, "--plugin-dir", dir)
		return "", nil
	case Codex:
		if req.Home == "" {
			// The consumer chose the user's own CODEX_HOME, so the library
			// must not graft a skills directory into it.
			return skillIndex(req.Harness, loaded), nil
		}
		if err := linkSkills(filepath.Join(req.Home, "skills"), loaded); err != nil {
			return "", err
		}
		return "", nil
	case OpenCode, OpenCode2:
		if req.Home == "" {
			return skillIndex(req.Harness, loaded), nil
		}
		if err := linkSkills(filepath.Join(req.Home, "skill"), loaded); err != nil {
			return "", err
		}
		return "", nil
	case Pi:
		if rt.supportsFlag(ctx, l.bin, "--skill") {
			for _, s := range loaded {
				l.args = append(l.args, "--skill", s.Dir)
			}
			return "", nil
		}
		if req.Home == "" {
			return skillIndex(req.Harness, loaded), nil
		}
		if err := linkSkills(filepath.Join(req.Home, "skills"), loaded); err != nil {
			return "", err
		}
		return "", nil
	}
	return skillIndex(req.Harness, loaded), nil
}

// loadedSkill is a skill with its resolved name and description.
type loadedSkill struct {
	Skill
	Description string
}

// loadSkills reads the frontmatter of every skill.
func loadSkills(skills []Skill) ([]loadedSkill, error) {
	out := make([]loadedSkill, 0, len(skills))
	for _, s := range skills {
		if s.Dir == "" {
			return nil, fmt.Errorf("agentwire: skill with no directory")
		}
		path := filepath.Join(s.Dir, "SKILL.md")
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("agentwire: skill %s: %w", s.Dir, err)
		}
		if info.IsDir() {
			return nil, fmt.Errorf("agentwire: skill %s: SKILL.md is a directory", s.Dir)
		}
		text, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("agentwire: skill %s: %w", s.Dir, err)
		}
		front := parseFrontmatter(string(text))
		name := s.Name
		if name == "" {
			name = front["name"]
		}
		if name == "" {
			name = filepath.Base(strings.TrimRight(s.Dir, string(filepath.Separator)))
		}
		out = append(out, loadedSkill{
			Skill:       Skill{Name: name, Dir: s.Dir},
			Description: strings.TrimSpace(front["description"]),
		})
	}
	return out, nil
}

// skillIndex renders the instructions fallback: enough for the agent to find
// the skill and the rule to read it first.
func skillIndex(h Harness, skills []loadedSkill) string {
	var b strings.Builder
	b.WriteString("Skills available for this session. Each entry names a task area. ")
	b.WriteString("Read the file before you do a task that matches it.\n")
	for _, s := range skills {
		b.WriteString("\n- ")
		b.WriteString(s.Name)
		if s.Description != "" {
			b.WriteString(": ")
			b.WriteString(oneLine(s.Description))
		}
		b.WriteString("\n  file: ")
		b.WriteString(filepath.Join(s.Dir, "SKILL.md"))
		b.WriteString("\n")
	}
	return b.String()
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// writeClaudePlugin writes a generated Claude Code plugin whose skills are
// symlinks to the consumer's skill directories. The plugin loads next to the
// user's own skills instead of replacing them.
func writeClaudePlugin(dir string, skills []loadedSkill) error {
	metaDir := filepath.Join(dir, ".claude-plugin")
	if err := os.MkdirAll(metaDir, 0o700); err != nil {
		return err
	}
	if err := writeJSONFile(filepath.Join(metaDir, "plugin.json"), map[string]any{
		"name":        "agentwire-session",
		"description": "Skills injected by agentwire for this session.",
		"version":     "0.0.0",
	}); err != nil {
		return err
	}
	return linkSkills(filepath.Join(dir, "skills"), skills)
}

// linkSkills points name → skill directory inside a directory the harness
// reads. An existing entry is replaced, so a repeated session is idempotent.
func linkSkills(root string, skills []loadedSkill) error {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("agentwire: create skills dir: %w", err)
	}
	for _, s := range skills {
		target := filepath.Join(root, s.Name)
		if err := os.RemoveAll(target); err != nil {
			return err
		}
		if err := os.Symlink(s.Dir, target); err != nil {
			return fmt.Errorf("agentwire: link skill %s: %w", s.Name, err)
		}
	}
	return nil
}

// parseFrontmatter reads the leading YAML frontmatter block of a document. It
// supports the subset SKILL.md files use: flat `key: value` pairs, quoted
// values, and the `>`/`|` block scalars that multi-line descriptions need.
// Nested mappings are skipped rather than mis-parsed.
func parseFrontmatter(text string) map[string]string {
	out := map[string]string{}
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return out
	}
	for i := 1; i < len(lines); i++ {
		line := lines[i]
		if strings.TrimSpace(line) == "---" {
			break
		}
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			// Part of a nested block already handled by its parent key.
			continue
		}
		key, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value := strings.TrimSpace(rest)
		switch value {
		case ">", ">-", ">+", "|", "|-", "|+":
			block, next := readBlockScalar(lines, i+1, value[0])
			out[key] = block
			i = next - 1
		default:
			out[key] = unquoteYAML(value)
		}
	}
	return out
}

// readBlockScalar consumes an indented block and returns it with the block
// style applied. It returns the index of the first line after the block.
func readBlockScalar(lines []string, start int, style byte) (string, int) {
	indent := -1
	var body []string
	i := start
	for ; i < len(lines); i++ {
		line := lines[i]
		if strings.TrimSpace(line) == "" {
			body = append(body, "")
			continue
		}
		lead := leadingSpaces(line)
		if indent < 0 {
			if lead == 0 {
				break
			}
			indent = lead
		}
		if lead < indent {
			break
		}
		body = append(body, line[indent:])
	}
	// Trailing blank lines belong to the block only when the block continues.
	for len(body) > 0 && body[len(body)-1] == "" {
		body = body[:len(body)-1]
	}
	if style == '|' {
		return strings.Join(body, "\n") + "\n", i
	}
	// Folded: a blank line becomes a newline, a single break becomes a space.
	var b strings.Builder
	for j, line := range body {
		switch {
		case j == 0:
			b.WriteString(line)
		case line == "":
			b.WriteString("\n")
		case body[j-1] == "":
			b.WriteString(line)
		default:
			b.WriteString(" ")
			b.WriteString(line)
		}
	}
	if b.Len() > 0 {
		b.WriteString("\n")
	}
	return b.String(), i
}

func leadingSpaces(s string) int {
	n := 0
	for n < len(s) && s[n] == ' ' {
		n++
	}
	return n
}

// unquoteYAML removes matching quotes and drops a trailing comment from an
// unquoted scalar.
func unquoteYAML(v string) string {
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			inner := v[1 : len(v)-1]
			if v[0] == '"' {
				inner = strings.ReplaceAll(inner, `\"`, `"`)
				inner = strings.ReplaceAll(inner, `\\`, `\`)
			} else {
				inner = strings.ReplaceAll(inner, "''", "'")
			}
			return inner
		}
	}
	if i := strings.Index(v, " #"); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}
