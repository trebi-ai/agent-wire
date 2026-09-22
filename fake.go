package agentwire

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/trebi-ai/agent-wire/internal/proc"
	"github.com/trebi-ai/agent-wire/internal/wire"
)

// FakeScriptEnv overrides the fake driver script from the environment.
const FakeScriptEnv = "AGENTWIRE_FAKE_SCRIPT"

// DefaultFakeScript emits a tiny assistant turn and a success result.
const DefaultFakeScript = `read -r line || true; echo '{"type":"assistant","text":"working"}'; echo '{"type":"result","subtype":"success","text":"done"}'`

// FakeDriver is the scripted driver for tests: it spawns a shell that emits the
// same NDJSON vocabulary the vendors do, so a consumer's event pump is
// exercised without a vendor binary.
//
// It implements Driver on its own, so a consumer injects it with
// Runtime.SetDriver.
type FakeDriver struct {
	// Script is a shell program. Empty uses FakeScriptEnv, then
	// DefaultFakeScript.
	Script string
	// Dir overrides the working directory.
	Dir string
}

// Name implements Driver.
func (FakeDriver) Name() string { return string(Fake) }

// Start implements Driver.
func (f FakeDriver) Start(ctx context.Context, req StartRequest) (Session, error) {
	return f.start(ctx, req)
}

// Resume implements Driver.
func (f FakeDriver) Resume(ctx context.Context, req StartRequest) (Session, error) {
	return f.start(ctx, req)
}

func (f FakeDriver) start(ctx context.Context, req StartRequest) (Session, error) {
	script := f.Script
	if script == "" {
		script = os.Getenv(FakeScriptEnv)
	}
	if script == "" {
		script = DefaultFakeScript
	}
	dir := f.Dir
	if dir == "" {
		dir = req.WorkingDir
	}
	p, err := proc.Start(ctx, proc.Opts{
		Path: "sh", Args: []string{"-c", script}, Dir: dir, Env: proc.ChildEnv(req.Env),
	})
	if err != nil {
		return nil, err
	}
	s, err := wire.Start(ctx, string(Fake), p, fakeProtocol{}, wire.Config{})
	if err != nil {
		return nil, err
	}
	if req.SessionID != "" {
		s.SetID(req.SessionID)
	}
	if req.OneShot && req.OneShotPrompt != "" {
		if err := s.Prompt(ctx, Prompt{Text: req.OneShotPrompt, Kind: "job"}); err != nil {
			_ = s.KillTree()
			return nil, err
		}
		_ = p.CloseStdin()
	}
	return s, nil
}

// fakeProtocol parses the fake NDJSON vocabulary.
type fakeProtocol struct{}

func (fakeProtocol) Handshake(context.Context, *wire.Writer) ([]Event, error) { return nil, nil }

func (fakeProtocol) EncodePrompt(p Prompt) ([]byte, error) {
	return json.Marshal(map[string]any{"type": "user", "text": p.Text})
}

func (fakeProtocol) Parse(line []byte) []Event {
	var m map[string]any
	if json.Unmarshal(line, &m) != nil {
		return nil
	}
	text := func(key string) string { return fmt.Sprint(m[key]) }
	switch strings.ToLower(text("type")) {
	case "init":
		return []Event{{Type: EventInit, SessionID: text("session_id")}}
	case "assistant":
		return []Event{{Type: EventAssistant, Text: text("text")}}
	case "user":
		return []Event{{Type: EventUser, Text: text("text")}}
	case "tool":
		return []Event{{Type: EventTool, Tool: &ToolEvent{
			ID: text("id"), Name: text("name"),
			Input: text("input"), Output: text("output"),
			Status: orDefault(text("status"), "started"),
		}}}
	case "permission":
		return []Event{{Type: EventPermission, Permission: &Permission{
			ID: text("id"), Tool: text("tool"), Question: text("question"),
		}}}
	case "result":
		isErr, _ := m["is_error"].(bool)
		return []Event{{Type: EventResult, Result: &Result{
			Subtype: orDefault(text("subtype"), "success"),
			IsError: isErr,
			Text:    text("text"),
		}}}
	case "error":
		return []Event{{Type: EventError, Error: text("error"), Code: text("code"), EndReason: text("end_reason")}}
	case "status":
		return []Event{{Type: EventStatus, Status: SessionStatus(text("status"))}}
	}
	return nil
}

func (fakeProtocol) EncodeDecision(id string, d Decision) ([]byte, error) {
	behavior := "deny"
	if d.Allow {
		behavior = "allow"
	}
	return json.Marshal(map[string]any{"type": "decision", "id": id, "behavior": behavior, "message": d.Message})
}

func (fakeProtocol) Exit(code int, err error, stderr string) []Event {
	if code == 0 && err == nil {
		return nil
	}
	msg := strings.TrimSpace(stderr)
	if msg == "" && err != nil {
		msg = err.Error()
	}
	if msg == "" {
		msg = fmt.Sprintf("fake harness exited with code %d", code)
	}
	return []Event{{Type: EventError, Error: msg, Code: "process_exited", ExitCode: &code}}
}
