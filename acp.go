package agentwire

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/trebi-ai/agent-wire/internal/jsonrpc"
	"github.com/trebi-ai/agent-wire/internal/proc"
	"github.com/trebi-ai/agent-wire/internal/wire"
)

// acpKind distinguishes the ACP agents that share this driver.
//
// Copilot runs `copilot --acp`. Gemini runs `gemini --experimental-acp`, or
// `--acp` in newer releases; the launch builder picks the flag the installed
// binary advertises. Cursor runs `cursor-agent acp`, a hidden subcommand that
// `--help` does not list, and a new session mints a chat id with
// `cursor-agent create-chat` first, because the agent has no method that opens
// a chat the caller does not already know. omp runs `omp acp`, and grok runs
// `grok agent --no-leader stdio`; both are configured through session config
// options, not the `models` block.
type acpKind string

const (
	acpCopilot acpKind = "copilot"
	acpCursor  acpKind = "cursor"
	acpGemini  acpKind = "gemini"
	acpOMP     acpKind = "omp"
	acpGrok    acpKind = "grok"
)

// harness is the Agentwire harness name of the kind.
func (k acpKind) harness() Harness {
	switch k {
	case acpCopilot:
		return Copilot
	case acpCursor:
		return Cursor
	case acpGemini:
		return Gemini
	case acpOMP:
		return OMP
	case acpGrok:
		return Grok
	}
	return Harness(k)
}

// neverAuthenticate reports a kind whose authenticate method starts a browser
// or device login. The library never starts one from a session open: the
// session fails with a login error and the caller shows "Log in".
func (k acpKind) neverAuthenticate() bool {
	return k == acpGrok
}

// effortOption reports a kind that takes the effort level as a session config
// option. omp takes it as the launch flag --thinking instead.
func (k acpKind) effortOption() bool {
	return k == acpGrok
}

const acpHandshakeTimeout = 45 * time.Second

// acpDriver is the Agent Client Protocol driver shared by Copilot, Cursor,
// Gemini, omp and grok.
type acpDriver struct {
	rt   *Runtime
	kind acpKind
}

func (d acpDriver) Name() string { return string(d.kind.harness()) }

func (d acpDriver) Start(ctx context.Context, req StartRequest) (Session, error) {
	return d.start(ctx, req, false)
}

func (d acpDriver) Resume(ctx context.Context, req StartRequest) (Session, error) {
	return d.start(ctx, req, true)
}

func (d acpDriver) start(ctx context.Context, req StartRequest, resume bool) (Session, error) {
	fresh := false
	if d.kind == acpCursor && !resume {
		// A Cursor session starts as a fresh chat: the id must exist before the
		// agent can load it, and it is never a resume of an older chat.
		id, err := d.createCursorChat(ctx, req.Binary)
		if err != nil {
			return nil, err
		}
		req.SessionID = id
		fresh = true
	}
	l, err := d.rt.prepare(ctx, &req, resume)
	if err != nil {
		return nil, err
	}
	args, err := d.acpArgs(ctx, l, req, resume)
	if err != nil {
		return nil, err
	}
	proto := newACPProtocol(d.rt, d.kind, req, l, fresh, resume)
	p, err := proc.Start(ctx, proc.Opts{
		Path: l.bin, Args: args, Dir: req.WorkingDir,
		Env: proc.ChildEnv(l.env), OnStderr: d.rt.stderrHook(d.kind.harness(), req.OnStderr),
	})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", d.kind, err)
	}
	s, err := wire.Start(ctx, string(d.kind.harness()), p, proto, d.rt.wireConfig(req.LogPath))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", d.kind, err)
	}
	d.rt.bindSession(s, l)
	if id := proto.currentSession(); id != "" {
		s.SetID(id)
	}
	if req.OneShot && req.OneShotPrompt != "" {
		if err := s.Prompt(ctx, Prompt{Text: req.OneShotPrompt, Kind: "job"}); err != nil {
			_ = s.KillTree()
			return nil, err
		}
		_ = p.CloseStdin()
	}
	return &acpSession{Session: s, p: proto}, nil
}

// acpSession adds the model calls to the wire session.
type acpSession struct {
	*wire.Session
	p *acpProtocol
}

// Models returns the models the session open offered, with Default on the
// current model.
func (s *acpSession) Models() []ModelInfo { return s.p.sessionModels() }

// SetModel sends session/set_model. It returns ErrUnsupported when the agent
// offered no model list.
func (s *acpSession) SetModel(ctx context.Context, model string) error {
	s.p.mu.Lock()
	offered := len(s.p.models) > 0
	s.p.mu.Unlock()
	if !offered {
		return fmt.Errorf("%w: %s offered no models", ErrUnsupported, s.p.kind)
	}
	return s.p.applyModel(ctx, model)
}

// Steer sends input into the running turn. The ACP wire carries steering as a
// protocol extension; an agent without it answers with a plain message, which
// becomes ErrUnsupported.
func (s *acpSession) Steer(ctx context.Context, pr Prompt) error {
	return s.p.Steer(ctx, pr)
}

// Models implements ModelLister. It opens a session with no MCP servers,
// reads the model list, and kills the child. It never calls authenticate, so
// a probe cannot start a login flow.
func (d acpDriver) Models(ctx context.Context, q ModelQuery) ([]ModelInfo, error) {
	dir, done, err := probeDir(q)
	if err != nil {
		return nil, err
	}
	defer done()
	h := d.kind.harness()
	req := probeRequest(h, q, dir)
	l, err := d.rt.prepare(ctx, &req, false)
	if err != nil {
		return nil, err
	}
	defer l.cleanup()
	args, err := d.acpArgs(ctx, l, req, false)
	if err != nil {
		return nil, err
	}
	proto := newACPProtocol(d.rt, d.kind, req, l, false, false)
	a := probeAdapter{
		run: func(ctx context.Context, w *wire.Writer) error {
			proto.SetWriter(w)
			if _, err := proto.initialize(ctx); err != nil {
				return err
			}
			if proto.offersModels() {
				// grok lists its models on initialize, so the probe needs no
				// session and no login.
				return nil
			}
			_, err := proto.newSession(ctx)
			return err
		},
		parse: func(line []byte) { proto.client.Handle(line) },
	}
	if err := d.rt.runProbe(ctx, h, l.bin, args, dir, l.env, a); err != nil {
		return nil, err
	}
	models := proto.sessionModels()
	if len(models) == 0 {
		return nil, fmt.Errorf("%w: %s offered no models", ErrUnsupported, d.kind)
	}
	return models, nil
}

