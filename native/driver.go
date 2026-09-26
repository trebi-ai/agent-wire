package native

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/trebi-ai/agent-wire"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Config is the consumer-side configuration of the native driver. The
// consumer supplies tools, MCP servers, skills, a permission policy, and
// provider credentials, as it does for Claude or Codex (plan 2026-09-26 I).
type Config struct {
	// NewModel builds the Model of one session from the credentials and the
	// model name. A provider module supplies the constructor.
	NewModel func(c agentwire.Credentials, model string) (Model, error)
	// Credentials resolves the provider credentials of one session. It is
	// called per session; StartRequest.Credentials wins when set. The
	// library never reads env or secrets for it.
	Credentials func() (agentwire.Credentials, error)
	// ToolSets are the consumer's own tools, next to the session's MCP
	// servers and the built-in file tools.
	ToolSets []ToolSet
	// BuiltinTools roots the built-in read, write, edit, glob, grep, and
	// bash tools at StartRequest.WorkingDir.
	BuiltinTools bool
	// Store persists the message log. Nil keeps no log and resume never
	// finds anything.
	Store Store
	// Compactor folds the message list when it outgrows the budget. Nil
	// disables compaction.
	Compactor Compactor
	// ContextTokens is the compaction budget.
	ContextTokens int
	// MaxTurns bounds one prompt. Zero uses DefaultMaxTurns. A cap ends the
	// turn with the max_turn_requests reason.
	MaxTurns int
	// MaxOutputTokens bounds one model call. Zero uses the provider
	// default.
	MaxOutputTokens int
	// BeforeCall runs before every model call. A non-nil error fails the
	// turn; a daily token cap goes here.
	BeforeCall func(ctx context.Context, sessionID string) error
	// AfterCall runs after every model call with its usage.
	AfterCall func(ctx context.Context, sessionID string, u Usage)
	// Approver replaces the default policy approver.
	Approver Approver
}

// Driver drives the in-process native harness. It implements
// agentwire.Driver and agentwire.Detector.
type Driver struct {
	cfg Config
	log logger

	mu       sync.Mutex
	toolSets []ToolSet // opened MCP sets, closed with the driver
	closed   bool
}

type logger interface {
	Info(msg string, args ...any)
	Error(msg string, args ...any)
}

// NewDriver builds the native driver.
func NewDriver(cfg Config) *Driver {
	return &Driver{cfg: cfg}
}

// Name implements agentwire.Driver.
func (d *Driver) Name() string { return string(agentwire.Native) }

// Detect implements agentwire.Detector: native is always in-process and
// supported; the auth state comes from the credential resolver and the
// model name.
func (d *Driver) Detect(_ context.Context) (agentwire.Detection, error) {
	det := agentwire.Detection{
		Harness:   agentwire.Native,
		Supported: true,
		Auth:      agentwire.AuthOK,
	}
	if d.cfg.Credentials != nil {
		c, err := d.cfg.Credentials()
		switch {
		case err != nil:
			det.Auth = agentwire.AuthMissing
			det.Detail = err.Error()
		case c.APIKey == "":
			det.Auth = agentwire.AuthMissing
			det.Detail = "no API key"
		}
	}
	return det, nil
}

// Start opens one native session. Resume is Start: the message log decides
// what the model sees.
func (d *Driver) Start(ctx context.Context, req agentwire.StartRequest) (agentwire.Session, error) {
	return d.open(ctx, req)
}

func (d *Driver) Resume(ctx context.Context, req agentwire.StartRequest) (agentwire.Session, error) {
	return d.open(ctx, req)
}

