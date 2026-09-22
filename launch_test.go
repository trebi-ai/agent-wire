package agentwire

import (
	"context"
	"maps"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// launchTestSh is a real executable. The builder tests point Binary at it, so
// no vendor binary is needed and no PATH lookup happens.
const launchTestSh = "/bin/sh"

// launchTestRuntime builds the runtime under test.
func launchTestRuntime(t *testing.T) *Runtime {
	t.Helper()
	return New(Options{})
}

// launchTestClaudeClientEnv is the environment every Claude launch carries: the
// client marker the vendor uses for telemetry and for "run by a tool" mode.
func launchTestClaudeClientEnv() map[string]string {
	return map[string]string{
		"CLAUDE_CODE_ENTRYPOINT":   "agentwire-cli",
		"CLAUDE_AGENT_SDK_VERSION": "agentwire-cli/1",
	}
}

// launchTestClaudeHomeEnv is the client marker plus the isolated-home env of a
// Claude launch.
func launchTestClaudeHomeEnv(home string) map[string]string {
	env := launchTestClaudeClientEnv()
	env["CLAUDE_CONFIG_DIR"] = home
	env["DISABLE_AUTOUPDATER"] = "1"
	env["DISABLE_UPDATES"] = "1"
	return env
}

// TestLaunchPrepareGoldenBuilds asserts the exact argv tail and the exact child
// environment the launch builder produces for each harness.
func TestLaunchPrepareGoldenBuilds(t *testing.T) {
	home := t.TempDir()
	cases := []struct {
		name string
		req  StartRequest
		// resume selects the resume form of the builder.
		resume   bool
		wantArgs []string
		wantEnv  map[string]string
		// wantDarwinEnv holds env keys that only appear on darwin.
		wantDarwinEnv map[string]string
	}{
		{
			name:     "claude-new-session-id-default-permission",
			req:      StartRequest{Harness: Claude, SessionID: "sid-1"},
			wantArgs: []string{"--session-id=sid-1", "--permission-prompt-tool", "stdio", "--permission-mode", "default"},
			wantEnv:  launchTestClaudeClientEnv(),
		},
		{
			name:     "empty-harness-defaults-to-claude",
			req:      StartRequest{SessionID: "sid-1"},
			wantArgs: []string{"--session-id=sid-1", "--permission-prompt-tool", "stdio", "--permission-mode", "default"},
			wantEnv:  launchTestClaudeClientEnv(),
		},
		{
			name:     "claude-resume-session-id",
			req:      StartRequest{Harness: Claude, SessionID: "sid-1"},
			resume:   true,
			wantArgs: []string{"--resume=sid-1", "--permission-prompt-tool", "stdio", "--permission-mode", "default"},
			wantEnv:  launchTestClaudeClientEnv(),
		},
		{
			name:     "claude-model-and-effort",
			req:      StartRequest{Harness: Claude, Model: "claude-opus-4-1", Effort: "high"},
			wantArgs: []string{"--model", "claude-opus-4-1", "--effort", "high", "--permission-prompt-tool", "stdio", "--permission-mode", "default"},
			wantEnv:  launchTestClaudeClientEnv(),
		},
		{
			name:     "claude-permission-auto",
			req:      StartRequest{Harness: Claude, Permissions: PermissionPolicy{Mode: PermissionAuto}},
			wantArgs: []string{"--permission-prompt-tool", "stdio", "--permission-mode", "bypassPermissions"},
			wantEnv:  launchTestClaudeClientEnv(),
		},
		{
			name:     "claude-permission-auto-edit",
			req:      StartRequest{Harness: Claude, Permissions: PermissionPolicy{Mode: PermissionAutoEdit}},
			wantArgs: []string{"--permission-prompt-tool", "stdio", "--permission-mode", "acceptEdits"},
			wantEnv:  launchTestClaudeClientEnv(),
		},
		{
			// inherit keeps the host callback but adds no permission-mode
			// flag, so the operator's own Claude configuration decides.
			name:     "claude-permission-inherit-keeps-stdio-without-mode-flag",
			req:      StartRequest{Harness: Claude, Permissions: PermissionPolicy{Mode: PermissionInherit}},
			wantArgs: []string{"--permission-prompt-tool", "stdio"},
			wantEnv:  launchTestClaudeClientEnv(),
		},
		{
			name:     "claude-extra-args-prompt-tool-wins",
			req:      StartRequest{Harness: Claude, ExtraArgs: []string{"--permission-prompt-tool", "my-bridge"}},
			wantArgs: []string{"--permission-mode", "default", "--permission-prompt-tool", "my-bridge"},
			wantEnv:  launchTestClaudeClientEnv(),
		},
		{
			name:     "claude-extra-args-prompt-tool-equals-form-wins",
			req:      StartRequest{Harness: Claude, Effort: "low", ExtraArgs: []string{"--permission-prompt-tool=my-bridge"}},
			wantArgs: []string{"--effort", "low", "--permission-mode", "default", "--permission-prompt-tool=my-bridge"},
			wantEnv:  launchTestClaudeClientEnv(),
		},
		{
			// The stdio callback is Claude-only.
			name:     "opencode-permission-auto-gets-no-stdio-callback",
			req:      StartRequest{Harness: OpenCode, Permissions: PermissionPolicy{Mode: PermissionAuto}},
			wantArgs: nil,
		},
		{
			name:     "claude-isolated-home",
			req:      StartRequest{Harness: Claude, Home: home},
			wantArgs: []string{"--permission-prompt-tool", "stdio", "--permission-mode", "default"},
			wantEnv:  launchTestClaudeHomeEnv(home),
		},
		{
			name:          "claude-shared-auth-home-pins-keychain-on-darwin",
			req:           StartRequest{Harness: Claude, Home: home, Raw: map[string]any{"shared_auth": true}},
			wantArgs:      []string{"--permission-prompt-tool", "stdio", "--permission-mode", "default"},
			wantEnv:       launchTestClaudeHomeEnv(home),
			wantDarwinEnv: map[string]string{"CLAUDE_SECURESTORAGE_CONFIG_DIR": ""},
		},
		{
			name:          "claude-shared-auth-off-home",
			req:           StartRequest{Harness: Claude, Home: home, Raw: map[string]any{"shared_auth": false}},
			wantArgs:      []string{"--permission-prompt-tool", "stdio", "--permission-mode", "default"},
			wantEnv:       launchTestClaudeHomeEnv(home),
			wantDarwinEnv: nil,
		},
		{
			name:     "codex-session-id-stays-on-the-wire",
			req:      StartRequest{Harness: Codex, SessionID: "t-1"},
			wantArgs: nil,
		},
		{
			name:     "codex-effort-c-override-and-no-model-flag",
			req:      StartRequest{Harness: Codex, Model: "gpt-5-codex", Effort: "high"},
			wantArgs: []string{"-c", "model_reasoning_effort=high"},
		},
		{
			name:     "codex-isolated-home",
			req:      StartRequest{Harness: Codex, Home: home},
			wantArgs: nil,
			wantEnv:  map[string]string{"CODEX_HOME": home},
		},
		{
			name:     "codex-no-home-sets-nothing",
			req:      StartRequest{Harness: Codex},
			wantArgs: nil,
		},
		{
			name:     "opencode-isolated-home",
			req:      StartRequest{Harness: OpenCode, Home: home},
			wantArgs: nil,
			wantEnv:  map[string]string{"OPENCODE_CONFIG_DIR": home},
		},
		{
			name:     "pi-new-session-id",
			req:      StartRequest{Harness: Pi, SessionID: "pi-1", Home: home},
			wantArgs: []string{"--session-id", "pi-1"},
			wantEnv: map[string]string{
				"PI_CODING_AGENT_DIR":   home,
				"PI_SKIP_VERSION_CHECK": "1",
				"PI_TELEMETRY":          "0",
			},
		},
		{
			name:     "pi-resume-session",
			req:      StartRequest{Harness: Pi, SessionID: "pi-1", Home: home},
			resume:   true,
			wantArgs: []string{"--session", "pi-1"},
			wantEnv: map[string]string{
				"PI_CODING_AGENT_DIR":   home,
				"PI_SKIP_VERSION_CHECK": "1",
				"PI_TELEMETRY":          "0",
			},
		},
		{
			name:     "pi-permission-auto-approves",
			req:      StartRequest{Harness: Pi, Permissions: PermissionPolicy{Mode: PermissionAuto}},
			wantArgs: []string{"--approve"},
		},
		{
			name:     "pi-permission-ask-has-no-trust-flag",
			req:      StartRequest{Harness: Pi, Permissions: PermissionPolicy{Mode: PermissionAsk}},
			wantArgs: nil,
		},
		{
			name:     "cursor-isolated-home",
			req:      StartRequest{Harness: Cursor, Home: home},
			wantArgs: nil,
			wantEnv:  map[string]string{"CURSOR_CONFIG_DIR": home, "CURSOR_DATA_DIR": home},
		},
		{
			name:     "cursor-permission-auto-trusts",
			req:      StartRequest{Harness: Cursor, Permissions: PermissionPolicy{Mode: PermissionAuto}},
			wantArgs: []string{"--trust"},
		},
		{
			name:     "cursor-permission-auto-edit-has-no-trust-flag",
			req:      StartRequest{Harness: Cursor, Permissions: PermissionPolicy{Mode: PermissionAutoEdit}},
			wantArgs: nil,
		},
		{
			name:     "copilot-new-session-id-has-no-flag",
			req:      StartRequest{Harness: Copilot, SessionID: "cp-1"},
			wantArgs: nil,
		},
		{
			name:     "copilot-resume-adds-resume-flag",
			req:      StartRequest{Harness: Copilot, SessionID: "cp-1"},
			resume:   true,
			wantArgs: []string{"--resume", "cp-1"},
		},
		{
			name:     "copilot-model-effort-and-no-trust-flag",
			req:      StartRequest{Harness: Copilot, Model: "gpt-5", Effort: "low", Permissions: PermissionPolicy{Mode: PermissionAuto}},
			wantArgs: []string{"--model", "gpt-5", "--effort", "low"},
		},
		{
			name:     "gemini-model-without-effort",
			req:      StartRequest{Harness: Gemini, Model: "gemini-2.5-pro", Effort: "high"},
			wantArgs: []string{"--model", "gemini-2.5-pro"},
		},
		{
			name:     "extra-args-land-last",
			req:      StartRequest{Harness: Claude, Model: "m", ExtraArgs: []string{"--settings", "hooks.json"}, Env: map[string]string{"TREBI_RUN_ID": "r1"}},
			wantArgs: []string{"--model", "m", "--permission-prompt-tool", "stdio", "--permission-mode", "default", "--settings", "hooks.json"},
			wantEnv:  launchTestClaudeEnvWithRunID(),
		},
		{
			name:     "codex-extra-args-land-last",
			req:      StartRequest{Harness: Codex, Effort: "low", ExtraArgs: []string{"-c", "notify=true"}},
			wantArgs: []string{"-c", "model_reasoning_effort=low", "-c", "notify=true"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := launchTestRuntime(t)
			req := tc.req
			req.Binary = launchTestSh
			l, err := rt.prepare(context.Background(), &req, tc.resume)
			if err != nil {
				t.Fatalf("prepare: %v", err)
			}
			if l.bin != launchTestSh {
				t.Fatalf("bin = %q, want %q", l.bin, launchTestSh)
			}
			if !slices.Equal(l.args, tc.wantArgs) {
				t.Fatalf("args = %q, want %q", l.args, tc.wantArgs)
			}
			if l.resume != tc.resume {
				t.Fatalf("resume = %v, want %v", l.resume, tc.resume)
			}
			want := map[string]string{}
			maps.Copy(want, tc.wantEnv)
			if runtime.GOOS == "darwin" {
				maps.Copy(want, tc.wantDarwinEnv)
			}
			if !maps.Equal(l.env, want) {
				t.Fatalf("env = %v, want %v", l.env, want)
			}
		})
	}
}

// launchTestClaudeEnvWithRunID returns the claude client env plus the request
// Env key of the ExtraArgs case.
func launchTestClaudeEnvWithRunID() map[string]string {
	env := launchTestClaudeClientEnv()
	env["TREBI_RUN_ID"] = "r1"
	return env
}

// TestLaunchPrepareClientEnv asserts the consumer name reaches the vendor
// client marker.
func TestLaunchPrepareClientEnv(t *testing.T) {
	rt := New(Options{ClientName: "sling", ClientVersion: "1.2.3"})
	req := StartRequest{Harness: Claude, Binary: launchTestSh}
	l, err := rt.prepare(context.Background(), &req, false)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	want := map[string]string{
		"CLAUDE_CODE_ENTRYPOINT":   "sling-cli",
		"CLAUDE_AGENT_SDK_VERSION": "sling-cli/1",
	}
	if !maps.Equal(l.env, want) {
		t.Fatalf("env = %v, want %v", l.env, want)
	}
}

// TestLaunchPrepareBaseCommand asserts that BaseCommand short-circuits the
// builder: no harness flags and no home env.
func TestLaunchPrepareBaseCommand(t *testing.T) {
	rt := launchTestRuntime(t)
	req := StartRequest{
		Harness:     Claude,
		Binary:      launchTestSh,
		Home:        t.TempDir(),
		Env:         map[string]string{"HOOK": "1"},
		BaseCommand: []string{"/bin/sh", "-c", "echo hi", "--model", "x"},
	}
	l, err := rt.prepare(context.Background(), &req, true)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if l.bin != launchTestSh {
		t.Fatalf("bin = %q, want %q", l.bin, launchTestSh)
	}
	wantArgs := []string{"-c", "echo hi", "--model", "x"}
	if !slices.Equal(l.args, wantArgs) {
		t.Fatalf("args = %q, want %q", l.args, wantArgs)
	}
	wantEnv := map[string]string{"HOOK": "1"}
	if !maps.Equal(l.env, wantEnv) {
		t.Fatalf("env = %v, want %v", l.env, wantEnv)
	}
	if !l.resume {
		t.Fatal("resume = false, want true")
	}
}

// TestLaunchPrepareBinaryResolution asserts that a missing binary is an error
// with the PATH detail, and that an explicit binary skips the PATH lookup.
func TestLaunchPrepareBinaryResolution(t *testing.T) {
	rt := launchTestRuntime(t)
	ctx := context.Background()

	t.Run("missing binary names PATH", func(t *testing.T) {
		_, err := rt.prepare(ctx, &StartRequest{Harness: Harness("launchtest-missing-binary")}, false)
		if err == nil {
			t.Fatal("prepare with an unknown harness and no binary: want error, got nil")
		}
		if !strings.Contains(err.Error(), "PATH") {
			t.Fatalf("error %q does not name PATH", err)
		}
		if !strings.Contains(err.Error(), "launchtest-missing-binary") {
			t.Fatalf("error %q does not name the harness", err)
		}
	})

	t.Run("explicit binary skips PATH", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		req := StartRequest{Harness: Claude, Binary: launchTestSh}
		l, err := rt.prepare(ctx, &req, false)
		if err != nil {
			t.Fatalf("prepare: %v", err)
		}
		if l.bin != launchTestSh {
			t.Fatalf("bin = %q, want %q", l.bin, launchTestSh)
		}
	})

	t.Run("empty base command is an error", func(t *testing.T) {
		_, err := rt.prepare(ctx, &StartRequest{Harness: Claude, Binary: launchTestSh, BaseCommand: []string{"   "}}, false)
		if err == nil {
			t.Fatal("prepare with an empty BaseCommand: want error, got nil")
		}
		if !strings.Contains(err.Error(), "empty base command") {
			t.Fatalf("error %q does not explain the empty base command", err)
		}
	})

	t.Run("unknown harness with an explicit binary is a generic launch", func(t *testing.T) {
		// prepare is the builder seam and does not validate the harness name;
		// Runtime.Start rejects an unknown harness through the driver table.
		// The builder must stay generic here and must not panic.
		req := StartRequest{Harness: Harness("launchtest-unknown"), Binary: launchTestSh, ExtraArgs: []string{"--x"}}
		l, err := rt.prepare(ctx, &req, false)
		if err != nil {
			t.Fatalf("prepare: %v", err)
		}
		if l.bin != launchTestSh {
			t.Fatalf("bin = %q, want %q", l.bin, launchTestSh)
		}
		if !slices.Equal(l.args, []string{"--x"}) {
			t.Fatalf("args = %q, want the extra args alone", l.args)
		}
		if len(l.env) != 0 {
			t.Fatalf("env = %v, want empty", l.env)
		}
	})
}
