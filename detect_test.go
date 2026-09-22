package agentwire

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// launchTestFakeBinary writes an executable that prints version on stdout. It
// stands in for a vendor binary, so Detect is exercised without one.
func launchTestFakeBinary(t *testing.T, dir, name, version string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		path := filepath.Join(dir, name+".bat")
		if err := os.WriteFile(path, []byte("@echo "+version+"\r\n"), 0o755); err != nil {
			t.Fatalf("write fake %s: %v", name, err)
		}
		return path
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho "+version+"\n"), 0o755); err != nil {
		t.Fatalf("write fake %s: %v", name, err)
	}
	return path
}

// TestDetectReportsUnsupportedWithoutBinary asserts that a missing binary is a
// verdict, not an error. The PATH points at an empty dir, so no vendor binary
// can be found or spawned.
func TestDetectReportsUnsupportedWithoutBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	rt := launchTestRuntime(t)
	d, err := rt.Detect(context.Background(), Claude)
	if err != nil {
		t.Fatalf("Detect(Claude): %v", err)
	}
	if d.Harness != Claude {
		t.Fatalf("Harness = %q, want %q", d.Harness, Claude)
	}
	if d.Supported {
		t.Fatal("Supported = true without a binary")
	}
	if d.Path != "" {
		t.Fatalf("Path = %q, want empty", d.Path)
	}
	if d.Detail == "" {
		t.Fatal("Detail is empty for an unsupported verdict")
	}
	// The static feature table is reported even when the binary is missing.
	if d.Features != featuresFor(Claude) {
		t.Fatalf("Features = %+v, want %+v", d.Features, featuresFor(Claude))
	}
}

// TestDetectReportsSupportedBinary asserts that a binary above the version
// floor is supported and reports its version.
func TestDetectReportsSupportedBinary(t *testing.T) {
	dir := t.TempDir()
	path := launchTestFakeBinary(t, dir, "claude", "2.1.0")
	t.Setenv("PATH", dir)
	rt := launchTestRuntime(t)
	d, err := rt.Detect(context.Background(), Claude)
	if err != nil {
		t.Fatalf("Detect(Claude): %v", err)
	}
	if !d.Supported {
		t.Fatalf("Supported = false, Detail = %q", d.Detail)
	}
	if d.Path != path {
		t.Fatalf("Path = %q, want %q", d.Path, path)
	}
	if d.Version != "2.1.0" {
		t.Fatalf("Version = %q, want %q", d.Version, "2.1.0")
	}
	if d.Detail != "" {
		t.Fatalf("Detail = %q, want empty for a supported harness", d.Detail)
	}
	if d.Features != featuresFor(Claude) {
		t.Fatalf("Features = %+v, want %+v", d.Features, featuresFor(Claude))
	}
}

// TestDetectRejectsBelowVersionFloor asserts the version floor is a hard gate.
func TestDetectRejectsBelowVersionFloor(t *testing.T) {
	dir := t.TempDir()
	launchTestFakeBinary(t, dir, "claude", "1.9.9")
	t.Setenv("PATH", dir)
	rt := launchTestRuntime(t)
	d, err := rt.Detect(context.Background(), Claude)
	if err != nil {
		t.Fatalf("Detect(Claude): %v", err)
	}
	if d.Supported {
		t.Fatal("Supported = true below the floor")
	}
	if !strings.Contains(d.Detail, "2.0.0") {
		t.Fatalf("Detail = %q, want the floor version", d.Detail)
	}
}

// TestDetectUnsupportedHarness asserts a harness with no proven wire is an
// unsupported verdict, not an error.
func TestDetectUnsupportedHarness(t *testing.T) {
	rt := launchTestRuntime(t)
	d, err := rt.Detect(context.Background(), Harness("grok"))
	if err != nil {
		t.Fatalf("Detect(grok): %v", err)
	}
	if d.Supported {
		t.Fatal("Supported = true for a harness with no driver")
	}
	if d.Detail == "" {
		t.Fatal("Detail is empty for a driverless harness")
	}
	if d.Path != "" {
		t.Fatalf("Path = %q, want empty", d.Path)
	}
}

