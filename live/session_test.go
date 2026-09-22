//go:build live

package live

import (
	"strings"
	"testing"
)

// TestLiveSession opens one real session for every enabled harness and runs a
// trivial turn. It checks the event shape: an init, assistant text, a result
// that is not an error, and an exit event after Close.
func TestLiveSession(t *testing.T) {
	for _, h := range liveTestHarnesses {
		t.Run(string(h), func(t *testing.T) {
			if !liveEnabled(t, h) {
				return
			}
			rt := liveRuntime(t)
			d := liveTestReady(t, rt, h)
			t.Logf("live tier: %s version %q auth=%s authDetail=%q features=%+v",
				h, d.Version, d.Auth, d.AuthDetail, d.Features)

			s := liveTestStart(t, rt, liveTestRequest(h, liveWorkDir(t)))
			o := liveTestTurn(t, h, s, "Reply with exactly the word READY and nothing else.", liveTestTurnTimeout)
			liveTestAssertTurn(t, h, o)
			if !strings.Contains(strings.ToUpper(o.text.String()), "READY") {
				t.Errorf("live tier: %s: assistant text %q does not contain READY", h, o.text.String())
			}

			liveTestCloseSession(t, h, s, o)
			if o.exit == nil {
				t.Fatalf("live tier: %s: no exit event after Close", h)
			}
			t.Logf("live tier: %s exit code %v", h, o.exit.ExitCode)
		})
	}
}
