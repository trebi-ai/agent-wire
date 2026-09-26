// Package agentwire drives vendor coding-agent CLIs as child processes over
// their structured protocols: Claude stream-json, Codex app-server, OpenCode
// serve, ACP (Copilot, Cursor, Gemini) and pi RPC.
//
// One event model covers every harness. The library knows vendors; it does not
// know consumers. Vendor flags, env vars, config formats, protocol quirks and
// version floors live here. Homes such as ~/.trebi/harness, hooks, memory,
// secret env, job semantics and UI text belong to the caller.
//
// There is no global state: every cache, pool and ledger hangs off one
// Runtime, and two runtimes in one process share nothing.
//
// The wire is Go-native. Official vendor SDKs are the reference implementation
// and the fixture oracle at build and test time only; they are never a runtime
// dependency.
package agentwire

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/trebi-ai/agent-wire/internal/flight"
	"github.com/trebi-ai/agent-wire/internal/wire"
)

// Harness identifies one vendor coding agent.
type Harness string

const (
	Claude    Harness = "claude"
	Codex     Harness = "codex"
	OpenCode  Harness = "opencode"
	OpenCode2 Harness = "opencode2"
	Pi        Harness = "pi"
	Copilot   Harness = "copilot"
	Cursor    Harness = "cursor"
	Gemini    Harness = "gemini"
	Fake      Harness = "fake"
)

// Harnesses lists every harness the library can drive a wire for.
func Harnesses() []Harness {
	return []Harness{Claude, Codex, OpenCode, OpenCode2, Pi, Copilot, Cursor, Gemini, Fake}
}

// Options configures a Runtime.
type Options struct {
	// ClientName is the product name the harness sees in clientInfo and in
	// vendor client markers, for example "trebi" or "sling".
	ClientName string
	// ClientVersion is the product version reported next to ClientName.
	ClientVersion string
	// LedgerPath is the crash ledger file. Empty disables the ledger.
	LedgerPath string
	// Logger receives structured lifecycle lines. Nil discards them.
	Logger *slog.Logger
	// FingerprintIgnoreEnv lists environment keys left out of the OpenCode
	// server fingerprint, so per-run values do not split a shared server.
	FingerprintIgnoreEnv []string
	// MaxFrameBytes bounds one wire frame. Zero uses 16 MiB.
	MaxFrameBytes int
	// OnStderr receives live stderr lines from every child.
	OnStderr func(Harness, string)
	// AuthCacheTTL is how long a harness login probe answers from cache.
	// Zero uses 5 min. A negative value disables the cache.
	AuthCacheTTL time.Duration
}

// Session is one vendor conversation. The caller holds it for the life of the
// run.
//
// A caller MUST read Events until it closes, or call Close. A caller that
// stops reading without closing leaks the framing goroutine.
type Session interface {
	// Provider is the harness name.
	Provider() string
	// ID is the vendor session id; it may be empty until init.
	ID() string
	// PID is the child process id (0 for in-process sessions).
	PID() int
	// Prompt writes one user turn.
	Prompt(ctx context.Context, p Prompt) error
	// Interrupt asks the harness to stop the current turn.
	Interrupt(ctx context.Context) error
	// AnswerPermission routes a decision to the vendor.
	AnswerPermission(ctx context.Context, permissionID string, d Decision) error
	// InterruptTurn is the protocol-level interrupt. It is the same call as
	// Interrupt for every built-in driver, and is kept for callers that want
	// to be explicit about not falling back to a signal.
	InterruptTurn(ctx context.Context) error
	// Done is closed when the session has ended and Events is drained.
	Done() <-chan struct{}
	// Err is the first failure the session recorded, if any.
	Err() error
	// Close runs the graceful close sequence and waits for the drain.
	Close(ctx context.Context) error
	// Events is closed after the terminal events.
	Events() <-chan Event
}

// Driver starts sessions for one vendor protocol. The built-in drivers are
// unexported and reached through Runtime.Driver.
type Driver interface {
	Name() string
	Start(ctx context.Context, req StartRequest) (Session, error)
	Resume(ctx context.Context, req StartRequest) (Session, error)
}

