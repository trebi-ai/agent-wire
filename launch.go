package agentwire

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// launch is one prepared child command: the binary, its harness-level
// arguments, the child environment, and the temp paths that go away when the
// session ends.
//
// A driver takes l as the base, adds its protocol arguments, and starts the
// process. The protocol half of a session (thread/start, POST /session,
// session/new) belongs to the driver; the process half (flags, env, files)
// belongs here.
type launch struct {
	bin  string
	args []string
	env  map[string]string
	tmp  []string

	// instructions is the effective per-session instruction text: the
	// consumer's text plus the skill index, minus whatever this harness
	// already took as a flag.
	instructions string

	// resume reports the resume form, for drivers that switch protocol methods.
	resume bool
}

// cleanup removes the temp paths this launch created.
func (l *launch) cleanup() {
	for _, p := range l.tmp {
		_ = os.RemoveAll(p)
	}
}

// attach registers the launch cleanup on a session, so temp files survive
// until the session really ends.
func (l *launch) attach(s Session) {
	if l == nil || len(l.tmp) == 0 {
		return
	}
	if c, ok := s.(interface{ OnClose(func()) }); ok {
		c.OnClose(l.cleanup)
	}
}

// binName is the executable name of a harness on PATH.
func binName(h Harness) string {
	switch h {
	case Cursor:
		return "cursor-agent"
	case Claude, Codex, OpenCode, OpenCode2, Pi, Copilot, Gemini, OMP, Grok:
		return string(h)
	}
	return string(h)
}

// prepare builds the child command for a request. BaseCommand short-circuits
// the whole builder.
func (rt *Runtime) prepare(ctx context.Context, req *StartRequest, resume bool) (*launch, error) {
	req.Harness = harnessOrDefault(req.Harness)
	if len(req.BaseCommand) > 0 {
		argv := req.BaseCommand
		if strings.TrimSpace(argv[0]) == "" {
			return nil, fmt.Errorf("agentwire: %s: empty base command", req.Harness)
		}
		return &launch{bin: argv[0], args: append([]string{}, argv[1:]...), env: cloneEnv(req.Env), resume: resume}, nil
	}
	bin, err := resolveBinary(req)
	if err != nil {
		return nil, err
	}
	l := &launch{bin: bin, env: cloneEnv(req.Env), resume: resume}
	rt.applyClientEnv(req, l)
	applyHomeEnv(req, l)
	applySessionArgs(req, l)
	applyModelEffort(req, l)
	applyPermissionArgs(req, l)
	if err := rt.applyMCPServers(req, l); err != nil {
		return nil, err
	}
	skillIndex, err := rt.applySkills(ctx, req, l)
	if err != nil {
		return nil, err
	}
	instructions := joinInstructions(req.Instructions, skillIndex)
	if err := rt.applyInstructions(ctx, req, l, instructions); err != nil {
		return nil, err
	}
	l.args = append(l.args, req.ExtraArgs...)
	return l, nil
}

func harnessOrDefault(h Harness) Harness {
	if h == "" {
		return Claude
	}
	return h
}

func cloneEnv(env map[string]string) map[string]string {
	out := make(map[string]string, len(env)+8)
	for k, v := range env {
		out[k] = v
	}
	return out
}

func resolveBinary(req *StartRequest) (string, error) {
	if req.Binary != "" {
		return req.Binary, nil
	}
	name := binName(req.Harness)
	path, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("agentwire: %s not found on PATH: %w", name, err)
	}
	return path, nil
}

// applyClientEnv marks the child as an SDK-class client of the runtime's
// ClientName. Vendors use the marker for telemetry and for the "run by a tool"
// behavior, so the value must be honest.
func (rt *Runtime) applyClientEnv(req *StartRequest, l *launch) {
	name := rt.opts.ClientName
	if name == "" {
		name = "agentwire"
	}
	switch req.Harness {
	case Claude:
		// The vendor sees an SDK-class client, never the plain CLI: the
		// stream-json wire is the SDK's. The version is the wire revision this
		// driver was ported from (claude-agent-sdk-python at main, 2026-09);
		// the fixture directory names the CLI build instead.
		l.env["CLAUDE_CODE_ENTRYPOINT"] = name + "-cli"
		l.env["CLAUDE_AGENT_SDK_VERSION"] = name + "-cli/1"
	}
}

// applyHomeEnv points the harness at an isolated config home. An empty Home
// changes nothing: the user's own login, MCP servers and skills stay visible.
func applyHomeEnv(req *StartRequest, l *launch) {
	if req.Home == "" {
		return
	}
	switch req.Harness {
	case Claude:
		l.env["CLAUDE_CONFIG_DIR"] = req.Home
		if shared, _ := req.RawBool("shared_auth"); shared && runtime.GOOS == "darwin" {
			// An empty value pins the default credential store (the macOS
			// Keychain) while CLAUDE_CONFIG_DIR stays isolated.
			l.env["CLAUDE_SECURESTORAGE_CONFIG_DIR"] = ""
		}
		l.env["DISABLE_AUTOUPDATER"] = "1"
		l.env["DISABLE_UPDATES"] = "1"
	case Codex:
		l.env["CODEX_HOME"] = req.Home
	case OpenCode, OpenCode2:
		l.env["OPENCODE_CONFIG_DIR"] = req.Home
	case Pi:
		l.env["PI_CODING_AGENT_DIR"] = req.Home
	case OMP:
		// omp reads the same variable as pi.
		l.env["PI_CODING_AGENT_DIR"] = req.Home
	case Grok:
		l.env["GROK_HOME"] = req.Home
		disableGrokAutoUpdate(req.Home)
	case Cursor:
		l.env["CURSOR_CONFIG_DIR"] = req.Home
		l.env["CURSOR_DATA_DIR"] = req.Home
	}
	// An isolated home must never self-update mid-run.
	switch req.Harness {
	case Pi:
		l.env["PI_SKIP_VERSION_CHECK"] = "1"
		l.env["PI_TELEMETRY"] = "0"
	}
}