// TestDetectFeaturesTable pins the static per-harness capability table. The
// fake driver has no binary, so Detect reaches the table without a process
// spawn.
func TestDetectFeaturesTable(t *testing.T) {
	cases := []struct {
		h    Harness
		want Features
	}{
		{
			h: Claude,
			want: Features{
				MCPInject: MCPFlags, SkillsInject: SkillNative, Instructions: true,
				Attachments: true, Resume: true, Permissions: true,
			},
		},
		{
			h: Codex,
			want: Features{
				MCPInject: MCPFlags, SkillsInject: SkillOverlay, Instructions: true,
				Attachments: true, Resume: true, Permissions: true,
			},
		},
		{
			h: OpenCode,
			want: Features{
				MCPInject: MCPFlags, SkillsInject: SkillOverlay, Instructions: true,
				Attachments: true, Resume: true, Permissions: true,
			},
		},
		{
			h: Pi,
			want: Features{
				MCPInject: MCPFlags, SkillsInject: SkillNative, Instructions: true,
				Attachments: false, Resume: true, Permissions: true,
			},
		},
		{
			h: Copilot,
			want: Features{
				MCPInject: MCPSession, SkillsInject: SkillInstructions, Instructions: true,
				Attachments: true, Resume: true, Permissions: true, FS: true,
			},
		},
		{
			h: Cursor,
			want: Features{
				MCPInject: MCPSession, SkillsInject: SkillInstructions, Instructions: true,
				Attachments: true, Resume: true, Permissions: true, FS: true,
			},
		},
		{
			h: Gemini,
			want: Features{
				MCPInject: MCPSession, SkillsInject: SkillInstructions, Instructions: true,
				Attachments: true, Resume: true, Permissions: true, FS: true,
			},
		},
		{
			h: Fake,
			// The fake driver injects no MCP servers, so its capability is the
			// empty value, not MCPNone.
			want: Features{
				SkillsInject: SkillInstructions, Instructions: true,
				Attachments: false, Resume: true, Permissions: true,
			},
		},
	}
	for _, tc := range cases {
		t.Run(string(tc.h), func(t *testing.T) {
			if got := featuresFor(tc.h); got != tc.want {
				t.Fatalf("featuresFor(%s) = %+v, want %+v", tc.h, got, tc.want)
			}
		})
	}

	// The same table arrives through Detect for the fake driver, which needs no
	// binary.
	d, err := launchTestRuntime(t).Detect(context.Background(), Fake)
	if err != nil {
		t.Fatalf("Detect(Fake): %v", err)
	}
	if d.Features != featuresFor(Fake) {
		t.Fatalf("Detect(Fake).Features = %+v, want %+v", d.Features, featuresFor(Fake))
	}
}

// TestSemverAtLeastAndParse covers the version gate and its parser.
func TestSemverAtLeastAndParse(t *testing.T) {
	atLeast := []struct {
		got, want string
		ok        bool
	}{
		{got: "2.0.0", want: "2.0.0", ok: true},
		{got: "2.1.0", want: "2.0.0", ok: true},
		{got: "v2.0.1", want: "2.0.0", ok: true},
		{got: "2.0", want: "2.0.0", ok: true},
		{got: "3.0.0", want: "2.9.9", ok: true},
		{got: "1.9.9", want: "2.0.0", ok: false},
		{got: "2.0.0", want: "2.1.0", ok: false},
		{got: "garbage", want: "2.0.0", ok: false},
		{got: "2.0.0", want: "garbage", ok: false},
		{got: "", want: "2.0.0", ok: false},
		{got: "2.0.0-beta.1", want: "2.0.0", ok: true},
	}
	for _, tc := range atLeast {
		if got := semverAtLeast(tc.got, tc.want); got != tc.ok {
			t.Fatalf("semverAtLeast(%q, %q) = %v, want %v", tc.got, tc.want, got, tc.ok)
		}
	}

	parsed := []struct {
		in                  string
		major, minor, patch int
		ok                  bool
	}{
		{in: "2.0.0", major: 2, minor: 0, patch: 0, ok: true},
		{in: "2.1.3", major: 2, minor: 1, patch: 3, ok: true},
		{in: "v2.0.1", major: 2, minor: 0, patch: 1, ok: true},
		{in: "2.0", major: 2, minor: 0, patch: 0, ok: true},
		{in: "2.0.0-beta.1", major: 2, minor: 0, patch: 0, ok: true},
		{in: "2.0.0+build7", major: 2, minor: 0, patch: 0, ok: true},
		{in: "2.0.0 (Claude Code)", major: 2, minor: 0, patch: 0, ok: true},
		{in: "2", ok: false},
		{in: "2.x", ok: false},
		{in: "", ok: false},
		{in: "not a version", ok: false},
	}
	for _, tc := range parsed {
		major, minor, patch, ok := parseSemver(tc.in)
		if ok != tc.ok {
			t.Fatalf("parseSemver(%q) ok = %v, want %v", tc.in, ok, tc.ok)
		}
		if !ok {
			continue
		}
		if major != tc.major || minor != tc.minor || patch != tc.patch {
			t.Fatalf("parseSemver(%q) = (%d, %d, %d), want (%d, %d, %d)",
				tc.in, major, minor, patch, tc.major, tc.minor, tc.patch)
		}
	}
}
