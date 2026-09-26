package agentwire

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// AuthState is the best-effort login view of a harness.
type AuthState string

const (
	AuthUnknown AuthState = "unknown"
	AuthOK      AuthState = "ok"
	AuthMissing AuthState = "missing"
)

// Features reports what a harness can take per session. It is static per
// harness: a feature that needs StartRequest.Home degrades at run time rather
// than appearing and disappearing here.
type Features struct {
	// MCPInject says how MCPServers reach the harness.
	MCPInject MCPCapability
	// SkillsInject says how Skills reach the harness.
	SkillsInject SkillSupport
	// Instructions reports per-session instruction support.
	Instructions bool
	// Attachments reports prompt image support.
	Attachments bool
	// Resume reports session resume support.
	Resume bool
	// Permissions reports that the harness asks before a tool runs and the
	// library can answer.
	Permissions bool
	// FS reports that the harness can route file reads and writes to
	// StartRequest.FS.
	FS bool
}

// Detection is the result of probing one harness on this machine.
type Detection struct {
	Harness Harness
	// Path is the resolved binary, empty when the harness is not installed.
	Path string
	// Version is the first line of `<bin> --version`, empty when unknown.
	Version string
	// Supported reports that the binary exists and meets the version floor.
	Supported bool
	// Auth is the best-effort login state.
	Auth AuthState
	// AuthDetail is a short note from the auth probe, such as an account
	// email. It never contains a token.
	AuthDetail string
	// Features is the static capability table for the harness.
	Features Features
	// Detail explains an unsupported verdict.
	Detail string
}

// versionFloor is the minimum vendor version this library was written against.
// A floor of "" means any version the binary reports.
var versionFloors = map[Harness]string{
	Claude: "2.0.0",
}

// featuresFor is the static per-harness capability table.
func featuresFor(h Harness) Features {
	switch h {
	case Claude:
		return Features{MCPInject: MCPFlags, SkillsInject: SkillNative, Instructions: true, Attachments: true, Resume: true, Permissions: true}
	case Codex:
		return Features{MCPInject: MCPFlags, SkillsInject: SkillOverlay, Instructions: true, Attachments: true, Resume: true, Permissions: true}
	case OpenCode, OpenCode2:
		return Features{MCPInject: MCPFlags, SkillsInject: SkillOverlay, Instructions: true, Attachments: true, Resume: true, Permissions: true}
	case Pi:
		return Features{MCPInject: MCPFlags, SkillsInject: SkillNative, Instructions: true, Resume: true, Permissions: true}
	case Copilot, Cursor, Gemini:
		return Features{MCPInject: MCPSession, SkillsInject: SkillInstructions, Instructions: true, Attachments: true, Resume: true, Permissions: true, FS: true}
	case Fake:
		return Features{SkillsInject: SkillInstructions, Instructions: true, Resume: true, Permissions: true}
	}
	return Features{SkillsInject: SkillInstructions}
}

// Detect probes one harness: binary, version, floor and login state. A missing
// binary is a supported=false verdict, not an error.
func (rt *Runtime) Detect(ctx context.Context, h Harness) (Detection, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	d := Detection{Harness: h, Features: featuresFor(h)}
	if h == Fake {
		// The fake driver runs a shell script from AGENTWIRE_FAKE_SCRIPT. It
		// has no vendor binary to find and no login, so it is always usable.
		d.Supported = true
		d.Auth = AuthOK
		return d, nil
	}
	if _, ok := rt.Driver(h); !ok {
		d.Detail = "no proven subprocess wire for this harness"
		return d, nil
	}
	bin := binName(h)
	if p, err := exec.LookPath(bin); err == nil {
		d.Path = p
	}
	if d.Path == "" {
		d.Detail = bin + " is not installed"
		return d, nil
	}
	d.Version = rt.Version(ctx, d.Path)
	if floor := versionFloors[h]; floor != "" && d.Version != "" && !semverAtLeast(d.Version, floor) {
		d.Detail = fmt.Sprintf("%s %s is below the supported floor %s", h, firstToken(d.Version), floor)
		return d, nil
	}
	d.Supported = true
	d.Auth, d.AuthDetail = rt.probeAuth(ctx, h, d.Path)
	return d, nil
}

// Version returns the cached `--version` line of a binary.
func (rt *Runtime) Version(ctx context.Context, bin string) string {
	return rt.vers.version(ctx, bin)
}

