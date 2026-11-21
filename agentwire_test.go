package agentwire

import (
	"context"
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestRuntimeZeroOptionsIsUsable covers New with no options: every listed
// harness resolves a driver, the frame cap is the documented default, and the
// logger is never nil.
func TestRuntimeZeroOptionsIsUsable(t *testing.T) {
	t.Parallel()
	rt := New(Options{})
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	if rt.Logger() == nil {
		t.Fatal("Logger() = nil, want a logger")
	}
	if got := rt.MaxFrameBytes(); got != 16<<20 {
		t.Fatalf("MaxFrameBytes = %d, want 16 MiB", got)
	}
	if d, ok := rt.Driver(Claude); !ok || d.Name() != string(Claude) {
		t.Fatalf("Driver(claude) = %v ok=%v, want the claude driver", d, ok)
	}
	if d, ok := rt.Driver(Fake); !ok || d.Name() != string(Fake) {
		t.Fatalf("Driver(fake) = %v ok=%v, want the fake driver", d, ok)
	}
	if _, ok := rt.Driver(Harness("no-such-harness")); ok {
		t.Fatal("Driver(no-such-harness) is ok, but it has no proven wire")
	}
	for _, h := range Harnesses() {
		if h == Native {
			// A consumer registers the native driver; the library ships no
			// in-process loop in the root module.
			continue
		}
		if _, ok := rt.Driver(h); !ok {
			t.Errorf("Driver(%q) missing for a listed harness", h)
		}
	}
}

// TestRuntimeHarnessList covers the public harness set.
func TestRuntimeHarnessList(t *testing.T) {
	t.Parallel()
	list := Harnesses()
	if len(list) != 12 {
		t.Fatalf("Harnesses() = %v, want 12 entries", list)
	}
	set := map[Harness]bool{}
	for _, h := range list {
		set[h] = true
	}
	for _, want := range []Harness{Claude, Codex, OpenCode, OpenCode2, Pi, Copilot, Cursor, Gemini, OMP, Grok, Fake, Native} {
		if !set[want] {
			t.Errorf("Harnesses() misses %q", want)
		}
	}
}

// TestRuntimeSetDriverNilRestores covers driver restoration: a nil override
// drops the test driver and puts the built-in driver back, so a caller can
// always return to the default table. A harness with no built-in driver stays
// absent.
func TestRuntimeSetDriverNilRestores(t *testing.T) {
	t.Parallel()
	rt := New(Options{})
	t.Cleanup(func() { _ = rt.Close(context.Background()) })

	rt.SetDriver(Claude, FakeDriver{Script: "sleep 30"})
	if d, ok := rt.Driver(Claude); !ok || d.Name() != "fake" {
		t.Fatalf("Driver(claude) = %v (%v) after an override, want the fake driver", d, ok)
	}
	rt.SetDriver(Claude, nil)
	d, ok := rt.Driver(Claude)
	if !ok {
		t.Fatal("Driver(claude) is absent after SetDriver(nil), want the built-in driver back")
	}
	if d.Name() != string(Claude) {
		t.Fatalf("Driver(claude).Name() = %q after SetDriver(nil), want %q", d.Name(), Claude)
	}

	// A harness with no built-in driver keeps no entry at all: an override
	// goes, and a nil override leaves nothing behind.
	const unknown = Harness("no-such-harness")
	rt.SetDriver(unknown, FakeDriver{Script: "sleep 30"})
	if d, ok := rt.Driver(unknown); !ok || d.Name() != "fake" {
		t.Fatalf("Driver(%q) = %v (%v) after an override, want the fake driver", unknown, d, ok)
	}
	rt.SetDriver(unknown, nil)
	if d, ok := rt.Driver(unknown); ok {
		t.Fatalf("Driver(%q) = %v, want no driver", unknown, d)
	}
}

// TestRuntimeStartUnknownHarness covers the unknown harness path.
func TestRuntimeStartUnknownHarness(t *testing.T) {
	t.Parallel()
	rt := New(Options{})
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	ctx := context.Background()
	_, err := rt.Start(ctx, StartRequest{Harness: Harness("grok")})
	if err == nil {
		t.Fatal("Start(grok) returned no error")
	}
	if !strings.Contains(err.Error(), "grok") {
		t.Fatalf("Start error %q does not name the harness", err)
	}
	if _, err := rt.Resume(ctx, StartRequest{Harness: Harness("grok")}); err == nil || !strings.Contains(err.Error(), "grok") {
		t.Fatalf("Resume(grok) error = %v, want one naming the harness", err)
	}
}

// TestRuntimeMaxFrameBytesOption covers the option and its fallback.
func TestRuntimeMaxFrameBytesOption(t *testing.T) {
	t.Parallel()
	if got := New(Options{}).MaxFrameBytes(); got != 16<<20 {
		t.Fatalf("default MaxFrameBytes = %d, want 16 MiB", got)
	}
	if got := New(Options{MaxFrameBytes: 1024}).MaxFrameBytes(); got != 1024 {
		t.Fatalf("MaxFrameBytes option = %d, want 1024", got)
	}
	if got := New(Options{MaxFrameBytes: -1}).MaxFrameBytes(); got != 16<<20 {
		t.Fatalf("non-positive MaxFrameBytes = %d, want the default", got)
	}
}

// TestRuntimeCloseIsIdempotent covers the shutdown path: Close twice is safe.
func TestRuntimeCloseIsIdempotent(t *testing.T) {
	t.Parallel()
	rt := New(Options{})
	ctx := context.Background()
	if err := rt.Close(ctx); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := rt.Close(ctx); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

type jsonTestFS struct{}

func (jsonTestFS) ReadTextFile(context.Context, string) (string, error) { return "", nil }
func (jsonTestFS) WriteTextFile(context.Context, string, string) error  { return nil }

// TestStartRequestJSON pins the JSON field names and the three fields that the
// JSON form leaves out.
func TestStartRequestJSON(t *testing.T) {
	t.Parallel()
	req := StartRequest{
		Harness:          Claude,
		Binary:           "/bin/claude",
		WorkingDir:       "/work",
		Env:              map[string]string{"A": "b"},
		Home:             "/home",
		SessionID:        "s1",
		Model:            "m",
		Effort:           "high",
		Permissions:      PermissionPolicy{Mode: PermissionMode("ask")},
		MCPServers:       []MCPServer{{Name: "x"}},
		Skills:           []Skill{{Name: "y"}},
		Instructions:     "be brief",
		Credentials:      &Credentials{APIKey: "secret"},
		OneShot:          true,
		OneShotPrompt:    "go",
		HandshakeTimeout: time.Second,
		ExtraArgs:        []string{"--x"},
		BaseCommand:      []string{"claude"},
		Raw:              map[string]any{"strict_mcp": true},
		LogPath:          "/log",
		FragmentLogPath:  "/frag",
		LogFragments:     true,
		FS:               jsonTestFS{},
		OnStderr:         func(Harness, string) {},
	}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(data, &keys); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"harness", "binary", "working_dir", "env", "home", "session_id", "model", "effort",
		"permissions", "mcp_servers", "skills", "instructions", "one_shot", "one_shot_prompt",
		"handshake_timeout", "extra_args", "base_command", "raw", "frame_log", "fragment_log",
		"log_fragments",
	}
	got := slices.Sorted(maps.Keys(keys))
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}

	var back StartRequest
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	req.Credentials, req.FS, req.OnStderr = nil, nil, nil
	if !reflect.DeepEqual(back, req) {
		t.Errorf("round trip = %+v, want %+v", back, req)
	}
}