// acpArgs adds the subcommand or flag that selects the ACP transport.
func (d acpDriver) acpArgs(ctx context.Context, l *launch, req StartRequest, resume bool) ([]string, error) {
	switch d.kind {
	case acpCopilot:
		args := append([]string{}, l.args...)
		if !hasFlag(args, "--acp") {
			args = append(args, "--acp")
		}
		return args, nil
	case acpCursor:
		// The acp subcommand replaces the run-mode flags, and the session id
		// travels on the wire (create-chat plus session/load).
		args := append([]string{}, stripCursorArgs(l.args)...)
		return append(args, "acp"), nil
	case acpGemini:
		args := append([]string{}, l.args...)
		help := d.rt.HelpText(ctx, l.bin)
		switch {
		case strings.Contains(help, "--experimental-acp"):
			args = append(args, "--experimental-acp")
		case strings.Contains(help, "--acp"):
			args = append(args, "--acp")
		default:
			// The documented form. A binary without it fails loudly in the
			// handshake, which is better than silently opening a TUI.
			args = append(args, "--experimental-acp")
		}
		return args, nil
	case acpOMP:
		// omp acp parses every root flag after the subcommand. Without
		// --allow-home, omp moves a session started in the user's home to a
		// temp dir. The session id travels on the wire, so the pane flags do
		// not go to the ACP child.
		args := []string{"acp"}
		args = append(args, stripACPSessionArgs(l.args)...)
		if isUserHome(req.WorkingDir) && !hasFlag(args, "--allow-home") {
			args = append(args, "--allow-home")
		}
		return args, nil
	case acpGrok:
		// Agent flags sit between `agent` and the `stdio` mode name. A shared
		// leader process outlives the session, so --no-leader is the default;
		// Raw["grok_leader"] keeps it for an expert caller.
		args := []string{"agent"}
		args = append(args, stripACPSessionArgs(l.args)...)
		if keepLeader, _ := req.RawBool("grok_leader"); !keepLeader && !hasFlag(args, "--no-leader") {
			args = append(args, "--no-leader")
		}
		return append(args, "stdio"), nil
	}
	return append([]string{}, l.args...), nil
}

// createCursorChat mints a chat id with `cursor-agent create-chat`.
func (d acpDriver) createCursorChat(ctx context.Context, binary string) (string, error) {
	bin := binary
	if bin == "" {
		resolved, err := resolveBinary(&StartRequest{Harness: Cursor})
		if err != nil {
			return "", err
		}
		bin = resolved
	}
	runCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(runCtx, bin, "create-chat").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("cursor create-chat: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	id := lastIdentifier(string(out))
	if id == "" {
		return "", errors.New("cursor create-chat returned no chat id")
	}
	return id, nil
}

// isUserHome reports whether dir is the user's home directory.
func isUserHome(dir string) bool {
	if dir == "" {
		return false
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return false
	}
	return filepath.Clean(dir) == filepath.Clean(home)
}

// lastIdentifier picks the last uuid-shaped token in text, else the last
// non-empty line.
func lastIdentifier(text string) string {
	lines := strings.Split(text, "\n")
	last := ""
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		last = line
		for _, field := range strings.Fields(line) {
			field = strings.Trim(field, `"'.,`)
			if looksLikeUUID(field) {
				return field
			}
		}
	}
	return strings.Trim(last, `"' `)
}

func looksLikeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// stripACPSessionArgs removes the pane session flags. The ACP wire opens or
// resumes the session with session/new, session/resume or session/load, and
// the agent subcommand does not accept the pane form.
func stripACPSessionArgs(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		name, _, hasValue := strings.Cut(args[i], "=")
		switch name {
		case "--continue", "-c":
		case "--session-id", "-s", "--resume", "-r", "--restore":
			// The --resume value is optional, so a flag after it stays.
			if !hasValue && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
			}
		default:
			out = append(out, args[i])
		}
	}
	return out
}

// stripCursorArgs removes run-mode flags that conflict with the acp
// subcommand.
func stripCursorArgs(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--resume" || a == "--continue" || a == "-c":
			i++
		case strings.HasPrefix(a, "--resume=") || strings.HasPrefix(a, "--continue="):
		default:
			out = append(out, a)
		}
	}
	return out
}

// acpOption is one permission option the agent offers.
type acpOption struct {
	ID   string
	Kind string
}

// acpProtocol maps Agent Client Protocol JSON-RPC to events.
type acpProtocol struct {
	client *jsonrpc.Client
	kind   acpKind
	policy PermissionPolicy
	fs     FileSystem

	mu sync.Mutex
	w  *wire.Writer

	handshakeTimeout time.Duration
	cwd              string
	model            string
	effort           string
	clientName       string
	clientVersion    string
	mcpServers       []MCPServer
	instructions     string
	// rules is the instruction text grok takes in session/new _meta.rules.
	// It is not prepended to the first prompt.
	rules        string
	autoMode     bool
	authMethod   string
	trustHTTPMCP bool

	resumeID     string
	freshSession bool
	sessionID    string
	firstPrompt  bool

	promptID    string
	sawResult   bool
	approvals   map[string][]acpOption
	authMethods []string
	// steering reports the top-level _meta.steering.supported of initialize.
	steering bool

	// models is the list the session open offered; currentModel is the model
	// the agent reports as current. configOptions is the whole option list,
	// which carries the model and thought-level ids for a live change.
	models        []ModelInfo
	currentModel  string
	configOptions []acpConfigOption
}

