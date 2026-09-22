//go:build live

// Package live holds the live tier of the agentwire test suite.
//
// The live tier drives the real vendor binaries with the machine's own shared
// login. It never mocks a vendor, and it never runs in CI. Run it with
// scripts/test.live.sh, or set AGENTWIRE_LIVE to a comma-separated harness list
// (or "all").
//
// Every file in this package carries the "live" build tag, so the default
// build never compiles it.
package live

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trebi-ai/agent-wire"
)

// liveTestHarnesses lists the harnesses that need a vendor binary. The fake
// harness has no binary and no wire, so it stays out of the live tier.
var liveTestHarnesses = []agentwire.Harness{
	agentwire.Claude,
	agentwire.Codex,
	agentwire.OpenCode,
	agentwire.Pi,
	agentwire.Copilot,
	agentwire.Cursor,
	agentwire.Gemini,
}

// liveTestMCPHelperEnv marks the re-executed test binary that serves the MCP
// helper. The helper writes JSON-RPC on stdout, so TestMain returns before the
// live preflight when it sees this variable, and it starts no other test.
const liveTestMCPHelperEnv = "AGENTWIRE_MCP_HELPER"

// liveTestForbiddenKeys must stay unset. The live tier proves the shared login
// path; an API key would mask a broken login.
var liveTestForbiddenKeys = []string{
	"ANTHROPIC_API_KEY",
	"CLAUDE_CODE_OAUTH_TOKEN",
	"OPENAI_API_KEY",
}

// liveTestMarker is the fixed prefix of every skill marker phrase. The random
// suffix makes the phrase unique in one machine.
const liveTestMarker = "agentwire-live-marker-"

// liveTestSkillName is the skill name the live skills test injects.
const liveTestSkillName = "agentwire-live-marker"

// Timeouts. A hung vendor binary must not hang CI.
const (
	liveTestDetectTimeout = 90 * time.Second
	liveTestStartTimeout  = 3 * time.Minute
	liveTestTurnTimeout   = 5 * time.Minute
	liveTestSkillTimeout  = 6 * time.Minute
	liveTestCloseTimeout  = 45 * time.Second
)

func TestMain(m *testing.M) {
	if os.Getenv(liveTestMCPHelperEnv) == "1" {
		// The MCP helper is this test binary run again. It must serve MCP on
		// stdout without any preflight text on the stream.
		os.Exit(m.Run())
	}
	if os.Getenv("AGENTWIRE_LIVE") != "" {
		if key := liveTestForbiddenKey(); key != "" {
			fmt.Fprintf(os.Stderr, "agentwire live tier: %s is set\n", key)
			fmt.Fprintln(os.Stderr, "The live tier uses the shared login only. Unset the key and run it again.")
			os.Exit(1)
		}
	}
	os.Exit(m.Run())
}

// liveTestForbiddenKey returns the first forbidden environment key that is set.
func liveTestForbiddenKey() string {
	for _, key := range liveTestForbiddenKeys {
		if os.Getenv(key) != "" {
			return key
		}
	}
	return ""
}

// liveEnabled reports whether the live tier runs this harness.
//
// An unset AGENTWIRE_LIVE skips every test. A harness reached only through
// "all" that has no binary skips; an explicitly named harness that has no
// binary fails the run.
func liveEnabled(t *testing.T, h agentwire.Harness) bool {
	t.Helper()
	name := string(h)
	sel := strings.TrimSpace(os.Getenv("AGENTWIRE_LIVE"))
	if sel == "" {
		t.Skipf("live tier disabled: set AGENTWIRE_LIVE=%s or run scripts/test.live.sh %s", name, name)
		return false
	}
	all := false
	selected := false
	for _, part := range strings.Split(sel, ",") {
		switch strings.TrimSpace(part) {
		case "all":
			all = true
			selected = true
		case name:
			selected = true
		}
	}
	if !selected {
		t.Skipf("live tier: harness %s is not selected", name)
		return false
	}
	if liveTestBinary(name) == "" {
		if all {
			t.Skipf("live tier: %s is not installed", name)
			return false
		}
		t.Fatalf("live tier: %s is named in AGENTWIRE_LIVE but has no binary on PATH", name)
	}
	return true
}

// liveTestBinary resolves the vendor binary the library launches for one
// harness name. The cursor agent installs under a different name.
func liveTestBinary(name string) string {
	bin := name
	if name == string(agentwire.Cursor) {
		bin = "cursor-agent"
	}
	p, err := exec.LookPath(bin)
	if err != nil {
		return ""
	}
	return p
}