// FileSystem is the optional file access an ACP agent may request. When it is
// set, the library advertises fs.readTextFile and fs.writeTextFile and routes
// those calls here, so a consumer can observe or approve every file write.
type FileSystem interface {
	ReadTextFile(ctx context.Context, path string) (string, error)
	WriteTextFile(ctx context.Context, path, content string) error
}

// StartRequest describes one session. The caller describes; the library builds
// the command.
type StartRequest struct {
	Harness Harness
	// Binary is the executable. Empty searches PATH.
	Binary     string
	WorkingDir string
	// Env is added to the parent environment and wins on conflict.
	Env map[string]string
	// Home is an optional isolated config home. Empty uses the user's own
	// configuration, including their login, MCP servers and skills.
	Home string

	// SessionID is a resume target, or a pre-assigned id where the harness
	// allows one.
	SessionID string
	// Model is the vendor model name. Empty uses the vendor default.
	Model string
	// Effort is one of low, medium, high, max. Empty uses the vendor default.
	Effort string

	Permissions  PermissionPolicy
	MCPServers   []MCPServer
	Skills       []Skill
	Instructions string

	// OneShot writes OneShotPrompt and closes stdin, so the child exits after
	// the turn. Use it for a job with no follow-up surface.
	OneShot       bool
	OneShotPrompt string
	// HandshakeTimeout overrides the protocol handshake wait.
	HandshakeTimeout time.Duration

	// ExtraArgs is appended verbatim after the library's own arguments. Use
	// it for consumer flags such as --settings or -c notify=...
	ExtraArgs []string
	// BaseCommand is an expert override: when it is set the launch builder is
	// skipped and the command is used as-is.
	BaseCommand []string
	// Raw carries per-harness overrides the typed fields do not cover:
	// "strict_mcp" (bool, Claude), "acp_auth_method" (string), "approvalPolicy"
	// and "sandbox" (string, Codex), "auth_method" (string, ACP).
	Raw map[string]any

	// LogPath writes every frame in and out as NDJSON. Secrets are not
	// redacted, so the file is created mode 0600.
	LogPath string
	// FS is the optional ACP file-system adapter.
	FS FileSystem
	// OnStderr receives live stderr lines from this child.
	OnStderr func(Harness, string)
}

// RawBool reads a boolean override.
func (r StartRequest) RawBool(key string) (bool, bool) {
	v, ok := r.Raw[key]
	if !ok {
		return false, false
	}
	b, ok := v.(bool)
	return b, ok
}

// RawString reads a string override.
func (r StartRequest) RawString(key string) string {
	s, _ := r.Raw[key].(string)
	return s
}

// Runtime owns every piece of per-process state: the driver table, the
// OpenCode server pool, version caches, the crash ledger and the id counter.
type Runtime struct {
	opts  Options
	log   *slog.Logger
	led   *Registry
	oc    *openCodeManager
	vers  *versionCache
	maxFr int

	// auth caches login probes per harness and binary, with single-flight on
	// a miss. A probe spawns a process; Detect must stay cheap.
	authMu   sync.Mutex
	auth     map[authKey]authEntry
	authFlt  flight.Group[authKey, authEntry]
	authTTL  time.Duration

	mu      sync.RWMutex
	drivers map[Harness]Driver
	// builtin is the table New installs. SetDriver(h, nil) restores from it.
	builtin map[Harness]Driver
	closed  bool

	// reqSeq numbers wire request ids. It is per runtime, so two runtimes in
	// one process never share an id space.
	reqSeq atomic.Int64
}

