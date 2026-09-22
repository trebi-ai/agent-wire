package agentwire

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// injTestWriteSkill creates one skill directory with a SKILL.md file.
func injTestWriteSkill(t *testing.T, name, body string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o600); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
	return dir
}

// injTestPlainSkill creates one skill with the given name and description.
func injTestPlainSkill(t *testing.T, name, description string) string {
	t.Helper()
	return injTestWriteSkill(t, name, "---\nname: "+name+"\ndescription: "+description+"\n---\n# "+name+"\n")
}

// injTestSymlinkTo asserts that link is a symlink that resolves to target.
func injTestSymlinkTo(t *testing.T, link, target string) {
	t.Helper()
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat %s: %v", link, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s is not a symlink (mode %s)", link, info.Mode())
	}
	got, err := filepath.EvalSymlinks(link)
	if err != nil {
		t.Fatalf("resolve %s: %v", link, err)
	}
	want, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatalf("resolve %s: %v", target, err)
	}
	if got != want {
		t.Fatalf("%s resolves to %s, want %s", link, got, want)
	}
}

// TestSkillFrontmatterParser checks the YAML subset the SKILL.md files use.
func TestSkillFrontmatterParser(t *testing.T) {
	tests := []struct {
		name string
		text string
		want map[string]string
		// lenient skips the exact key-count check, for documents that carry a
		// key the parser records with an empty value (a nested mapping).
		lenient bool
	}{
		{
			name: "single line",
			text: "---\nname: my-skill\ndescription: A short description.\n---\nbody\n",
			want: map[string]string{"name": "my-skill", "description": "A short description."},
		},
		{
			name: "folded block",
			text: "---\nname: folded\ndescription: >\n  first line\n  second line\n---\nbody\n",
			want: map[string]string{"name": "folded", "description": "first line second line\n"},
		},
		{
			name: "literal block",
			text: "---\ndescription: |\n  line one\n  line two\nname: literal\n---\n",
			want: map[string]string{"name": "literal", "description": "line one\nline two\n"},
		},
		{
			name: "double quotes",
			text: "---\nname: \"quoted name\"\ndescription: \"say \\\"hi\\\"\"\n---\n",
			want: map[string]string{"name": "quoted name", "description": `say "hi"`},
		},
		{
			name: "single quotes",
			text: "---\nname: 'single quoted'\ndescription: 'it''s here'\n---\n",
			want: map[string]string{"name": "single quoted", "description": "it's here"},
		},
		{
			name: "trailing comment",
			text: "---\nname: my-skill # for the session\ndescription: plain\n---\n",
			want: map[string]string{"name": "my-skill", "description": "plain"},
		},
		{
			name:    "nested mapping",
			text:    "---\nname: nested\nmeta:\n  owner: team\n  tags:\n    - a\ndescription: after nested\n---\n",
			want:    map[string]string{"name": "nested", "description": "after nested"},
			lenient: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseFrontmatter(tt.text)
			if !tt.lenient && len(got) != len(tt.want) {
				t.Fatalf("parseFrontmatter = %v, want %v", got, tt.want)
			}
			for k, want := range tt.want {
				if got[k] != want {
					t.Fatalf("parseFrontmatter[%q] = %q, want %q", k, got[k], want)
				}
			}
		})
	}
	// A document with no frontmatter yields no keys.
	if got := parseFrontmatter("# just a body\n"); len(got) != 0 {
		t.Fatalf("parseFrontmatter without a block = %v, want empty", got)
	}
}