// liveRuntime builds a runtime for one test: the live client name, a temp
// ledger, and a logger that lands in the test log.
func liveRuntime(t *testing.T) *agentwire.Runtime {
	t.Helper()
	logs := &liveTestLogBuf{}
	rt := agentwire.New(agentwire.Options{
		ClientName:    "agentwire-live",
		ClientVersion: "0.0.0-live",
		LedgerPath:    filepath.Join(t.TempDir(), "agentwire-live-procs.json"),
		Logger:        slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		OnStderr: func(h agentwire.Harness, line string) {
			logs.append(fmt.Sprintf("[%s] stderr: %s", h, line))
		},
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), liveTestCloseTimeout)
		defer cancel()
		if err := rt.Close(ctx); err != nil {
			t.Errorf("live tier: runtime close: %v", err)
		}
		logs.flush(t)
	})
	return rt
}

// liveWorkDir returns a temp working directory for one session. Symlinks are
// resolved, because a harness may compare paths against its real root.
func liveWorkDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	return dir
}

// liveTestLogBuf collects runtime log lines. The runtime logs from session
// goroutines, so the buffer takes the lines and the test flushes them at the
// end, when no goroutine writes to testing.T any more.
type liveTestLogBuf struct {
	mu    sync.Mutex
	lines []string
}

func (b *liveTestLogBuf) Write(p []byte) (int, error) {
	text := strings.TrimRight(string(p), "\n")
	b.mu.Lock()
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) != "" {
			b.lines = append(b.lines, line)
		}
	}
	b.mu.Unlock()
	return len(p), nil
}

func (b *liveTestLogBuf) append(line string) {
	b.mu.Lock()
	b.lines = append(b.lines, line)
	b.mu.Unlock()
}

func (b *liveTestLogBuf) flush(t *testing.T) {
	b.mu.Lock()
	lines := b.lines
	b.lines = nil
	b.mu.Unlock()
	for _, line := range lines {
		t.Logf("runtime: %s", line)
	}
}

// liveTestReady probes one harness and gates on the login. A missing login
// skips when AGENTWIRE_LIVE_SKIP_NOAUTH=1 and fails otherwise.
func liveTestReady(t *testing.T, rt *agentwire.Runtime, h agentwire.Harness) agentwire.Detection {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), liveTestDetectTimeout)
	defer cancel()
	d, err := rt.Detect(ctx, h)
	if err != nil {
		t.Fatalf("live tier: detect %s: %v", h, err)
	}
	if !d.Supported {
		t.Fatalf("live tier: %s is not supported: %s", h, d.Detail)
	}
	if d.Auth == agentwire.AuthMissing {
		if os.Getenv("AGENTWIRE_LIVE_SKIP_NOAUTH") == "1" {
			t.Skipf("live tier: %s has no login: %s", h, d.AuthDetail)
		}
		t.Fatalf("live tier: %s has no login: %s (set AGENTWIRE_LIVE_SKIP_NOAUTH=1 to skip)", h, d.AuthDetail)
	}
	return d
}

// liveTestRequest builds a session request for one harness: a temp working
// directory, the user's own login, and permissions the tier answers itself.
//
// AGENTWIRE_LIVE_MODEL_<HARNESS> (or AGENTWIRE_LIVE_MODEL for every harness)
// pins the model. Set it when the harness default is unavailable on this
// machine, for example an OpenCode config whose provider is out of credits:
// the tier then measures the library, not the account.
func liveTestRequest(h agentwire.Harness, dir string) agentwire.StartRequest {
	return agentwire.StartRequest{
		Harness:     h,
		WorkingDir:  dir,
		Model:       liveTestModel(h),
		Permissions: agentwire.PermissionPolicy{Mode: agentwire.PermissionAuto},
	}
}

// liveTestModel reads the model override for one harness.
func liveTestModel(h agentwire.Harness) string {
	name := strings.ToUpper(strings.ReplaceAll(string(h), "-", "_"))
	if m := strings.TrimSpace(os.Getenv("AGENTWIRE_LIVE_MODEL_" + name)); m != "" {
		return m
	}
	return strings.TrimSpace(os.Getenv("AGENTWIRE_LIVE_MODEL"))
}