// disableGrokAutoUpdate turns self-update off in an isolated grok home. An
// existing config.toml belongs to the caller and is left as it is.
func disableGrokAutoUpdate(home string) {
	path := filepath.Join(home, "config.toml")
	if _, err := os.Stat(path); err == nil {
		return
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return
	}
	_ = os.WriteFile(path, []byte("[cli]\nauto_update = false\n"), 0o600)
}

// applySessionArgs adds the session id where the harness takes it as a flag.
// Codex, OpenCode, OpenCode2, Gemini and the ACP agents take it on the wire
// instead; the driver reads StartRequest.SessionID.
func applySessionArgs(req *StartRequest, l *launch) {
	id := req.SessionID
	if id == "" {
		return
	}
	switch req.Harness {
	case Claude:
		if l.resume {
			l.args = append(l.args, "--resume="+id)
		} else {
			l.args = append(l.args, "--session-id="+id)
		}
	case Pi:
		if l.resume {
			l.args = append(l.args, "--session", id)
		} else {
			l.args = append(l.args, "--session-id", id)
		}
	case Copilot:
		if l.resume {
			l.args = append(l.args, "--resume", id)
		}
	}
}

// applyModelEffort adds the model and reasoning-effort flags. A harness that
// takes the model on the wire (Codex, OpenCode and OpenCode2 model and
// variant, Cursor set_model) gets it from the driver instead.
func applyModelEffort(req *StartRequest, l *launch) {
	model := req.Model
	switch req.Harness {
	case Codex, OpenCode, OpenCode2, Cursor:
		model = ""
	}
	l.args = append(l.args, ModelArgs(req.Harness, model, req.Effort)...)
}

// ModelArgs returns the flags that set the model and the reasoning effort on
// the interactive CLI of a harness, for example to start it in a terminal. An
// empty model or effort adds no flag. A harness without the flag gets none:
// OpenCode 2 has no model flag, and only some CLIs take an effort.
func ModelArgs(h Harness, model, effort string) []string {
	var args []string
	if model != "" {
		switch h {
		case Claude, Pi, Copilot, Gemini, OMP, Cursor:
			args = append(args, "--model", model)
		case Codex, Grok:
			args = append(args, "-m", model)
		}
	}
	if effort == "" {
		return args
	}
	switch h {
	case Claude, Pi, Copilot:
		args = append(args, "--effort", effort)
	case Codex:
		args = append(args, "-c", "model_reasoning_effort="+xhighEffort(effort))
	case OMP:
		// omp passes every level through unchanged, including off, minimal,
		// xhigh and auto.
		args = append(args, "--thinking", effort)
	case Grok:
		args = append(args, "--reasoning-effort", xhighEffort(effort))
	}
	return args
}

// xhighEffort maps the shared max level onto xhigh, the highest level of
// Codex and grok.
func xhighEffort(effort string) string {
	if effort == "max" {
		return "xhigh"
	}
	return effort
}

// applyPermissionArgs renders the policy where the harness takes it as a flag.
// Claude and Codex handle it through their own machinery; the rest ask the
// library for each request.
func applyPermissionArgs(req *StartRequest, l *launch) {
	l.args = append(l.args, trustFlags(req.Harness, req.Permissions)...)
	l.args = append(l.args, acpPermissionFlags(req.Harness, req.Permissions)...)
	if req.Harness == Claude {
		// The CLI answers prompts itself unless the host callback is enabled:
		// without `stdio` it never sends can_use_tool (C.4). This is not a
		// permission mode, so PermissionInherit keeps it.
		if !hasFlag(req.ExtraArgs, "--permission-prompt-tool") {
			l.args = append(l.args, "--permission-prompt-tool", "stdio")
		}
		if req.Permissions.Normalized().Mode != PermissionInherit {
			l.args = append(l.args, "--permission-mode", claudePermissionMode(req.Permissions))
		}
	}
}

// joinInstructions merges the consumer's instruction text with the skill
// index.
func joinInstructions(instructions, skillIndex string) string {
	instructions = strings.TrimSpace(instructions)
	skillIndex = strings.TrimSpace(skillIndex)
	switch {
	case instructions == "":
		return skillIndex
	case skillIndex == "":
		return instructions
	}
	return instructions + "\n\n" + skillIndex
}

// requireHome reports the harness's isolated-home requirement for a feature
// that has no per-session form.
func requireHome(h Harness, req *StartRequest, feature string) error {
	if req.Home != "" {
		return nil
	}
	return fmt.Errorf("%w: %s needs StartRequest.Home for harness %s", ErrUnsupported, feature, h)
}