func newACPProtocol(rt *Runtime, kind acpKind, req StartRequest, l *launch, fresh, resume bool) *acpProtocol {
	// Start opens a new session. Only Cursor needs a client-chosen id,
	// because create-chat mints the chat before the handshake. Every other
	// ACP agent assigns its own id on session/new.
	resumeID := req.SessionID
	if !resume && kind != acpCursor {
		resumeID = ""
	}
	timeout := req.HandshakeTimeout
	if timeout <= 0 {
		timeout = acpHandshakeTimeout
	}
	name := rt.clientName()
	autoMode, _ := req.RawBool("grok_auto_mode")
	p := &acpProtocol{
		kind:             kind,
		policy:           req.Permissions.Normalized(),
		fs:               req.FS,
		handshakeTimeout: timeout,
		cwd:              req.WorkingDir,
		model:            req.Model,
		effort:           req.Effort,
		clientName:       name,
		clientVersion:    rt.opts.ClientVersion,
		mcpServers:       req.MCPServers,
		instructions:     l.instructions,
		autoMode:         autoMode,
		authMethod:       req.RawString("acp_auth_method"),
		resumeID:         resumeID,
		freshSession:     fresh,
		approvals:        map[string][]acpOption{},
	}
	if kind == acpGrok {
		// grok takes the instruction text as session/new _meta.rules, so it
		// must not also ride in the first prompt.
		p.rules = strings.TrimSpace(l.instructions)
		p.instructions = ""
	}
	p.firstPrompt = strings.TrimSpace(p.instructions) != ""
	p.client = jsonrpc.New(nil, timeout)
	return p
}

func (p *acpProtocol) SetWriter(w *wire.Writer) {
	p.mu.Lock()
	p.w = w
	p.mu.Unlock()
	p.client.SetWrite(w.Write)
}

// Handshake negotiates capabilities and opens or loads a session.
func (p *acpProtocol) Handshake(ctx context.Context, _ *wire.Writer) ([]Event, error) {
	if _, err := p.initialize(ctx); err != nil {
		return nil, err
	}
	events := p.openOrLoad(ctx)
	if len(events) == 0 {
		return nil, errors.New(string(p.kind) + ": handshake produced no session")
	}
	if events[0].Type == EventError {
		return nil, errors.New(events[0].Error)
	}
	if p.model != "" {
		// The agent may take a model it did not list, so the call goes out
		// even without an offered list.
		if err := p.applyModel(ctx, p.model); err != nil {
			events = append(events, Event{Type: EventStatus, Status: StatusRunning, Text: "model not applied: " + err.Error()})
		} else {
			events = append(events, Event{Type: EventStatus, Status: StatusRunning, Text: "model " + p.model})
		}
	}
	if p.effort != "" && p.kind.effortOption() {
		// A model change can reset the effort, so it goes after the model.
		switch value, err := p.applyEffort(ctx, p.effort); {
		case err != nil:
			events = append(events, Event{Type: EventStatus, Status: StatusRunning, Text: "effort not applied: " + err.Error()})
		case value == "":
			events = append(events, Event{Type: EventStatus, Status: StatusRunning, Text: "effort " + p.effort + " not offered"})
		default:
			events = append(events, Event{Type: EventStatus, Status: StatusRunning, Text: "effort " + value})
		}
	}
	return events, nil
}

// initialize negotiates capabilities and records the auth methods.
func (p *acpProtocol) initialize(ctx context.Context) (json.RawMessage, error) {
	caps := map[string]any{
		"terminal": false,
		"auth":     map[string]any{"terminal": false},
	}
	if p.fs != nil {
		// Advertise the file system so every agent write can be observed or
		// approved by the consumer.
		caps["fs"] = map[string]any{"readTextFile": true, "writeTextFile": true}
	} else {
		caps["fs"] = map[string]any{"readTextFile": false, "writeTextFile": false}
	}
	raw, err := p.client.Call(ctx, "initialize", map[string]any{
		"protocolVersion":    1,
		"clientCapabilities": caps,
		"clientInfo":         map[string]any{"name": p.clientName, "version": p.clientVersion},
	}, p.handshakeTimeout)
	if err != nil {
		return nil, err
	}
	var init struct {
		AuthMethods       []map[string]any `json:"authMethods"`
		AgentCapabilities struct {
			MCPCapabilities struct {
				HTTP bool `json:"http"`
			} `json:"mcpCapabilities"`
		} `json:"agentCapabilities"`
		Meta struct {
			Steering struct {
				Supported bool `json:"supported"`
			} `json:"steering"`
		} `json:"_meta"`
	}
	_ = json.Unmarshal(raw, &init)
	p.mu.Lock()
	for _, m := range init.AuthMethods {
		if id := str(m["id"]); id != "" {
			p.authMethods = append(p.authMethods, id)
		}
	}
	p.trustHTTPMCP = init.AgentCapabilities.MCPCapabilities.HTTP
	p.steering = init.Meta.Steering.Supported
	p.mu.Unlock()
	if p.kind == acpGrok {
		// grok lists the models on initialize, before a session and before a
		// login, so the model probe needs neither.
		p.noteModelState(raw)
	}
	return raw, nil
}

// openOrLoad resumes a known session or opens a new one, authenticating once
// when the agent demands it. A kind whose authenticate starts a browser or
// device login never authenticates here: the session fails with the vendor's
// login error, which the caller shows as "Log in".
func (p *acpProtocol) openOrLoad(ctx context.Context) []Event {
	if p.resumeID != "" {
		p.mu.Lock()
		p.sessionID = p.resumeID
		p.mu.Unlock()
		events, err := p.loadSession(ctx)
		if err == nil {
			return events
		}
		if !errors.Is(err, ErrResumeUnsupported) {
			return []Event{{Type: EventError, Error: err.Error(), Code: "resume_failed", EndReason: string(FailProtocol)}}
		}
	}
	events, err := p.newSession(ctx)
	if err != nil && !p.kind.neverAuthenticate() {
		if authErr := p.authenticate(ctx); authErr == nil {
			events, err = p.newSession(ctx)
		} else if p.resumeID != "" {
			// The session may exist but need the login first.
			if retry, retryErr := p.loadSession(ctx); retryErr == nil {
				return retry
			}
		}
	}
	if err != nil {
		return sessionOpenFailure(p.kind.harness(), err)
	}
	return events
}

