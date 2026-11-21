package agentwire

import (
	"context"
	"encoding/base64"
	"fmt"

	"github.com/trebi-ai/agent-wire/internal/proc"
	"github.com/trebi-ai/agent-wire/internal/wire"
)

// wireConfig is the framing configuration for one session.
func (rt *Runtime) wireConfig(req StartRequest) wire.Config {
	return wire.Config{MaxFrameBytes: rt.maxFr, LogPath: req.LogPath, FragmentLogPath: req.FragmentLogPath, LogFragments: req.LogFragments}
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

// startWire starts the vendor process and its wire session, and attaches the
// launch cleanup.
func (rt *Runtime) startWire(ctx context.Context, h Harness, l *launch, req StartRequest, args []string, adapter wire.Adapter) (*wire.Session, error) {
	p, err := proc.Start(ctx, proc.Opts{
		Path: l.bin, Args: args, Dir: req.WorkingDir,
		Env: proc.ChildEnv(l.env), OnStderr: rt.stderrHook(h, req.OnStderr),
	})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", h, err)
	}
	s, err := wire.Start(ctx, string(h), p, adapter, rt.wireConfig(req))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", h, err)
	}
	l.attach(s)
	return s, nil
}

// encodeBase64 renders attachment bytes for a JSON content block.
func encodeBase64(data []byte) string { return base64.StdEncoding.EncodeToString(data) }
