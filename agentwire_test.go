package agentwire

import (
	"context"
	"strings"
	"testing"
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
	if _, ok := rt.Driver(Harness("grok")); ok {
		t.Fatal("Driver(grok) is ok, but grok has no proven wire")
	}
	for _, h := range Harnesses() {
		if _, ok := rt.Driver(h); !ok {
			t.Errorf("Driver(%q) missing for a listed harness", h)
		}
	}
}

// TestRuntimeHarnessList covers the public harness set.
func TestRuntimeHarnessList(t *testing.T) {
	t.Parallel()
	list := Harnesses()
	if len(list) != 9 {
		t.Fatalf("Harnesses() = %v, want 9 entries", list)
	}
	set := map[Harness]bool{}
	for _, h := range list {
		set[h] = true
	}
	for _, want := range []Harness{Claude, Codex, OpenCode, OpenCode2, Pi, Copilot, Cursor, Gemini, Fake} {
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
