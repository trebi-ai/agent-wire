//go:build live

package live

import (
	"context"
	"crypto/rand"
	"errors"
	"strings"
	"testing"

	"github.com/trebi-ai/agent-wire"
)

// liveResumeModels pins a cheap model for the resume test. A harness that is
// not in the map uses AGENTWIRE_LIVE_MODEL_<HARNESS> or the vendor default.
// Codex is not in the map, because its config can select a model provider
// that does not know the OpenAI model names.
var liveResumeModels = map[agentwire.Harness]string{
	agentwire.Claude: "haiku",
}

// TestLiveResume tells a session a code word, closes it, resumes it by id,
// and asks for the word. The reply proves that the harness kept the context.
func TestLiveResume(t *testing.T) {
	for _, h := range liveTestHarnesses {
		t.Run(string(h), func(t *testing.T) {
			if !liveEnabled(t, h) {
				return
			}
			rt := liveRuntime(t)
			liveTestReady(t, rt, h)
			word := liveResumeWord(t)

			req := liveTestRequest(h, liveWorkDir(t))
			if req.Model == "" {
				req.Model = liveResumeModels[h]
			}

			s := liveTestStart(t, rt, req)
			o := liveTestTurn(t, h, s, "Remember the code word "+word+". Reply with OK only.", liveTestTurnTimeout)
			liveResumeAssertResult(t, h, o)
			id := s.ID()
			if id == "" {
				id = o.result.Result.SessionID
			}
			if id == "" {
				t.Fatalf("live tier: %s: no session id after turn 1", h)
			}
			liveTestCloseSession(t, h, s, o)

			req.SessionID = id
			ctx, cancel := context.WithTimeout(context.Background(), liveTestStartTimeout)
			r, err := rt.Resume(ctx, req)
			cancel()
			if errors.Is(err, agentwire.ErrResumeUnsupported) {
				t.Skipf("live tier: %s does not resume: %v", h, err)
			}
			if err != nil {
				t.Fatalf("live tier: %s resume %q: %v", h, id, err)
			}
			t.Cleanup(func() { liveTestCloseSession(t, h, r, &liveTestObs{}) })

			o2 := liveTestTurn(t, h, r, "What is the code word? Reply with the word only.", liveTestTurnTimeout)
			liveResumeAssertResult(t, h, o2)
			reply := o2.text.String()
			if reply == "" {
				reply = o2.result.Result.Text
			}
			if !strings.Contains(strings.ToUpper(reply), word) {
				t.Errorf("live tier: %s: resumed reply %q does not hold the code word %s", h, reply, word)
			}
			if h == agentwire.Claude || h == agentwire.Codex {
				for i, u := range []agentwire.Usage{o.result.Result.Usage, o2.result.Result.Usage} {
					if u.Input+u.Output+u.CacheRead+u.CacheCreation == 0 {
						t.Errorf("live tier: %s: turn %d result has no usage", h, i+1)
					}
				}
			}
			t.Logf("live tier: %s: session %q resumed as %q, turn usage %+v then %+v",
				h, id, r.ID(), o.result.Result.Usage, o2.result.Result.Usage)
		})
	}
}

// liveResumeAssertResult checks that a turn ended with a result that is not
// an error.
func liveResumeAssertResult(t *testing.T, h agentwire.Harness, o *liveTestObs) {
	t.Helper()
	if o.result == nil || o.result.Result == nil {
		t.Fatalf("live tier: %s: no result (errors=%v)", h, o.errors)
	}
	if o.result.Result.IsError {
		t.Fatalf("live tier: %s: result is an error: subtype=%q text=%q", h, o.result.Result.Subtype, o.result.Result.Text)
	}
}

// liveResumeWord returns 8 random upper-case letters.
func liveResumeWord(t *testing.T) string {
	t.Helper()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("live tier: random word: %v", err)
	}
	for i := range b {
		b[i] = 'A' + b[i]%26
	}
	return string(b)
}