// New builds a runtime.
func New(opts Options) *Runtime {
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	rt := &Runtime{
		opts:    opts,
		log:     log,
		maxFr:   opts.MaxFrameBytes,
		vers:    newVersionCache(),
		auth:    map[authKey]authEntry{},
		authTTL: opts.AuthCacheTTL,
	}
	if rt.maxFr <= 0 {
		rt.maxFr = wire.DefaultMaxFrameBytes
	}
	if opts.LedgerPath != "" {
		rt.led = NewRegistry(opts.LedgerPath)
	}
	rt.oc = newOpenCodeManager(rt)
	rt.builtin = map[Harness]Driver{
		Claude:    claudeDriver{rt},
		Codex:     codexDriver{rt},
		OpenCode:  openCodeDriver{rt},
		OpenCode2: openCode2Driver{rt},
		Pi:        piDriver{rt},
		Copilot:   acpDriver{rt, acpCopilot},
		Cursor:    acpDriver{rt, acpCursor},
		Gemini:    acpDriver{rt, acpGemini},
		Fake:      FakeDriver{},
	}
	rt.drivers = map[Harness]Driver{}
	for h, d := range rt.builtin {
		rt.drivers[h] = d
	}
	return rt
}

// Logger is the runtime logger.
func (rt *Runtime) Logger() *slog.Logger { return rt.log }

// MaxFrameBytes is the effective frame cap.
func (rt *Runtime) MaxFrameBytes() int { return rt.maxFr }

// Driver resolves the driver for a harness. ok=false means the harness has no
// proven wire.
func (rt *Runtime) Driver(h Harness) (Driver, bool) {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	d, ok := rt.drivers[h]
	return d, ok
}

// SetDriver replaces the driver for a harness. Tests use it to inject a
// scripted driver. nil drops the override and restores the built-in driver, so
// a caller can always get back to the default table.
func (rt *Runtime) SetDriver(h Harness, d Driver) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if d == nil {
		if builtin, ok := rt.builtin[h]; ok {
			rt.drivers[h] = builtin
			return
		}
		delete(rt.drivers, h)
		return
	}
	rt.drivers[h] = d
}

// Start opens a new session and waits for the protocol handshake. A handshake
// failure is returned as an error; the child is killed first.
func (rt *Runtime) Start(ctx context.Context, req StartRequest) (Session, error) {
	return rt.session(ctx, req, false)
}

// Resume reopens a known session. It returns ErrResumeUnsupported when the
// vendor no longer knows the session, or the protocol has no resume.
func (rt *Runtime) Resume(ctx context.Context, req StartRequest) (Session, error) {
	return rt.session(ctx, req, true)
}

func (rt *Runtime) session(ctx context.Context, req StartRequest, resume bool) (Session, error) {
	if req.Harness == "" {
		req.Harness = Claude
	}
	d, ok := rt.Driver(req.Harness)
	if !ok {
		return nil, fmt.Errorf("agentwire: no driver for harness %q", req.Harness)
	}
	var (
		s   Session
		err error
	)
	if resume {
		s, err = d.Resume(ctx, req)
	} else {
		s, err = d.Start(ctx, req)
	}
	if err != nil {
		return nil, err
	}
	// The runtime owns the ledger, so it tracks every session it hands out.
	// A driver that never calls back into the runtime is covered too.
	rt.track(s)
	return s, nil
}

// Reconcile kills leftover children from a previous process and drops the
// ledger. Only positively matched pids die: a reused pid is left alone.
func (rt *Runtime) Reconcile() error {
	if rt.led != nil {
		killed, _ := rt.led.Reconcile()
		if len(killed) > 0 {
			rt.log.Info("reconciled stale children", "count", len(killed), "ids", killed)
		}
	}
	// The ledger covers every child this runtime spawned, including the shared
	// OpenCode servers, so one reconcile pass reaps all of them.
	return nil
}

// Close stops every live session and shared server.
func (rt *Runtime) Close(ctx context.Context) error {
	rt.mu.Lock()
	if rt.closed {
		rt.mu.Unlock()
		return nil
	}
	rt.closed = true
	rt.mu.Unlock()
	if rt.led != nil {
		rt.led.CloseAll(ctx)
	}
	rt.oc.shutdown()
	return nil
}

// ErrResumeUnsupported reports a driver that cannot resume a session (the
// vendor no longer knows it, or the protocol has no resume).
var ErrResumeUnsupported = errors.New("agentwire: session resume not supported")

// ErrUnsupported reports a capability the harness does not have.
var ErrUnsupported = wire.ErrUnsupported