// liveTestStart starts one session and registers its close.
func liveTestStart(t *testing.T, rt *agentwire.Runtime, req agentwire.StartRequest) agentwire.Session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), liveTestStartTimeout)
	defer cancel()
	s, err := rt.Start(ctx, req)
	if err != nil {
		t.Fatalf("live tier: %s start: %v", req.Harness, err)
	}
	t.Cleanup(func() { liveTestCloseSession(t, req.Harness, s, &liveTestObs{}) })
	return s
}

// liveTestPrompt writes one user turn.
func liveTestPrompt(t *testing.T, h agentwire.Harness, s agentwire.Session, text string, timeout time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := s.Prompt(ctx, agentwire.Prompt{Text: text, Kind: "live"}); err != nil {
		t.Fatalf("live tier: %s prompt: %v", h, err)
	}
}

// liveTestObs is what a test observed while reading a session.
type liveTestObs struct {
	text      strings.Builder
	init      int
	assistant int
	tools     []agentwire.ToolEvent
	errors    []string
	result    *agentwire.Event
	exit      *agentwire.Event
	closed    bool
}

// note folds one event into the observation.
func (o *liveTestObs) note(ev *agentwire.Event) {
	switch ev.Type {
	case agentwire.EventInit:
		o.init++
	case agentwire.EventAssistant:
		o.assistant++
		o.text.WriteString(ev.Text)
	case agentwire.EventTool:
		if ev.Tool != nil {
			o.tools = append(o.tools, *ev.Tool)
		}
	case agentwire.EventError:
		o.errors = append(o.errors, ev.Error)
	case agentwire.EventResult:
		cp := *ev
		o.result = &cp
	case agentwire.EventExit:
		cp := *ev
		o.exit = &cp
	}
}

// liveTestRead reads events until done reports true, the channel closes, or
// the deadline passes. A deadline is a test failure: a hung vendor must not
// hang CI.
func liveTestRead(t *testing.T, h agentwire.Harness, s agentwire.Session, o *liveTestObs, timeout time.Duration, done func(*liveTestObs) bool) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		if done != nil && done(o) {
			return
		}
		select {
		case ev, ok := <-s.Events():
			if !ok {
				o.closed = true
				return
			}
			o.note(&ev)
		case <-deadline:
			t.Fatalf("live tier: %s did not reach the expected event within %s (init=%d assistant=%d result=%v exit=%v closed=%v errors=%v)",
				h, timeout, o.init, o.assistant, o.result != nil, o.exit != nil, o.closed, o.errors)
			return
		}
	}
}

// liveTestCloseSession closes a session and drains Events until it closes.
func liveTestCloseSession(t *testing.T, h agentwire.Harness, s agentwire.Session, o *liveTestObs) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), liveTestCloseTimeout)
	defer cancel()
	if err := s.Close(ctx); err != nil {
		t.Errorf("live tier: %s close: %v", h, err)
	}
	o.closed = false
	liveTestRead(t, h, s, o, liveTestCloseTimeout, func(obs *liveTestObs) bool { return obs.closed })
}

// liveTestTurn runs one prompt and reads until the terminal result.
func liveTestTurn(t *testing.T, h agentwire.Harness, s agentwire.Session, prompt string, timeout time.Duration) *liveTestObs {
	t.Helper()
	o := &liveTestObs{}
	liveTestPrompt(t, h, s, prompt, timeout)
	liveTestRead(t, h, s, o, timeout, func(obs *liveTestObs) bool { return obs.result != nil })
	return o
}

// liveTestAssertTurn checks the shape every live turn must have: an init, at
// least one assistant event, and a result that is not an error.
func liveTestAssertTurn(t *testing.T, h agentwire.Harness, o *liveTestObs) {
	t.Helper()
	if o.init == 0 {
		t.Errorf("live tier: %s: no init event", h)
	}
	if o.assistant == 0 {
		t.Errorf("live tier: %s: no assistant event (errors=%v)", h, o.errors)
	}
	if o.result == nil {
		t.Fatalf("live tier: %s: no result event (errors=%v)", h, o.errors)
	}
	if o.result.Result == nil {
		t.Fatalf("live tier: %s: result event carries no payload", h)
	}
	if o.result.Result.IsError {
		t.Errorf("live tier: %s: result is an error: subtype=%q code=%q text=%q",
			h, o.result.Result.Subtype, o.result.Result.Code, o.result.Result.Text)
	}
}

// liveTestToken returns a short random token that no vendor can guess.
func liveTestToken(t *testing.T) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("live tier: random: %v", err)
	}
	return hex.EncodeToString(b[:])
}
