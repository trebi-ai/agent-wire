//go:build live

package live

import (
	"context"
	"testing"
)

// TestLiveInterrupt asks for a long answer, interrupts the turn, and checks
// that the wire reaches a terminal event instead of hanging.
func TestLiveInterrupt(t *testing.T) {
	for _, h := range liveTestHarnesses {
		t.Run(string(h), func(t *testing.T) {
			if !liveEnabled(t, h) {
				return
			}
			rt := liveRuntime(t)
			liveTestReady(t, rt, h)
			s := liveTestStart(t, rt, liveTestRequest(h, liveWorkDir(t)))

			o := &liveTestObs{}
			liveTestPrompt(t, h, s, "Count from 1 to 200 slowly, one number per line.", liveTestTurnTimeout)
			liveTestRead(t, h, s, o, liveTestTurnTimeout, func(obs *liveTestObs) bool { return obs.assistant > 0 })

			ctx, cancel := context.WithTimeout(context.Background(), liveTestCloseTimeout)
			err := s.Interrupt(ctx)
			cancel()
			if err != nil {
				t.Fatalf("live tier: %s interrupt: %v", h, err)
			}

			liveTestRead(t, h, s, o, liveTestTurnTimeout, func(obs *liveTestObs) bool {
				return obs.result != nil || len(obs.errors) > 0 || obs.closed
			})
			if o.result == nil && len(o.errors) == 0 && !o.closed {
				t.Fatalf("live tier: %s: interrupt produced no terminal event", h)
			}

			// Close must return, and the event channel must close.
			liveTestCloseSession(t, h, s, o)
			if !o.closed {
				t.Fatalf("live tier: %s: Events did not close after Close", h)
			}
			t.Logf("live tier: %s interrupt: result=%v errors=%v", h, o.result != nil, o.errors)
		})
	}
}