// supportsFlag reports, with a per-runtime cache, whether `<bin> --help`
// mentions a flag.
func (rt *Runtime) supportsFlag(ctx context.Context, bin, flag string) bool {
	return rt.vers.flag(ctx, bin, flag)
}

// HelpText returns the cached `<bin> --help` output, for a decision that needs
// to compare two flags instead of testing one.
func (rt *Runtime) HelpText(ctx context.Context, bin string) string {
	return rt.vers.help(ctx, bin)
}

// versionTTL caches vendor probes: a pinned binary changes rarely and every
// session would otherwise pay a process spawn.
const versionTTL = 5 * time.Minute

type versionCache struct {
	mu       sync.Mutex
	versions map[string]versionEntry
	flags    map[string]flagEntry
	helps    map[string]helpEntry
}

type helpEntry struct {
	at   time.Time
	text string
}

type versionEntry struct {
	at  time.Time
	ver string
}

type flagEntry struct {
	at  time.Time
	has bool
}

func newVersionCache() *versionCache {
	return &versionCache{
		versions: map[string]versionEntry{},
		flags:    map[string]flagEntry{},
		helps:    map[string]helpEntry{},
	}
}

func (c *versionCache) help(ctx context.Context, bin string) string {
	if bin == "" {
		return ""
	}
	c.mu.Lock()
	if e, ok := c.helps[bin]; ok && time.Since(e.at) < versionTTL {
		c.mu.Unlock()
		return e.text
	}
	c.mu.Unlock()
	out, _ := runProbe(ctx, bin, "--help")
	text := string(out)
	c.mu.Lock()
	c.helps[bin] = helpEntry{at: time.Now(), text: text}
	c.mu.Unlock()
	return text
}

func (c *versionCache) version(ctx context.Context, bin string) string {
	c.mu.Lock()
	if e, ok := c.versions[bin]; ok && time.Since(e.at) < versionTTL {
		c.mu.Unlock()
		return e.ver
	}
	c.mu.Unlock()
	ver := probeVersion(ctx, bin)
	if ver != "" {
		c.mu.Lock()
		c.versions[bin] = versionEntry{at: time.Now(), ver: ver}
		c.mu.Unlock()
	}
	return ver
}

func (c *versionCache) flag(ctx context.Context, bin, flag string) bool {
	key := bin + "\x00" + flag
	c.mu.Lock()
	if e, ok := c.flags[key]; ok && time.Since(e.at) < versionTTL {
		c.mu.Unlock()
		return e.has
	}
	c.mu.Unlock()
	has := probeFlag(ctx, bin, flag)
	c.mu.Lock()
	c.flags[key] = flagEntry{at: time.Now(), has: has}
	c.mu.Unlock()
	return has
}

// probeVersion runs `<bin> --version` with a short deadline.
func probeVersion(ctx context.Context, bin string) string {
	out, err := runProbe(ctx, bin, "--version")
	if err != nil || len(out) == 0 {
		return ""
	}
	line := strings.TrimSpace(string(out))
	if i := strings.IndexByte(line, '\n'); i > 0 {
		line = line[:i]
	}
	if len(line) > 120 {
		line = line[:120]
	}
	return line
}

// probeFlag greps `<bin> --help` for a flag.
func probeFlag(ctx context.Context, bin, flag string) bool {
	if bin == "" || flag == "" {
		return false
	}
	out, _ := runProbe(ctx, bin, "--help")
	return strings.Contains(string(out), flag)
}

func runProbe(ctx context.Context, bin string, args ...string) ([]byte, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return exec.CommandContext(probeCtx, bin, args...).CombinedOutput()
}

// authEnvKeys are the environment variables whose presence means a usable
// login, per harness. The library never reads a value.
var authEnvKeys = map[Harness][]string{
	Claude:  {"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"},
	Codex:   {"OPENAI_API_KEY"},
	Gemini:  {"GEMINI_API_KEY", "GOOGLE_API_KEY"},
	Copilot: {"GITHUB_TOKEN", "GH_TOKEN"},
}

// authProbe is one vendor login-status command.
type authProbe struct {
	args []string
	// parse maps the command output to a state; ok=false means the output was
	// not conclusive.
	parse func(out string, err error) (AuthState, string, bool)
}

