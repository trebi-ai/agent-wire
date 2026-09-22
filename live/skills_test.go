//go:build live

package live

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trebi-ai/agent-wire"
)

// TestLiveSkills injects one skill with a unique marker phrase and checks that
// the marker reaches the assistant text. A harness without native skill
// loading gets the instructions fallback, so the check runs everywhere.
func TestLiveSkills(t *testing.T) {
	for _, h := range liveTestHarnesses {
		t.Run(string(h), func(t *testing.T) {
			if !liveEnabled(t, h) {
				return
			}
			rt := liveRuntime(t)
			d := liveTestReady(t, rt, h)
			if d.Features.SkillsInject == agentwire.SkillNone {
				t.Skipf("live tier: %s takes no injected skill", h)
			}

			marker := liveTestMarker + liveTestToken(t)
			req := liveTestRequest(h, liveWorkDir(t))
			req.Skills = []agentwire.Skill{{
				Name: liveTestSkillName,
				Dir:  liveTestWriteSkill(t, liveTestSkillName, marker),
			}}
			t.Logf("live tier: %s skills=%s marker=%s", h, d.Features.SkillsInject, marker)

			s := liveTestStart(t, rt, req)
			prompt := fmt.Sprintf("Use the skill named %q. Read it, then reply with the marker phrase it contains, exactly and alone.", liveTestSkillName)
			o := liveTestTurn(t, h, s, prompt, liveTestSkillTimeout)
			liveTestAssertTurn(t, h, o)
			if !strings.Contains(o.text.String(), marker) {
				t.Errorf("live tier: %s: assistant text %q does not contain the skill marker %q", h, o.text.String(), marker)
			}
			liveTestCloseSession(t, h, s, o)
		})
	}
}

// liveTestWriteSkill writes one SKILL.md that carries the marker phrase and
// returns its directory.
func liveTestWriteSkill(t *testing.T, name, marker string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("live tier: skill dir: %v", err)
	}
	body := "---\n" +
		"name: " + name + "\n" +
		"description: A live tier probe. Use it when the operator asks for the marker phrase.\n" +
		"---\n\n" +
		"# Live tier marker\n\n" +
		"The marker phrase is " + marker + ".\n\n" +
		"When the operator asks for the marker phrase, reply with exactly that phrase and nothing else.\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o600); err != nil {
		t.Fatalf("live tier: skill file: %v", err)
	}
	return dir
}