// sessionOpenFailure maps a failed session open to one error event. A login
// error keeps the auth family, so a caller offers "Log in" instead of a
// protocol failure.
func sessionOpenFailure(h Harness, err error) []Event {
	text := err.Error()
	c := ClassifyFor(h, text, 0)
	if c.Class == FailAuth {
		return []Event{{Type: EventError, Error: text, Code: c.Code, EndReason: string(FailAuth)}}
	}
	return []Event{{Type: EventError, Error: text, Code: "session_open_failed", EndReason: string(FailProtocol)}}
}

// sessionParams renders the session-open parameters, including the injected
// MCP servers and the per-kind _meta block.
func (p *acpProtocol) sessionParams() map[string]any {
	params := map[string]any{"cwd": p.cwd}
	p.mu.Lock()
	httpOK := p.trustHTTPMCP
	p.mu.Unlock()
	servers := acpMCPServers(p.mcpServers, httpOK)
	if servers == nil {
		servers = []map[string]any{}
	}
	params["mcpServers"] = servers
	if meta := p.sessionMeta(); meta != nil {
		params["_meta"] = meta
	}
	return params
}

// sessionMeta renders the per-kind _meta of session/new, session/resume and
// session/load. _meta is vendor data, so every field is gated per kind.
func (p *acpProtocol) sessionMeta() map[string]any {
	if p.kind != acpGrok {
		return nil
	}
	p.mu.Lock()
	rules := p.rules
	mode := p.policy.Normalized().Mode
	auto := p.autoMode
	p.mu.Unlock()
	meta := map[string]any{}
	if rules != "" {
		meta["rules"] = rules
	}
	if mode == PermissionAuto {
		meta["yoloMode"] = true
	}
	if auto {
		meta["autoMode"] = true
	}
	if len(meta) == 0 {
		return nil
	}
	return meta
}

func (p *acpProtocol) newSession(ctx context.Context) ([]Event, error) {
	raw, err := p.client.Call(ctx, "session/new", p.sessionParams(), p.handshakeTimeout)
	if err != nil {
		return nil, err
	}
	id := acpSessionID(raw)
	if id == "" {
		return nil, fmt.Errorf("%s: session/new returned no sessionId", p.kind)
	}
	p.mu.Lock()
	p.sessionID = id
	p.mu.Unlock()
	p.noteModels(raw)
	return []Event{{Type: EventInit, SessionID: id}}, nil
}

func (p *acpProtocol) loadSession(ctx context.Context) ([]Event, error) {
	p.mu.Lock()
	sessionID := p.sessionID
	fresh := p.freshSession
	p.mu.Unlock()
	params := p.sessionParams()
	params["sessionId"] = sessionID
	if !fresh {
		raw, err := p.client.Call(ctx, "session/resume", params, p.handshakeTimeout)
		if err == nil {
			return p.sessionOpened(raw, sessionID), nil
		}
	}
	raw, err := p.client.Call(ctx, "session/load", params, p.handshakeTimeout)
	if err != nil {
		return nil, fmt.Errorf("%w: %s session %s: %v", ErrResumeUnsupported, p.kind, sessionID, err)
	}
	return p.sessionOpened(raw, sessionID), nil
}

func (p *acpProtocol) sessionOpened(raw json.RawMessage, fallback string) []Event {
	id := acpSessionID(raw)
	if id == "" {
		id = fallback
	}
	p.mu.Lock()
	p.sessionID = id
	p.mu.Unlock()
	p.noteModels(raw)
	return []Event{{Type: EventInit, SessionID: id}}
}

// acpConfigOption is one session config option of session/new or a
// session/set_config_option reply.
type acpConfigOption struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Category     string            `json:"category"`
	Type         string            `json:"type"`
	CurrentValue json.RawMessage   `json:"currentValue"`
	Options      []acpConfigChoice `json:"options"`
}

