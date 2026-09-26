package native

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/trebi-ai/agent-wire"
)

// SkillMeta is one catalog entry: enough for the model to decide whether to
// activate the skill.
type SkillMeta struct {
	Name string
	// Description is the frontmatter description.
	Description string
	// Dir is the absolute skill directory.
	Dir string
	// AllowedTools lists tools the skill's frontmatter pre-approves.
	AllowedTools []string
}

// SkillCatalog is the progressive-disclosure surface: the prompt carries the
// catalog only, and the activate_skill tool loads the body on demand
// (agentskills.io, plan 2026-09-26 I.5).
type SkillCatalog struct {
	Skills []SkillMeta
}

// LoadSkills reads the frontmatter of every skill.
func LoadSkills(skills []agentwire.Skill) (SkillCatalog, error) {
	var out SkillCatalog
	for _, s := range skills {
		if s.Dir == "" {
			return out, fmt.Errorf("native: skill %q has no directory", s.Name)
		}
		body, err := os.ReadFile(filepath.Join(s.Dir, "SKILL.md"))
		if err != nil {
			return out, fmt.Errorf("native: skill %s: %w", s.Name, err)
		}
		front := parseFrontmatter(string(body))
		name := s.Name
		if name == "" {
			name = front["name"]
		}
		if name == "" {
			name = filepath.Base(strings.TrimRight(s.Dir, string(filepath.Separator)))
		}
		meta := SkillMeta{
			Name:        name,
			Description: strings.TrimSpace(front["description"]),
			Dir:         s.Dir,
		}
		if list := strings.TrimSpace(front["allowed-tools"]); list != "" {
			for _, tool := range strings.Split(list, ",") {
				if tool = strings.TrimSpace(tool); tool != "" {
					meta.AllowedTools = append(meta.AllowedTools, tool)
				}
			}
		}
		out.Skills = append(out.Skills, meta)
	}
	return out, nil
}

// Prompt renders the <available_skills> block for the system prompt.
func (c SkillCatalog) Prompt() string {
	if len(c.Skills) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("<available_skills>\n")
	b.WriteString("Each entry names a task area. Before you work in that area, activate the skill with the activate_skill tool and follow it.\n")
	for _, s := range c.Skills {
		fmt.Fprintf(&b, "<skill>\n<name>%s</name>\n<description>%s</description>\n</skill>\n", s.Name, s.Description)
	}
	b.WriteString("</available_skills>\n")
	return b.String()
}

// PreApproved lists the tools of every skill's allowed-tools frontmatter.
func (c SkillCatalog) PreApproved() []string {
	var out []string
	for _, s := range c.Skills {
		out = append(out, s.AllowedTools...)
	}
	return out
}

// ActivateTool returns the activate_skill tool: it loads one SKILL.md body
// and lists the skill directory's resources.
func (c SkillCatalog) ActivateTool() Tool {
	return NewFunc(activateSpec, func(ctx context.Context, in activateInput) (activateOutput, error) {
		for _, s := range c.Skills {
			if s.Name != in.Name {
				continue
			}
			body, err := os.ReadFile(filepath.Join(s.Dir, "SKILL.md"))
			if err != nil {
				return activateOutput{}, fmt.Errorf("skill %s: %w", s.Name, err)
			}
			out := activateOutput{Content: string(body)}
			entries, _ := os.ReadDir(s.Dir)
			for _, e := range entries {
				if e.IsDir() || e.Name() == "SKILL.md" {
					continue
				}
				out.Resources = append(out.Resources, e.Name())
			}
			return out, nil
		}
		return activateOutput{}, fmt.Errorf("unknown skill %q", in.Name)
	})
}

type activateInput struct {
	Name string `json:"name"`
}

type activateOutput struct {
	// Content is the SKILL.md body.
	Content string `json:"content"`
	// Resources names the other files of the skill directory.
	Resources []string `json:"resources,omitempty"`
}

var activateSpec = ToolSpec{
	Name:        "activate_skill",
	Title:       "Activate a skill",
	Description: "Load the full SKILL.md of one skill. Activate a skill before you work in its task area.",
	InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string","description":"The skill name"}},"required":["name"]}`),
	Annotations: Annotations{ReadOnly: true, Kind: agentwire.ToolRead},
}

// parseFrontmatter reads the leading YAML frontmatter block of a document
// into a flat string map. Values stay unquoted strings; the skill
// frontmatter carries no nesting.
func parseFrontmatter(text string) map[string]string {
	out := map[string]string{}
	lines := strings.Split(text, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return out
	}
	for _, line := range lines[1:] {
		if strings.TrimSpace(line) == "---" {
			break
		}
		idx := strings.Index(line, ":")
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		val = strings.Trim(val, `"'`)
		out[key] = val
	}
	return out
}