// TestSkillLoad checks name resolution and the missing-file error.
func TestSkillLoad(t *testing.T) {
	dir := injTestPlainSkill(t, "from-front", "the description")

	loaded, err := loadSkills([]Skill{{Dir: dir}})
	if err != nil {
		t.Fatalf("loadSkills: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("loaded %d skills, want 1", len(loaded))
	}
	if loaded[0].Name != "from-front" {
		t.Fatalf("Name = %q, want from-front", loaded[0].Name)
	}
	if loaded[0].Description != "the description" {
		t.Fatalf("Description = %q", loaded[0].Description)
	}
	if loaded[0].Dir != dir {
		t.Fatalf("Dir = %q, want %q", loaded[0].Dir, dir)
	}

	overridden, err := loadSkills([]Skill{{Name: "explicit", Dir: dir}})
	if err != nil {
		t.Fatalf("loadSkills with explicit name: %v", err)
	}
	if overridden[0].Name != "explicit" {
		t.Fatalf("explicit Name = %q, want explicit", overridden[0].Name)
	}

	noName := injTestWriteSkill(t, "dir-name-wins", "---\ndescription: d\n---\n")
	fallback, err := loadSkills([]Skill{{Dir: noName}})
	if err != nil {
		t.Fatalf("loadSkills without a name: %v", err)
	}
	if fallback[0].Name != "dir-name-wins" {
		t.Fatalf("fallback Name = %q, want dir-name-wins", fallback[0].Name)
	}

	missing := filepath.Join(t.TempDir(), "no-skill-md")
	if err := os.MkdirAll(missing, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSkills([]Skill{{Dir: missing}}); err == nil {
		t.Fatal("loadSkills with no SKILL.md returned no error")
	} else if !strings.Contains(err.Error(), missing) {
		t.Fatalf("error %q does not name the directory %q", err, missing)
	}
}

// TestSkillStrategies checks the per-harness injection and the instruction
// index fallback.
func TestSkillStrategies(t *testing.T) {
	ctx := context.Background()
	first := injTestPlainSkill(t, "alpha", "first skill text")
	second := injTestPlainSkill(t, "beta", "second skill text")
	skills := []Skill{{Dir: first}, {Dir: second}}

	t.Run("claude plugin", func(t *testing.T) {
		rt := injTestRuntime(t)
		req := StartRequest{Harness: Claude, Binary: "/bin/sh", Skills: skills}
		l, err := rt.prepare(ctx, &req, false)
		if err != nil {
			t.Fatalf("prepare: %v", err)
		}
		defer l.cleanup()

		dir, ok := injTestArgValue(l.args, "--plugin-dir")
		if !ok {
			t.Fatalf("no --plugin-dir in %q", l.args)
		}
		meta := filepath.Join(dir, ".claude-plugin", "plugin.json")
		raw, err := os.ReadFile(meta)
		if err != nil {
			t.Fatalf("read %s: %v", meta, err)
		}
		var plugin struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &plugin); err != nil {
			t.Fatalf("plugin.json is not valid JSON: %v", err)
		}
		if plugin.Name == "" {
			t.Fatalf("plugin.json has no name: %s", raw)
		}
		injTestSymlinkTo(t, filepath.Join(dir, "skills", "alpha"), first)
		injTestSymlinkTo(t, filepath.Join(dir, "skills", "beta"), second)
		if l.instructions != "" {
			t.Fatalf("claude kept an instruction index: %q", l.instructions)
		}
	})

	t.Run("codex home", func(t *testing.T) {
		rt := injTestRuntime(t)
		home := t.TempDir()
		req := StartRequest{Harness: Codex, Binary: "/bin/sh", Home: home, Skills: skills}
		l, err := rt.prepare(ctx, &req, false)
		if err != nil {
			t.Fatalf("prepare: %v", err)
		}
		defer l.cleanup()
		injTestSymlinkTo(t, filepath.Join(home, "skills", "alpha"), first)
		injTestSymlinkTo(t, filepath.Join(home, "skills", "beta"), second)
		if l.instructions != "" {
			t.Fatalf("codex with Home kept an instruction index: %q", l.instructions)
		}
	})

	t.Run("codex instructions fallback", func(t *testing.T) {
		rt := injTestRuntime(t)
		req := StartRequest{Harness: Codex, Binary: "/bin/sh", Skills: skills}
		l, err := rt.prepare(ctx, &req, false)
		if err != nil {
			t.Fatalf("prepare: %v", err)
		}
		defer l.cleanup()
		if l.instructions == "" {
			t.Fatal("codex without Home returned no instruction index")
		}
		for _, s := range skills {
			loaded, err := loadSkills([]Skill{s})
			if err != nil {
				t.Fatal(err)
			}
			name := loaded[0].Name
			if !strings.Contains(l.instructions, name) {
				t.Fatalf("index does not name %q: %q", name, l.instructions)
			}
			if !strings.Contains(l.instructions, loaded[0].Description) {
				t.Fatalf("index does not describe %q: %q", name, l.instructions)
			}
			if !strings.Contains(l.instructions, filepath.Join(s.Dir, "SKILL.md")) {
				t.Fatalf("index does not carry the SKILL.md path of %q: %q", name, l.instructions)
			}
		}
	})

	t.Run("opencode home", func(t *testing.T) {
		rt := injTestRuntime(t)
		home := t.TempDir()
		req := StartRequest{Harness: OpenCode, Binary: "/bin/sh", Home: home, Skills: skills}
		l, err := rt.prepare(ctx, &req, false)
		if err != nil {
			t.Fatalf("prepare: %v", err)
		}
		defer l.cleanup()
		injTestSymlinkTo(t, filepath.Join(home, "skill", "alpha"), first)
		injTestSymlinkTo(t, filepath.Join(home, "skill", "beta"), second)
		if l.instructions != "" {
			t.Fatalf("opencode with Home kept an instruction index: %q", l.instructions)
		}
	})

	t.Run("index rule text", func(t *testing.T) {
		loaded, err := loadSkills(skills)
		if err != nil {
			t.Fatal(err)
		}
		index := skillIndex(Codex, loaded)
		if !strings.Contains(index, "SKILL.md") {
			t.Fatalf("index does not name the file: %q", index)
		}
		if !strings.Contains(index, "Read the file before you do a task") {
			t.Fatalf("index does not tell the agent to read it first: %q", index)
		}
	})
}