// acpConfigChoice is one selectable value of a config option.
type acpConfigChoice struct {
	Value       string `json:"value"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// current is the option value, as a string or as the {"value": …} object that
// some vendor docs show.
func (o acpConfigOption) current() string {
	if len(o.CurrentValue) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(o.CurrentValue, &s) == nil {
		return s
	}
	var obj struct {
		Value string `json:"value"`
	}
	if json.Unmarshal(o.CurrentValue, &obj) == nil {
		return obj.Value
	}
	return ""
}

// acpConfigOptions reads an option list, wrapped in {"configOptions": …} or
// bare. It returns nil when the reply carries none.
func acpConfigOptions(raw json.RawMessage) []acpConfigOption {
	var wrapped struct {
		ConfigOptions []acpConfigOption `json:"configOptions"`
	}
	if json.Unmarshal(raw, &wrapped) == nil && len(wrapped.ConfigOptions) > 0 {
		return wrapped.ConfigOptions
	}
	var bare []acpConfigOption
	if json.Unmarshal(raw, &bare) == nil && len(bare) > 0 {
		return bare
	}
	return nil
}

// configOption finds one option by id or category.
func configOption(options []acpConfigOption, key string) *acpConfigOption {
	for i := range options {
		if options[i].ID == key || options[i].Category == key {
			return &options[i]
		}
	}
	return nil
}

// noteModels records the model list of a session response. The session config
// options are the standard ACP source and win over the older models block;
// a response with neither keeps the old list.
func (p *acpProtocol) noteModels(raw json.RawMessage) {
	var res struct {
		Models *struct {
			CurrentModelID  string `json:"currentModelId"`
			AvailableModels []struct {
				ModelID     string `json:"modelId"`
				Name        string `json:"name"`
				Description string `json:"description"`
			} `json:"availableModels"`
		} `json:"models"`
	}
	var models []ModelInfo
	current := ""
	if json.Unmarshal(raw, &res) == nil && res.Models != nil {
		for _, m := range res.Models.AvailableModels {
			if m.ModelID != "" {
				models = append(models, ModelInfo{ID: m.ModelID, Name: m.Name, Description: m.Description})
			}
		}
		current = res.Models.CurrentModelID
	}
	if options := acpConfigOptions(raw); len(options) > 0 {
		p.noteConfigOptions(options)
		if opt := configOption(options, "model"); opt != nil && len(opt.Options) > 0 {
			// The config options are the standard source and win.
			return
		}
	}
	if models == nil {
		return
	}
	p.mu.Lock()
	p.models = models
	p.currentModel = current
	p.mu.Unlock()
}

// noteConfigOptions stores the option list and refreshes the model list and
// current model from its model option. A list without a model option leaves
// the model list alone.
func (p *acpProtocol) noteConfigOptions(options []acpConfigOption) {
	if len(options) == 0 {
		return
	}
	opt := configOption(options, "model")
	p.mu.Lock()
	p.configOptions = options
	if opt != nil && len(opt.Options) > 0 {
		models := make([]ModelInfo, 0, len(opt.Options))
		for _, choice := range opt.Options {
			if choice.Value == "" {
				continue
			}
			description := choice.Description
			if description == choice.Value {
				description = ""
			}
			models = append(models, ModelInfo{ID: choice.Value, Name: choice.Name, Description: description})
		}
		p.models = models
		p.currentModel = opt.current()
	}
	p.mu.Unlock()
}

// noteModelState records the model list that grok puts in the top-level _meta
// of initialize. It needs no session and no login.
func (p *acpProtocol) noteModelState(raw json.RawMessage) {
	var res struct {
		Meta *struct {
			ModelState *struct {
				CurrentModelID  string `json:"currentModelId"`
				AvailableModels []struct {
					ModelID     string `json:"modelId"`
					Name        string `json:"name"`
					Description string `json:"description"`
					Meta        *struct {
						TotalContextTokens int `json:"totalContextTokens"`
					} `json:"_meta"`
				} `json:"availableModels"`
			} `json:"modelState"`
		} `json:"_meta"`
	}
	if json.Unmarshal(raw, &res) != nil || res.Meta == nil || res.Meta.ModelState == nil {
		return
	}
	var models []ModelInfo
	for _, m := range res.Meta.ModelState.AvailableModels {
		if m.ModelID == "" {
			continue
		}
		description := m.Description
		if m.Meta != nil && m.Meta.TotalContextTokens > 0 {
			description = contextText(m.Meta.TotalContextTokens)
		}
		models = append(models, ModelInfo{ID: m.ModelID, Name: m.Name, Description: description})
	}
	if len(models) == 0 {
		return
	}
	p.mu.Lock()
	p.models = models
	p.currentModel = res.Meta.ModelState.CurrentModelID
	p.mu.Unlock()
}

// offersModels reports whether the handshake already gave a model list.
func (p *acpProtocol) offersModels() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.models) > 0
}

// sessionModels returns a copy of the offered models with Default on the
// current model.
func (p *acpProtocol) sessionModels() []ModelInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.models) == 0 {
		return nil
	}
	out := cloneModels(p.models)
	for i := range out {
		out[i].Default = out[i].ID == p.currentModel
	}
	return out
}

// applyModel changes the model of the open session. The standard ACP form is
// session/set_config_option when the session offers a model option; the older
// session/set_model is the fallback.
func (p *acpProtocol) applyModel(ctx context.Context, model string) error {
	p.mu.Lock()
	sessionID := p.sessionID
	opt := configOption(p.configOptions, "model")
	p.mu.Unlock()
	if opt != nil {
		raw, err := p.client.Call(ctx, "session/set_config_option", map[string]any{
			"sessionId": sessionID, "configId": opt.ID, "value": model,
		}, modelReplyWait)
		if err != nil {
			return fmt.Errorf("%s session/set_config_option: %w", p.kind, err)
		}
		// The reply is the full updated option list.
		p.noteModels(raw)
		p.mu.Lock()
		p.model = model
		p.currentModel = model
		p.mu.Unlock()
		return nil
	}
	_, err := p.client.Call(ctx, "session/set_model", map[string]any{
		"sessionId": sessionID, "modelId": model,
	}, modelReplyWait)
	if err != nil {
		return fmt.Errorf("%s session/set_model: %w", p.kind, err)
	}
	p.mu.Lock()
	p.model = model
	p.currentModel = model
	p.mu.Unlock()
	return nil
}

// applyEffort sets the thought level through the session config option and
// returns the value it sent. It returns "" when the session offers no such
// option or not the wanted level.
func (p *acpProtocol) applyEffort(ctx context.Context, effort string) (string, error) {
	p.mu.Lock()
	sessionID := p.sessionID
	opt := configOption(p.configOptions, "thought_level")
	p.mu.Unlock()
	if opt == nil {
		return "", nil
	}
	value := pickEffortValue(*opt, effort)
	if value == "" {
		return "", nil
	}
	raw, err := p.client.Call(ctx, "session/set_config_option", map[string]any{
		"sessionId": sessionID, "configId": opt.ID, "value": value,
	}, modelReplyWait)
	if err != nil {
		return "", fmt.Errorf("%s session/set_config_option: %w", p.kind, err)
	}
	p.noteModels(raw)
	return value, nil
}

// effortOrder ranks the thought levels a vendor offers.
var effortOrder = []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}

// pickEffortValue maps the requested effort onto an offered value. max means
// the highest level the vendor offers.
func pickEffortValue(opt acpConfigOption, want string) string {
	offered := make(map[string]bool, len(opt.Options))
	for _, choice := range opt.Options {
		offered[choice.Value] = true
	}
	if offered[want] {
		return want
	}
	if want == "max" {
		for i := len(effortOrder) - 1; i >= 0; i-- {
			if offered[effortOrder[i]] {
				return effortOrder[i]
			}
		}
	}
	return ""
}

// authenticate picks an auth method and reports the choice, so a consumer can
// tell which login the session used.
func (p *acpProtocol) authenticate(ctx context.Context) error {
	p.mu.Lock()
	methods := append([]string{}, p.authMethods...)
	preferred := p.authMethod
	p.mu.Unlock()
	if len(methods) == 0 {
		return errors.New("no auth methods")
	}
	pick := preferred
	if pick == "" {
		pick = methods[0]
		for _, m := range methods {
			if p.kind == acpCursor && strings.Contains(m, "cursor_login") {
				pick = m
			}
			if p.kind == acpCopilot && (strings.Contains(m, "github") || strings.Contains(m, "device")) {
				pick = m
			}
		}
	}
	_, err := p.client.Call(ctx, "authenticate", map[string]any{"methodId": pick}, p.handshakeTimeout)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.authMethod = pick
	p.mu.Unlock()
	return nil
}

// TurnActive implements wire.TurnStater: a session/prompt waits for its
// response.
func (p *acpProtocol) TurnActive() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.promptID != ""
}

// EncodePrompt sends session/prompt. Its response is the turn completion.
// It returns ErrTurnActive while a prompt waits for its response.
func (p *acpProtocol) EncodePrompt(pr Prompt) ([]byte, error) {
	p.mu.Lock()
	if p.promptID != "" {
		p.mu.Unlock()
		return nil, ErrTurnActive
	}
	p.sawResult = false
	id := p.client.NextID()
	p.promptID = string(id.Raw())
	sessionID := p.sessionID
	first := p.firstPrompt
	p.firstPrompt = false
	instructions := p.instructions
	p.mu.Unlock()
	content, err := p.promptContent(pr, first, instructions)
	if err != nil {
		return nil, err
	}
	return wire.JSONLine(map[string]any{
		"jsonrpc": jsonrpc.Version, "id": id, "method": "session/prompt",
		"params": map[string]any{"sessionId": sessionID, "prompt": content},
	})
}

// promptContent renders the ACP prompt blocks: images first, then the text
// with the session instructions in a fenced block on the first turn.
func (p *acpProtocol) promptContent(pr Prompt, first bool, instructions string) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(pr.Attachments)+1)
	for _, a := range sortedAttachments(pr.Attachments) {
		data, media, err := loadAttachment(a)
		if err != nil {
			return nil, err
		}
		if imageAttachment(media) {
			out = append(out, map[string]any{"type": "image", "mimeType": media, "data": encodeBase64(data)})
			continue
		}
		out = append(out, map[string]any{"type": "text", "text": string(data)})
	}
	out = append(out, map[string]any{"type": "text", "text": withInstructions(instructions, pr.Text, first)})
	return out, nil
}

// EncodeInterrupt sends the session/cancel notification.
func (p *acpProtocol) EncodeInterrupt() ([]byte, error) {
	p.mu.Lock()
	sessionID := p.sessionID
	p.mu.Unlock()
	return wire.JSONLine(map[string]any{
		"jsonrpc": jsonrpc.Version, "method": "session/cancel",
		"params": map[string]any{"sessionId": sessionID},
	})
}

// EncodeDecision answers session/request_permission.
func (p *acpProtocol) EncodeDecision(id string, d Decision) ([]byte, error) {
	p.mu.Lock()
	options := p.approvals[id]
	delete(p.approvals, id)
	p.mu.Unlock()
	rid, err := jsonrpc.ParseID([]byte(id))
	if err != nil {
		return nil, err
	}
	outcome := map[string]any{"outcome": "cancelled"}
	if d.Allow {
		if option := pickACPOption(options, true); option != "" {
			outcome = map[string]any{"outcome": "selected", "optionId": option}
		}
	} else if option := pickACPOption(options, false); option != "" {
		outcome = map[string]any{"outcome": "selected", "optionId": option}
	}
	return wire.JSONLine(map[string]any{
		"jsonrpc": jsonrpc.Version, "id": rid, "result": map[string]any{"outcome": outcome},
	})
}

func pickACPOption(options []acpOption, allow bool) string {
	for _, o := range options {
		if allow && (o.Kind == "allow_once" || o.Kind == "allow_always") {
			return o.ID
		}
		if !allow && (o.Kind == "reject_once" || o.Kind == "reject_always") {
			return o.ID
		}
	}
	if len(options) == 0 {
		return ""
	}
	if allow {
		return options[0].ID
	}
	return options[len(options)-1].ID
}

// Steer adds input to the running turn through the ACP extension method
// _session/steering. It returns ErrNoActiveTurn when no turn runs, and
// ErrUnsupported when the agent does not advertise steering or refuses the
// input, so Send holds the prompt.
func (p *acpProtocol) Steer(ctx context.Context, pr Prompt) error {
	p.mu.Lock()
	steering := p.steering
	active := p.promptID != ""
	sessionID := p.sessionID
	p.mu.Unlock()
	if !steering {
		return ErrUnsupported
	}
	if !active {
		return ErrNoActiveTurn
	}
	content, err := p.promptContent(pr, false, "")
	if err != nil {
		return err
	}
	raw, err := p.client.Call(ctx, "_session/steering", map[string]any{
		"sessionId": sessionID, "prompt": content,
	}, modelReplyWait)
	if err != nil {
		return fmt.Errorf("%w: %s steering: %v", ErrUnsupported, p.kind, err)
	}
	var res struct {
		Outcome string `json:"outcome"`
	}
	if json.Unmarshal(raw, &res) != nil || res.Outcome != "injected" {
		return fmt.Errorf("%w: %s steering outcome %q", ErrUnsupported, p.kind, res.Outcome)
	}
	return nil
}

// Parse routes responses, maps session/update notifications, answers the file
// system methods when a FileSystem is set, and refuses the rest so the agent
// never hangs.
func (p *acpProtocol) Parse(line []byte) []Event {
	kind, msg := p.client.Handle(line)
	switch kind {
	case jsonrpc.KindRequest:
		return p.handleAgentRequest(msg)
	case jsonrpc.KindResponse:
		return p.parseResponse(msg)
	case jsonrpc.KindUnknown:
		return nil
	}
	if msg.Method != "session/update" {
		return nil
	}
	var params struct {
		SessionID string         `json:"sessionId"`
		Update    map[string]any `json:"update"`
	}
	if len(msg.Params) > 0 {
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			return nil
		}
	}
	if params.SessionID != "" && params.SessionID != p.currentSession() {
		return nil
	}
	if params.Update == nil {
		return nil
	}
	return p.parseUpdate(params.Update)
}

func (p *acpProtocol) parseUpdate(update map[string]any) []Event {
	switch str(update["sessionUpdate"]) {
	case "agent_message_chunk":
		content, _ := update["content"].(map[string]any)
		if content != nil && str(content["type"]) == "text" {
			if text := str(content["text"]); text != "" {
				return []Event{{Type: EventAssistant, Text: text}}
			}
		}
	case "agent_thought_chunk", "plan", "plan_update", "available_commands_update", "current_mode_update":
		return []Event{{Type: EventStatus, Status: StatusRunning}}
	case "config_option_update":
		if raw, err := json.Marshal(update); err == nil {
			if options := acpConfigOptions(raw); len(options) > 0 {
				p.noteConfigOptions(options)
			}
		}
		return []Event{{Type: EventStatus, Status: StatusRunning}}
	case "usage_update":
		if u := acpUsage(update); u != nil {
			return []Event{{Type: EventUsage, Usage: u}}
		}
	case "tool_call":
		return []Event{{Type: EventTool, Tool: p.toolEvent(update, "started")}}
	case "tool_call_update":
		status := str(update["status"])
		switch status {
		case "completed", "failed":
		default:
			status = "started"
		}
		return []Event{{Type: EventTool, Tool: p.toolEvent(update, status)}}
	case "user_message_chunk":
		return nil
	}
	return nil
}

// toolEvent renders one tool call or tool call update.
func (p *acpProtocol) toolEvent(update map[string]any, status string) *ToolEvent {
	name := firstNonEmpty(str(update["title"]), str(update["kind"]), "tool")
	kind := classifyToolName(firstNonEmpty(str(update["kind"]), name))
	input := ""
	if raw, ok := update["rawInput"]; ok {
		if b, err := json.Marshal(raw); err == nil {
			input = string(b)
		}
	}
	return &ToolEvent{
		ID: str(update["toolCallId"]), Name: name, Kind: kind,
		Input: input, Output: acpToolOutput(update["content"]), Status: status,
		Paths: toolPaths(kind, input),
	}
}

// acpToolOutput renders ACP tool call content blocks: nested text, plain text
// and diff paths.
func acpToolOutput(v any) string {
	list, _ := v.([]any)
	var parts []string
	for _, item := range list {
		m, _ := item.(map[string]any)
		if m == nil {
			continue
		}
		if inner, _ := m["content"].(map[string]any); inner != nil {
			if t := str(inner["text"]); t != "" {
				parts = append(parts, t)
				continue
			}
		}
		if t := str(m["text"]); t != "" {
			parts = append(parts, t)
		}
		if p := str(m["path"]); p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, "\n")
}

// parseResponse turns a session/prompt response into the terminal turn event.
func (p *acpProtocol) parseResponse(msg jsonrpc.Message) []Event {
	p.mu.Lock()
	promptID := p.promptID
	p.mu.Unlock()
	if string(msg.ID.Raw()) != promptID {
		return nil
	}
	// A new turn resets the latch, so a crash after turn one still classifies.
	p.mu.Lock()
	p.sawResult = true
	p.promptID = ""
	p.mu.Unlock()
	if msg.Err != nil {
		text := msg.Err.Error()
		c := ClassifyFor(p.kind.harness(), text, 0)
		return []Event{{Type: EventError, Error: text, Code: c.Code, EndReason: string(c.Class)}}
	}
	var res struct {
		StopReason string          `json:"stopReason"`
		Usage      json.RawMessage `json:"usage"`
		Meta       struct {
			Usage            json.RawMessage `json:"usage"`
			InputTokens      int64           `json:"inputTokens"`
			OutputTokens     int64           `json:"outputTokens"`
			CachedReadTokens int64           `json:"cachedReadTokens"`
		} `json:"_meta"`
	}
	_ = json.Unmarshal(msg.Result, &res)
	var out []Event
	usage := acpUsageRaw(res.Usage)
	if usage == nil && len(res.Meta.Usage) > 0 {
		// grok reports the turn usage under _meta.
		usage = acpUsageRaw(res.Meta.Usage)
	}
	if usage == nil && (res.Meta.InputTokens != 0 || res.Meta.OutputTokens != 0 || res.Meta.CachedReadTokens != 0) {
		usage = &Usage{
			Input:     res.Meta.InputTokens,
			Output:    res.Meta.OutputTokens,
			CacheRead: res.Meta.CachedReadTokens,
		}
	}
	if usage != nil {
		out = append(out, Event{Type: EventUsage, Usage: usage})
	}
	switch res.StopReason {
	case "", "end_turn":
		out = append(out, Event{Type: EventResult, Result: &Result{Subtype: "success"}})
	case "cancelled":
		out = append(out, Event{Type: EventResult, Result: &Result{Subtype: "cancelled", IsError: true, Text: "turn cancelled"}})
	default:
		c := ClassifyFor(p.kind.harness(), res.StopReason, 0)
		out = append(out, Event{Type: EventResult, Result: &Result{
			Subtype: res.StopReason, IsError: true, Text: "stop: " + res.StopReason,
			EndReason: string(c.Class), Code: c.Code,
		}})
	}
	return out
}

// handleAgentRequest answers a peer request: a permission prompt, a file
// system call when the consumer supplied a FileSystem, or an explicit refusal.
func (p *acpProtocol) handleAgentRequest(msg jsonrpc.Message) []Event {
	switch msg.Method {
	case "session/request_permission":
		return p.permissionRequest(msg)
	case "fs/read_text_file":
		return p.handleReadTextFile(msg)
	case "fs/write_text_file":
		return p.handleWriteTextFile(msg)
	}
	if err := p.client.ReplyError(msg.ID, -32601, "agentwire: client method unsupported: "+msg.Method); err != nil {
		return []Event{{Type: EventError, Error: err.Error(), Code: "rpc_error", EndReason: string(FailProtocol)}}
	}
	return nil
}

// permissionRequest maps session/request_permission, unless the policy answers
// it here.
func (p *acpProtocol) permissionRequest(msg jsonrpc.Message) []Event {
	var params struct {
		ToolCall map[string]any   `json:"toolCall"`
		Options  []map[string]any `json:"options"`
	}
	_ = json.Unmarshal(msg.Params, &params)
	var options []acpOption
	for _, o := range params.Options {
		options = append(options, acpOption{ID: str(o["optionId"]), Kind: str(o["kind"])})
	}
	id := string(msg.ID.Raw())
	kind := classifyToolName(firstNonEmpty(str(params.ToolCall["kind"]), str(params.ToolCall["title"])))
	if d, ok := p.policy.Answer(kind, firstNonEmpty(str(params.ToolCall["title"]), str(params.ToolCall["kind"]))); ok {
		outcome := map[string]any{"outcome": "cancelled"}
		if option := pickACPOption(options, d.Allow); option != "" {
			outcome = map[string]any{"outcome": "selected", "optionId": option}
		}
		if err := p.client.Reply(msg.ID, map[string]any{"outcome": outcome}); err != nil {
			return []Event{{Type: EventError, Error: err.Error(), Code: "rpc_error", EndReason: string(FailProtocol)}}
		}
		return nil
	}
	p.mu.Lock()
	p.approvals[id] = options
	p.mu.Unlock()
	question := "Agent requests permission"
	if title := str(params.ToolCall["title"]); title != "" {
		question = "Allow " + title + "?"
	}
	return []Event{{Type: EventPermission, Permission: &Permission{
		ID: id, Tool: firstNonEmpty(str(params.ToolCall["title"]), str(params.ToolCall["kind"])),
		Kind: kind, Question: question,
	}}}
}

func (p *acpProtocol) handleReadTextFile(msg jsonrpc.Message) []Event {
	if p.fs == nil {
		if err := p.client.ReplyError(msg.ID, -32601, "agentwire: file system access is not enabled"); err != nil {
			return []Event{{Type: EventError, Error: err.Error(), Code: "rpc_error", EndReason: string(FailProtocol)}}
		}
		return nil
	}
	var params struct {
		Path string `json:"path"`
	}
	_ = json.Unmarshal(msg.Params, &params)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	content, err := p.fs.ReadTextFile(ctx, params.Path)
	if err != nil {
		if replyErr := p.client.ReplyError(msg.ID, -32603, err.Error()); replyErr != nil {
			return []Event{{Type: EventError, Error: replyErr.Error(), Code: "rpc_error", EndReason: string(FailProtocol)}}
		}
		return nil
	}
	if err := p.client.Reply(msg.ID, map[string]any{"content": content}); err != nil {
		return []Event{{Type: EventError, Error: err.Error(), Code: "rpc_error", EndReason: string(FailProtocol)}}
	}
	return nil
}

func (p *acpProtocol) handleWriteTextFile(msg jsonrpc.Message) []Event {
	if p.fs == nil {
		if err := p.client.ReplyError(msg.ID, -32601, "agentwire: file system access is not enabled"); err != nil {
			return []Event{{Type: EventError, Error: err.Error(), Code: "rpc_error", EndReason: string(FailProtocol)}}
		}
		return nil
	}
	var params struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	_ = json.Unmarshal(msg.Params, &params)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := p.fs.WriteTextFile(ctx, params.Path, params.Content); err != nil {
		if replyErr := p.client.ReplyError(msg.ID, -32603, err.Error()); replyErr != nil {
			return []Event{{Type: EventError, Error: replyErr.Error(), Code: "rpc_error", EndReason: string(FailProtocol)}}
		}
		return nil
	}
	if err := p.client.Reply(msg.ID, map[string]any{}); err != nil {
		return []Event{{Type: EventError, Error: err.Error(), Code: "rpc_error", EndReason: string(FailProtocol)}}
	}
	return nil
}

func (p *acpProtocol) currentSession() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sessionID
}

// Exit classifies a process exit without a prompt response.
func (p *acpProtocol) Exit(code int, err error, stderr string) []Event {
	p.mu.Lock()
	saw := p.sawResult
	p.mu.Unlock()
	if saw {
		return nil
	}
	text := strings.TrimSpace(stderr)
	if text == "" {
		if err != nil {
			text = err.Error()
		} else {
			text = fmt.Sprintf("%s exited with code %d before the turn ended", p.kind, code)
		}
	}
	c := ClassifyFor(p.kind.harness(), text, 0)
	return []Event{{Type: EventError, Error: text, Code: c.Code, EndReason: string(c.Class)}}
}

// acpSessionID reads the sessionId out of a session-open response.
func acpSessionID(raw json.RawMessage) string {
	var res struct {
		SessionID string `json:"sessionId"`
	}
	if json.Unmarshal(raw, &res) != nil {
		return ""
	}
	return res.SessionID
}

// acpUsage maps a usage_update notification.
func acpUsage(update map[string]any) *Usage {
	u := &Usage{
		Input:   int64(num(update["used"])),
		Output:  int64(num(update["size"])),
		CostUSD: acpCost(update["cost"]),
	}
	if u.Input == 0 && u.Output == 0 && u.CostUSD == 0 {
		return nil
	}
	return u
}

// acpUsageRaw maps the usage object of a prompt response.
func acpUsageRaw(raw json.RawMessage) *Usage {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	u := &Usage{
		Input:         int64(num(m["inputTokens"]) + num(m["input_tokens"])),
		Output:        int64(num(m["outputTokens"]) + num(m["output_tokens"])),
		CacheRead:     int64(num(m["cachedReadTokens"]) + num(m["cached_read_tokens"])),
		CacheCreation: int64(num(m["cachedWriteTokens"]) + num(m["cached_write_tokens"])),
		CostUSD:       acpCost(m["cost"]),
	}
	if u.Input == 0 && u.Output == 0 && u.CacheRead == 0 && u.CacheCreation == 0 && u.CostUSD == 0 {
		return nil
	}
	return u
}

func acpCost(v any) float64 {
	switch t := v.(type) {
	case map[string]any:
		return num(t["amount"])
	default:
		return num(v)
	}
}

// ensure the driver and the protocol satisfy their interfaces.
var (
	_ Driver            = acpDriver{}
	_ wire.Adapter      = (*acpProtocol)(nil)
	_ wire.SteerAdapter = (*acpProtocol)(nil)
	_ ModelLister       = acpDriver{}
	_ ModelSetter       = (*acpSession)(nil)
	_ SessionModels     = (*acpSession)(nil)
	_ Steerer           = (*acpSession)(nil)
)
