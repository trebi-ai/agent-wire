//go:build live

package live

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trebi-ai/agent-wire"
)

// TestLiveModels lists the models of each harness, changes the model of a
// live session, and checks the next turn runs on the new model.
//
// AGENTWIRE_LIVE_SETMODEL_<HARNESS> picks the model SetModel switches to.
// Without it the test picks a listed model that is not the current one.
func TestLiveModels(t *testing.T) {
	for _, h := range liveTestHarnesses {
		t.Run(string(h), func(t *testing.T) {
			if !liveEnabled(t, h) {
				return
			}
			rt := liveRuntime(t)
			d := liveTestReady(t, rt, h)
			dir := liveWorkDir(t)

			ctx, cancel := context.WithTimeout(context.Background(), liveTestDetectTimeout)
			models, err := rt.Models(ctx, h, agentwire.ModelQuery{WorkingDir: dir})
			cancel()
			if !d.Features.Models {
				if !errors.Is(err, agentwire.ErrUnsupported) {
					t.Fatalf("live tier: %s: Features.Models is false, but Models returned %v", h, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("live tier: %s models: %v", h, err)
			}
			if len(models) == 0 {
				t.Fatalf("live tier: %s models: empty list", h)
			}
			t.Logf("live tier: %s lists %d models, default %q, first %+v", h, len(models), liveTestDefault(models), models[0])

			req := liveTestRequest(h, dir)
			req.LogPath = filepath.Join(t.TempDir(), "frames.ndjson")
			s := liveTestStart(t, rt, req)
			if sm, ok := s.(agentwire.SessionModels); ok {
				t.Logf("live tier: %s session offers %d models", h, len(sm.Models()))
			}
			setter, ok := s.(agentwire.ModelSetter)
			if !ok {
				if d.Features.SetModel {
					t.Fatalf("live tier: %s: Features.SetModel is true, but %T is not a ModelSetter", h, s)
				}
				return
			}
			target := liveTestSecondModel(h, models, req.Model)
			if target == "" {
				t.Skipf("live tier: %s lists no second model", h)
			}

			first := liveTestTurn(t, h, s, "Reply with exactly: ONE", liveTestTurnTimeout)
			liveTestAssertTurn(t, h, first)

			var offset int64
			if fi, err := os.Stat(req.LogPath); err == nil {
				offset = fi.Size()
			}
			ctx, cancel = context.WithTimeout(context.Background(), liveTestCloseTimeout)
			err = setter.SetModel(ctx, target)
			cancel()
			if errors.Is(err, agentwire.ErrUnsupported) {
				t.Skipf("live tier: %s: SetModel is not supported here: %v", h, err)
			}
			if err != nil {
				t.Fatalf("live tier: %s set model %q: %v", h, target, err)
			}

			second := liveTestTurn(t, h, s, "Reply with exactly: TWO", liveTestTurnTimeout)
			// Only the first turn of a session carries the init event.
			second.init = first.init
			liveTestAssertTurn(t, h, second)
			t.Logf("live tier: %s set model %q: first turn models %v, second turn models %v", h, target, first.models, second.models)
			if len(second.models) > 0 && !liveTestSameModel(target, second.models) {
				t.Fatalf("live tier: %s: second turn ran on %v, want %q", h, second.models, target)
			}
			// Claude names the model on every assistant frame.
			seen := liveTestFramesName(req.LogPath, offset, target)
			t.Logf("live tier: %s: vendor frames after SetModel name %q: %v", h, target, seen)
			if h == agentwire.Claude && !seen {
				t.Fatalf("live tier: %s: no vendor frame after SetModel names %q", h, target)
			}
		})
	}
}

// TestLiveSteer adds input while a turn runs a tool, and checks that one turn
// took both inputs.
func TestLiveSteer(t *testing.T) {
	for _, h := range liveTestHarnesses {
		t.Run(string(h), func(t *testing.T) {
			if !liveEnabled(t, h) {
				return
			}
			rt := liveRuntime(t)
			d := liveTestReady(t, rt, h)
			s := liveTestStart(t, rt, liveTestRequest(h, liveWorkDir(t)))
			st, ok := s.(agentwire.Steerer)
			if !d.Features.Steer {
				if ok {
					t.Logf("live tier: %s: Features.Steer is false, the session still has Steer", h)
				}
				t.Skipf("live tier: %s does not steer", h)
			}
			if !ok {
				t.Fatalf("live tier: %s: Features.Steer is true, but %T is not a Steerer", h, s)
			}

			ctx, cancel := context.WithTimeout(context.Background(), liveTestCloseTimeout)
			err := st.Steer(ctx, agentwire.Prompt{Text: "early"})
			cancel()
			if !errors.Is(err, agentwire.ErrNoActiveTurn) {
				t.Fatalf("live tier: %s steer while idle: %v, want ErrNoActiveTurn", h, err)
			}

			o := &liveTestObs{}
			liveTestPrompt(t, h, s, "Run the shell command `sleep 10; echo FIRST` and wait for it. Then reply with the output of the command.", liveTestTurnTimeout)
			liveTestRead(t, h, s, o, liveTestTurnTimeout, func(obs *liveTestObs) bool {
				return len(obs.tools) > 0 || obs.result != nil
			})
			if o.result != nil {
				t.Fatalf("live tier: %s: the turn ended before a tool started", h)
			}
			ctx, cancel = context.WithTimeout(context.Background(), liveTestCloseTimeout)
			err = st.Steer(ctx, agentwire.Prompt{Text: "Also end your final reply with the word BANANA."})
			cancel()
			if err != nil {
				t.Fatalf("live tier: %s steer: %v", h, err)
			}
			liveTestRead(t, h, s, o, liveTestTurnTimeout, func(obs *liveTestObs) bool { return obs.result != nil })
			liveTestAssertTurn(t, h, o)
			text := o.text.String()
			if !strings.Contains(strings.ToUpper(text), "BANANA") {
				t.Fatalf("live tier: %s: the turn did not take the steer input: %q", h, text)
			}
			// No second result may follow: the steer joined the running turn.
			extra := &liveTestObs{}
			deadline := time.After(5 * time.Second)
		wait:
			for {
				select {
				case ev, ok := <-s.Events():
					if !ok {
						break wait
					}
					extra.note(&ev)
				case <-deadline:
					break wait
				}
			}
			if extra.result != nil {
				t.Fatalf("live tier: %s: a second result followed the steer", h)
			}
		})
	}
}

// liveTestDefault returns the id of the default model.
func liveTestDefault(models []agentwire.ModelInfo) string {
	for _, m := range models {
		if m.Default {
			return m.ID
		}
	}
	return ""
}

// liveTestSecondModel picks the model SetModel switches to.
func liveTestSecondModel(h agentwire.Harness, models []agentwire.ModelInfo, current string) string {
	name := strings.ToUpper(strings.ReplaceAll(string(h), "-", "_"))
	if m := strings.TrimSpace(os.Getenv("AGENTWIRE_LIVE_SETMODEL_" + name)); m != "" {
		return m
	}
	if h == agentwire.Claude {
		return "haiku"
	}
	for _, m := range models {
		if !m.Default && m.ID != current && !strings.HasPrefix(m.ID, "default") {
			return m.ID
		}
	}
	return ""
}

// liveTestFramesName reports whether a frame the vendor sent after offset
// names the model in model or message.model.
func liveTestFramesName(path string, offset int64, model string) bool {
	data, err := os.ReadFile(path)
	if err != nil || int64(len(data)) < offset {
		return false
	}
	for _, line := range strings.Split(string(data[offset:]), "\n") {
		var entry struct {
			Dir   string `json:"dir"`
			Frame struct {
				Model   string `json:"model"`
				Message struct {
					Model string `json:"model"`
				} `json:"message"`
			} `json:"frame"`
		}
		if json.Unmarshal([]byte(line), &entry) != nil || entry.Dir != "in" {
			continue
		}
		if liveTestSameModel(model, []string{entry.Frame.Model, entry.Frame.Message.Model}) {
			return true
		}
	}
	return false
}

// liveTestSameModel reports whether a usage model name matches the target.
// Vendors report the resolved name, so a match on the last id segment or an
// alias inside the name counts.
func liveTestSameModel(target string, seen []string) bool {
	want := strings.ToLower(target)
	if i := strings.LastIndex(want, "/"); i >= 0 {
		want = want[i+1:]
	}
	for _, m := range seen {
		got := strings.ToLower(m)
		if i := strings.LastIndex(got, "/"); i >= 0 {
			got = got[i+1:]
		}
		if got != "" && (strings.Contains(got, want) || strings.Contains(want, got)) {
			return true
		}
	}
	return false
}