var authProbes = map[Harness]authProbe{
	Claude: {
		args: []string{"auth", "status"},
		parse: func(out string, err error) (AuthState, string, bool) {
			var payload struct {
				LoggedIn bool   `json:"loggedIn"`
				Email    string `json:"email"`
			}
			if json.Unmarshal([]byte(out), &payload) == nil && strings.Contains(out, "loggedIn") {
				if payload.LoggedIn {
					return AuthOK, payload.Email, true
				}
				return AuthMissing, "", true
			}
			low := strings.ToLower(out)
			switch {
			case strings.Contains(low, "not logged") || strings.Contains(low, `"loggedin": false`):
				return AuthMissing, "", true
			case strings.Contains(low, "logged in") || strings.Contains(low, "loggedin\": true"):
				return AuthOK, "", true
			}
			_ = err
			return AuthUnknown, "", false
		},
	},
	Codex: {
		args: []string{"login", "status"},
		parse: func(out string, err error) (AuthState, string, bool) {
			low := strings.ToLower(out)
			switch {
			case strings.Contains(low, "not logged in"), strings.Contains(low, "logged out"):
				return AuthMissing, "", true
			case strings.Contains(low, "logged in"):
				return AuthOK, "", true
			}
			_ = err
			return AuthUnknown, "", false
		},
	},
	OpenCode: {
		args: []string{"auth", "list"},
		parse: func(out string, err error) (AuthState, string, bool) {
			if err != nil {
				return AuthUnknown, "", false
			}
			if strings.TrimSpace(out) == "" {
				return AuthMissing, "", true
			}
			return AuthOK, "", true
		},
	},
	OpenCode2: {
		args: []string{"auth", "list"},
		parse: func(out string, err error) (AuthState, string, bool) {
			if err != nil {
				return AuthUnknown, "", false
			}
			if strings.TrimSpace(out) == "" {
				return AuthMissing, "", true
			}
			return AuthOK, "", true
		},
	},
}

// probeAuth reports the best-effort login state. It never returns an error:
// an inconclusive probe is AuthUnknown. The subprocess probe answers from a
// TTL cache with single-flight on a miss (plan 2026-09-26 H); the environment
// check stays outside, because it is free and always current.
func (rt *Runtime) probeAuth(ctx context.Context, h Harness, bin string) (AuthState, string) {
	for _, key := range authEnvKeys[h] {
		if os.Getenv(key) != "" {
			return AuthOK, "key " + key
		}
	}
	if _, ok := authProbes[h]; !ok {
		return AuthUnknown, ""
	}
	ttl := rt.authTTL
	if ttl == 0 {
		ttl = defaultAuthTTL
	}
	key := authKey{harness: h, bin: bin}
	if ttl > 0 {
		rt.authMu.Lock()
		e, ok := rt.auth[key]
		rt.authMu.Unlock()
		if ok && time.Since(e.at) < ttl {
			return e.state, e.detail
		}
	}
	e, err := rt.authFlt.Do(key, func() (authEntry, error) {
		if ttl > 0 {
			rt.authMu.Lock()
			cached, ok := rt.auth[key]
			rt.authMu.Unlock()
			if ok && time.Since(cached.at) < ttl {
				return cached, nil
			}
		}
		state, detail := runAuthProbe(ctx, h, bin)
		e := authEntry{at: time.Now(), state: state, detail: detail}
		if ttl > 0 {
			rt.authMu.Lock()
			rt.auth[key] = e
			rt.authMu.Unlock()
		}
		return e, nil
	})
	if err != nil {
		return AuthUnknown, ""
	}
	return e.state, e.detail
}

// InvalidateDetect drops the cached login probes of one harness. A consumer
// calls it after a login or a logout, so Detect sees the new state at once.
func (rt *Runtime) InvalidateDetect(h Harness) {
	rt.authMu.Lock()
	for key := range rt.auth {
		if key.harness == h {
			delete(rt.auth, key)
		}
	}
	rt.authMu.Unlock()
}

// authKey names one cached probe.
type authKey struct {
	harness Harness
	bin     string
}

// authEntry is one cached login probe.
type authEntry struct {
	at     time.Time
	state  AuthState
	detail string
}

// defaultAuthTTL bounds a cached login probe. A login state changes rarely,
// and every Detect call would otherwise spawn a process.
const defaultAuthTTL = 5 * time.Minute

// runAuthProbe runs the vendor login-status command for one harness.
func runAuthProbe(ctx context.Context, h Harness, bin string) (AuthState, string) {
	probe := authProbes[h]
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(probeCtx, bin, probe.args...).CombinedOutput()
	if state, detail, conclusive := probe.parse(string(out), err); conclusive {
		return state, detail
	}
	return AuthUnknown, ""
}

// firstToken is the first whitespace-delimited field, used to trim a version
// line to its number.
func firstToken(s string) string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}
