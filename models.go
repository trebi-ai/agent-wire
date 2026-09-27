package agentwire

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/trebi-ai/agent-wire/internal/proc"
	"github.com/trebi-ai/agent-wire/internal/wire"
)

// ModelInfo is one model a harness offers.
type ModelInfo struct {
	// ID is the value StartRequest.Model and SetModel take.
	ID string `json:"id"`
	// Name is the display name. Empty means use ID.
	Name string `json:"name,omitempty"`
	// Description is a short vendor note, such as the context size.
	Description string `json:"description,omitempty"`
	// Default marks the model the harness uses when Model is empty.
	Default bool `json:"default,omitempty"`
}

// ModelQuery scopes a model list to one install and one config home.
type ModelQuery struct {
	Binary     string            // empty searches PATH
	Home       string            // same meaning as StartRequest.Home
	Env        map[string]string // same meaning as StartRequest.Env
	WorkingDir string            // some vendors read project config
}

// ModelLister is the optional driver capability that lists models without a
// live session.
type ModelLister interface {
	Models(ctx context.Context, q ModelQuery) ([]ModelInfo, error)
}

// ModelSetter is the optional session capability that changes the model of a
// live session. It never interrupts the running turn.
type ModelSetter interface {
	SetModel(ctx context.Context, model string) error
}

// SessionModels is the optional session capability that returns the models
// the live session offered in its handshake (Claude initialize, ACP
// session/new). It returns nil when the handshake offered none.
type SessionModels interface {
	Models() []ModelInfo
}

// Steerer is the optional session capability that adds input to the running
// turn. It returns ErrNoActiveTurn when no turn runs; the caller then uses
// Prompt. It returns ErrUnsupported when the vendor refused the steer; the
// caller then queues the input. It never interrupts the turn.
type Steerer interface {
	Steer(ctx context.Context, p Prompt) error
}

// ErrNoActiveTurn reports a Steer with no running turn.
var ErrNoActiveTurn = errors.New("agentwire: no active turn")

// ErrTurnActive reports a Prompt while a turn runs on a harness that cannot
// take a second prompt then. The caller waits for the result, or steers.
var ErrTurnActive = errors.New("agentwire: a turn is active")

// Model list timings.
const (
	defaultModelTTL = 10 * time.Minute
	// modelErrorTTL keeps a failed probe short, so a login fixes the list fast.
	modelErrorTTL = 30 * time.Second
	// modelProbeTimeout bounds a probe when the caller set no deadline.
	modelProbeTimeout = 20 * time.Second
	// modelReplyWait bounds a live-session model or steer request.
	modelReplyWait = 10 * time.Second
)

// modelKey names one cached model list.
type modelKey struct {
	harness Harness
	bin     string
	version string
	home    string
}

// modelEntry is one cached model list or probe failure.
type modelEntry struct {
	at     time.Time
	models []ModelInfo
	err    error
}

// fresh reports whether the entry still answers under ttl.
func (e modelEntry) fresh(ttl time.Duration) bool {
	if e.err != nil {
		ttl = min(ttl, modelErrorTTL)
	}
	return time.Since(e.at) < ttl
}