func (d *Driver) open(ctx context.Context, req agentwire.StartRequest) (agentwire.Session, error) {
	if req.SessionID == "" {
		return nil, errors.New("native session needs a session id")
	}
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil, errors.New("native driver is closed")
	}
	d.mu.Unlock()

	if d.cfg.NewModel == nil {
		return nil, errors.New("native driver needs NewModel")
	}
	creds := req.Credentials
	if creds == nil && d.cfg.Credentials != nil {
		c, err := d.cfg.Credentials()
		if err != nil {
			return nil, err
		}
		creds = &c
	}
	if creds == nil {
		return nil, errors.New("native session needs credentials")
	}
	model, err := d.cfg.NewModel(*creds, req.Model)
	if err != nil {
		return nil, err
	}

	tools, closeTools, err := d.assembleTools(ctx, req)
	if err != nil {
		return nil, err
	}

	skills, err := LoadSkills(req.Skills)
	if err != nil {
		closeTools()
		return nil, err
	}
	if act := skills.ActivateTool(); act != nil {
		tools = append(tools, nativeTool{tool: act, spec: act.Spec()})
	}

	system := strings.TrimSpace(req.Instructions)
	if p := skills.Prompt(); p != "" {
		if system != "" {
			system += "\n\n"
		}
		system += p
	}

	approver := d.cfg.Approver
	if approver == nil {
		approver = PolicyApprover{Policy: req.Permissions.Normalized()}
	}

	s := newSession(d, model, req.SessionID, sessionOpts{
		system:          system,
		maxTurns:        d.cfg.MaxTurns,
		maxOutputTokens: d.cfg.MaxOutputTokens,
		contextTokens:   d.cfg.ContextTokens,
		approver:        approver,
		beforeCall: func(ctx context.Context) error {
			if d.cfg.BeforeCall == nil {
				return nil
			}
			return d.cfg.BeforeCall(ctx, req.SessionID)
		},
		afterCall: func(ctx context.Context, u Usage) {
			if d.cfg.AfterCall != nil {
				d.cfg.AfterCall(ctx, req.SessionID, u)
			}
		},
	}, tools, skills)

	// The default approver: an ask the policy does not answer surfaces as
	// one EventPermission and the loop waits for AnswerPermission.
	if pa, ok := s.opts.approver.(PolicyApprover); ok && pa.Emit == nil {
		pa.Emit = func(ctx context.Context, r ApprovalRequest) (func(context.Context) (agentwire.Decision, error), func()) {
			return s.approvals.emit(r, func(id string, rr ApprovalRequest) {
				s.emit(agentwire.Event{Type: agentwire.EventPermission, SessionID: s.ID(), At: time.Now().UTC(),
					Permission: &agentwire.Permission{
						ID: id, Tool: rr.Spec.Name, Kind: rr.Spec.Annotations.Kind,
						Question: "Allow " + rr.Spec.Name + "?", Input: string(rr.Input),
					}})
			})
		}
		s.opts.approver = pa
	}

	// Resume: the store decides what the model sees.
	if d.cfg.Store != nil {
		msgs, lerr := d.cfg.Store.Load(ctx, req.SessionID)
		if lerr != nil && !errors.Is(lerr, ErrNotFound) {
			closeTools()
			return nil, lerr
		}
		s.mu.Lock()
		s.msgs = msgs
		s.mu.Unlock()
	}

	s.emit(s.initEvent())
	return s, nil
}

// assembleTools opens every ToolSet of one session: the consumer sets, one
// MCPToolSet per StartRequest.MCPServer, and the built-in file tools.
func (d *Driver) assembleTools(ctx context.Context, req agentwire.StartRequest) ([]nativeTool, func(), error) {
	var out []nativeTool
	var opened []ToolSet
	closeOpened := func() {
		for _, ts := range opened {
			_ = ts.Close()
		}
		d.mu.Lock()
		d.toolSets = removeFrom(d.toolSets, opened...)
		d.mu.Unlock()
	}
	fail := func(err error) ([]nativeTool, func(), error) {
		closeOpened()
		return nil, nil, err
	}

	sets := append([]ToolSet(nil), d.cfg.ToolSets...)
	for _, srv := range req.MCPServers {
		ts, err := MCPToolSet(ctx, srv, &mcp.Implementation{Name: "agentwire", Version: "native"})
		if err != nil {
			return fail(fmt.Errorf("native: mcp server %s: %w", srv.Name, err))
		}
		sets = append(sets, ts)
		opened = append(opened, ts)
	}
	if d.cfg.BuiltinTools {
		sets = append(sets, StaticTools("fs", newBuiltinTools(builtinOptions{
			Root: req.WorkingDir,
			Env:  envSlice(req.Env),
		})...))
	}
	for _, ts := range sets {
		tools, err := ts.Tools(ctx)
		if err != nil {
			return fail(fmt.Errorf("native: toolset %s: %w", ts.Name(), err))
		}
		for _, t := range tools {
			spec := t.Spec()
			out = append(out, nativeTool{tool: t, spec: spec})
		}
	}
	d.mu.Lock()
	d.toolSets = append(d.toolSets, opened...)
	d.mu.Unlock()
	return out, closeOpened, nil
}

// Close closes the driver and every MCP tool set it opened.
func (d *Driver) Close() error {
	d.mu.Lock()
	sets := d.toolSets
	d.toolSets = nil
	d.closed = true
	d.mu.Unlock()
	for _, ts := range sets {
		_ = ts.Close()
	}
	return nil
}

func removeFrom(list []ToolSet, gone ...ToolSet) []ToolSet {
	out := list[:0]
	for _, ts := range list {
		drop := false
		for _, g := range gone {
			if ts == g {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, ts)
		}
	}
	return out
}

func envSlice(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}
