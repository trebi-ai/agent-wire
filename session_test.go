//go:build unix

package agentwire

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestStartWireErrors(t *testing.T) {
	rt := New(Options{})
	_, err := rt.startWire(context.Background(), Pi, &launch{bin: "/nonexistent/pi-binary"}, StartRequest{}, nil, &fakeProtocol{})
	if err == nil || !strings.HasPrefix(err.Error(), "pi: ") {
		t.Fatalf("err = %v, want a pi: prefix", err)
	}
}

func TestStartWireAttachesLaunch(t *testing.T) {
	rt := New(Options{})
	tmp := t.TempDir()
	l := &launch{bin: "/bin/sh", tmp: []string{tmp}}
	s, err := rt.startWire(context.Background(), Fake, l, StartRequest{}, []string{"-c", "cat"}, &fakeProtocol{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tmp); err != nil {
		t.Fatalf("cleanup ran at start: %v", err)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("session did not end")
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatalf("launch cleanup did not run: %v", err)
	}
}