// Models lists the models of one harness. It answers from a cache; the first
// call can start a short-lived child. A harness with no lister returns
// ErrUnsupported.
func (rt *Runtime) Models(ctx context.Context, h Harness, q ModelQuery) ([]ModelInfo, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	d, ok := rt.Driver(h)
	if !ok {
		return nil, fmt.Errorf("agentwire: no driver for harness %q", h)
	}
	lister, ok := d.(ModelLister)
	if !ok {
		return nil, fmt.Errorf("%w: %s lists no models", ErrUnsupported, h)
	}
	ttl := rt.opts.ModelCacheTTL
	if ttl == 0 {
		ttl = defaultModelTTL
	}
	probe := func() ([]ModelInfo, error) {
		probeCtx := ctx
		if _, ok := ctx.Deadline(); !ok {
			var cancel context.CancelFunc
			probeCtx, cancel = context.WithTimeout(ctx, modelProbeTimeout)
			defer cancel()
		}
		return lister.Models(probeCtx, q)
	}
	if ttl < 0 {
		models, err := probe()
		return cloneModels(models), err
	}
	key := modelKey{harness: h, bin: q.Binary, home: q.Home}
	if key.bin == "" {
		key.bin, _ = exec.LookPath(binName(h))
	}
	if key.bin != "" {
		key.version = rt.Version(ctx, key.bin)
	}
	rt.modelMu.Lock()
	e, ok := rt.models[key]
	rt.modelMu.Unlock()
	if !ok || !e.fresh(ttl) {
		e, _ = rt.modelFlt.Do(key, func() (modelEntry, error) {
			rt.modelMu.Lock()
			cached, ok := rt.models[key]
			rt.modelMu.Unlock()
			if ok && cached.fresh(ttl) {
				return cached, nil
			}
			models, err := probe()
			e := modelEntry{at: time.Now(), models: models, err: err}
			rt.modelMu.Lock()
			rt.models[key] = e
			rt.modelMu.Unlock()
			return e, nil
		})
	}
	if e.err != nil {
		return nil, e.err
	}
	return cloneModels(e.models), nil
}

// InvalidateModels drops the cached lists of one harness, for example after
// a login or a config change.
func (rt *Runtime) InvalidateModels(h Harness) {
	rt.modelMu.Lock()
	for key := range rt.models {
		if key.harness == h {
			delete(rt.models, key)
		}
	}
	rt.modelMu.Unlock()
}

func cloneModels(models []ModelInfo) []ModelInfo {
	if models == nil {
		return nil
	}
	return append([]ModelInfo{}, models...)
}

// probeRequest is the start request a model probe prepares its child from.
func probeRequest(h Harness, q ModelQuery, dir string) StartRequest {
	return StartRequest{
		Harness: h, Binary: q.Binary, Home: q.Home, Env: q.Env, WorkingDir: dir,
		Permissions: PermissionPolicy{Mode: PermissionInherit},
	}
}

// probeDir is the working directory of a probe: the query's own, or a temp
// directory the returned func removes.
func probeDir(q ModelQuery) (string, func(), error) {
	if q.WorkingDir != "" {
		return q.WorkingDir, func() {}, nil
	}
	dir, err := os.MkdirTemp("", "agentwire-models-")
	if err != nil {
		return "", nil, err
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}

// probeAdapter is the wire adapter of a model probe: one request exchange
// with a short-lived child, and no events.
type probeAdapter struct {
	run   func(ctx context.Context, w *wire.Writer) error
	parse func(line []byte)
}

func (a probeAdapter) Handshake(ctx context.Context, w *wire.Writer) ([]Event, error) {
	return nil, a.run(ctx, w)
}

func (probeAdapter) EncodePrompt(Prompt) ([]byte, error) { return nil, ErrUnsupported }

func (a probeAdapter) Parse(line []byte) []Event {
	a.parse(line)
	return nil
}

func (probeAdapter) EncodeDecision(string, Decision) ([]byte, error) { return nil, ErrUnsupported }

func (probeAdapter) Exit(int, error, string) []Event { return nil }

// runProbe starts one child, runs the adapter exchange, and kills the child.
func (rt *Runtime) runProbe(ctx context.Context, h Harness, bin string, args []string, dir string, env map[string]string, a probeAdapter) error {
	p, err := proc.Start(ctx, proc.Opts{
		Path: bin, Args: args, Dir: dir, Env: proc.ChildEnv(env),
		OnStderr: rt.stderrHook(h, nil),
	})
	if err != nil {
		return fmt.Errorf("%s models: %w", h, err)
	}
	s, err := wire.Start(ctx, string(h), p, a, rt.wireConfig(""))
	if err != nil {
		if tail := proc.FirstLine(p.Stderr()); tail != "" {
			return fmt.Errorf("%s models: %w (%s)", h, err, tail)
		}
		return fmt.Errorf("%s models: %w", h, err)
	}
	_ = s.KillTree()
	return nil
}
