package agentwire

import (
	"encoding/base64"

	"github.com/trebi-ai/agent-wire/internal/wire"
)

// wireConfig is the framing configuration for one session.
func (rt *Runtime) wireConfig(logPath string) wire.Config {
	return wire.Config{MaxFrameBytes: rt.maxFr, LogPath: logPath}
}

// stderrHook combines the runtime-level stderr hook with a request-level one.
// The request hook runs first.
func (rt *Runtime) stderrHook(h Harness, perRequest func(Harness, string)) func(string) {
	runtimeHook := rt.opts.OnStderr
	if perRequest == nil && runtimeHook == nil {
		return nil
	}
	return func(line string) {
		if perRequest != nil {
			perRequest(h, line)
		}
		if runtimeHook != nil {
			runtimeHook(h, line)
		}
	}
}

// bindSession attaches the launch cleanup to a freshly started session. The
// runtime tracks the session itself, so a session from a consumer-supplied
// Driver reaches the crash ledger too.
func (rt *Runtime) bindSession(s Session, l *launch) {
	if l != nil {
		l.attach(s)
	}
}

// encodeBase64 renders attachment bytes for a JSON content block.
func encodeBase64(data []byte) string { return base64.StdEncoding.EncodeToString(data) }
