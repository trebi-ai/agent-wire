package agentwire

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/trebi-ai/agent-wire/internal/wire"
)

// This file tests the model list, the live model change and the mid-turn
// input: the runtime cache, then each harness against a scripted peer.

// mdTestFixture reads one file under testdata/models.
func mdTestFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/models/" + name)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return b
}

// mdTestIDs lists the ids of a model list.
func mdTestIDs(models []ModelInfo) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		out = append(out, m.ID)
	}
	return out
}

// mdTestDefault returns the id of the model marked Default.
func mdTestDefault(models []ModelInfo) string {
	for _, m := range models {
		if m.Default {
			return m.ID
		}
	}
	return ""
}

// mdTestLister is a driver that counts its probes.
type mdTestLister struct {
	FakeDriver
	calls *atomic.Int32
	err   error
	gate  chan struct{}
}

func (d mdTestLister) Models(context.Context, ModelQuery) ([]ModelInfo, error) {
	d.calls.Add(1)
	if d.gate != nil {
		<-d.gate
	}
	if d.err != nil {
		return nil, d.err
	}
	return []ModelInfo{{ID: "m1", Default: true}, {ID: "m2"}}, nil
}

// mdTestPlainDriver is a driver with no lister.
type mdTestPlainDriver struct{ FakeDriver }

func (mdTestPlainDriver) Models() {}

func TestRuntimeModelsCache(t *testing.T) {
	rt := New(Options{})
	calls := &atomic.Int32{}
	rt.SetDriver(Fake, mdTestLister{calls: calls})
	ctx := context.Background()

	first, err := rt.Models(ctx, Fake, ModelQuery{})
	if err != nil || !slices.Equal(mdTestIDs(first), []string{"m1", "m2"}) {
		t.Fatalf("first list: %v %v", first, err)
	}
	// The caller owns its copy.
	first[0].ID = "changed"
	second, err := rt.Models(ctx, Fake, ModelQuery{})
	if err != nil || second[0].ID != "m1" {
		t.Fatalf("second list: %v %v", second, err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("probes after two calls = %d, want 1", n)
	}
	// A different home is a different key.
	if _, err := rt.Models(ctx, Fake, ModelQuery{Home: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("probes after a second home = %d, want 2", n)
	}
	rt.InvalidateModels(Fake)
	if _, err := rt.Models(ctx, Fake, ModelQuery{}); err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 3 {
		t.Fatalf("probes after InvalidateModels = %d, want 3", n)
	}
}

func TestRuntimeModelsSingleProbe(t *testing.T) {
	rt := New(Options{})
	calls := &atomic.Int32{}
	gate := make(chan struct{})
	rt.SetDriver(Fake, mdTestLister{calls: calls, gate: gate})
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := rt.Models(context.Background(), Fake, ModelQuery{}); err != nil {
				t.Errorf("models: %v", err)
			}
		}()
	}
	time.Sleep(100 * time.Millisecond)
	close(gate)
	wg.Wait()
	if n := calls.Load(); n != 1 {
		t.Fatalf("probes for concurrent calls = %d, want 1", n)
	}
}

func TestRuntimeModelsErrorTTL(t *testing.T) {
	rt := New(Options{})
	calls := &atomic.Int32{}
	boom := errors.New("not logged in")
	rt.SetDriver(Fake, mdTestLister{calls: calls, err: boom})
	for range 2 {
		if _, err := rt.Models(context.Background(), Fake, ModelQuery{}); !errors.Is(err, boom) {
			t.Fatalf("models error = %v", err)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("probes for a cached error = %d, want 1", n)
	}
	old := time.Now().Add(-modelErrorTTL - time.Second)
	if (modelEntry{at: old, err: boom}).fresh(time.Hour) {
		t.Fatal("an error entry older than the error TTL is fresh")
	}
	if !(modelEntry{at: old}).fresh(time.Hour) {
		t.Fatal("a list entry inside the TTL is not fresh")
	}
}

func TestRuntimeModelsCacheOff(t *testing.T) {
	rt := New(Options{ModelCacheTTL: -1})
	calls := &atomic.Int32{}
	rt.SetDriver(Fake, mdTestLister{calls: calls})
	for range 2 {
		if _, err := rt.Models(context.Background(), Fake, ModelQuery{}); err != nil {
			t.Fatal(err)
		}
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("probes with the cache off = %d, want 2", n)
	}
}

func TestRuntimeModelsUnsupported(t *testing.T) {
	rt := New(Options{})
	rt.SetDriver(Fake, mdTestPlainDriver{})
	if _, err := rt.Models(context.Background(), Fake, ModelQuery{}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("models of a driver with no lister: %v", err)
	}
}

// Claude.

// mdTestClaudePeer answers the control requests of a Claude session. A
// set_model to "bogus" fails.
func mdTestClaudePeer(t *testing.T) func(*coaTestPeer, coaTestFrame) {
	initReply := string(mdTestFixture(t, "claude_initialize.json"))
	return func(p *coaTestPeer, f coaTestFrame) {
		if f.m["type"] != "control_request" {
			return
		}
		id, _ := f.m["request_id"].(string)
		req, _ := f.m["request"].(map[string]any)
		switch req["subtype"] {
		case "initialize":
			p.push([]byte(strings.Replace(initReply, "INIT", id, 1)))
		case "set_model":
			if req["model"] == "bogus" {
				p.push(map[string]any{"type": "control_response", "response": map[string]any{
					"subtype": "error", "request_id": id, "error": "Model 'bogus' not found",
				}})
				return
			}
			p.push(map[string]any{"type": "control_response", "response": map[string]any{
				"subtype": "success", "request_id": id, "response": map[string]any{},
			}})
		}
	}
}

func TestClaudeModelsAndSetModel(t *testing.T) {
	peer := coaTestNewPeer(t, mdTestClaudePeer(t))
	proto := newClaudeProtocol(coaTestRuntime(), PermissionPolicy{})
	ws := wire.NewMemorySession("claude", proto, peer.inW, peer.out, context.Background(), wire.Config{})
	s := &claudeSession{Session: ws, p: proto}

	// The initialize reply arrives after the handshake returns.
	deadline := time.Now().Add(3 * time.Second)
	for len(s.Models()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	models := s.Models()
	if !slices.Equal(mdTestIDs(models), []string{"default", "sonnet", "haiku"}) {
		t.Fatalf("session models: %+v", models)
	}
	if mdTestDefault(models) != "default" || models[1].Name != "Sonnet" || !strings.Contains(models[1].Description, "Sonnet 4.6") {
		t.Fatalf("session model fields: %+v", models)
	}

	peer.drain()
	if err := s.SetModel(context.Background(), "sonnet"); err != nil {
		t.Fatalf("set model: %v", err)
	}
	frame := peer.wait(func(f coaTestFrame) bool { return f.m["type"] == "control_request" })
	if req, _ := frame.m["request"].(map[string]any); req["subtype"] != "set_model" || req["model"] != "sonnet" {
		t.Fatalf("set_model frame: %s", frame.raw)
	}
	err := s.SetModel(context.Background(), "bogus")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("set model to a bad id: %v", err)
	}

	// A reply for an unknown id is dropped.
	peer.push(map[string]any{"type": "control_response", "response": map[string]any{
		"subtype": "success", "request_id": "nobody", "response": map[string]any{"models": []any{}},
	}})
	if err := s.SetModel(context.Background(), "haiku"); err != nil {
		t.Fatalf("set model after a stray reply: %v", err)
	}
	if got := len(s.Models()); got != 3 {
		t.Fatalf("models after a stray reply: %d", got)
	}
}

func TestClaudeModelsParse(t *testing.T) {
	var frame struct {
		Response struct {
			Response map[string]any `json:"response"`
		} `json:"response"`
	}
	if err := json.Unmarshal(mdTestFixture(t, "claude_initialize.json"), &frame); err != nil {
		t.Fatal(err)
	}
	models := claudeModels(frame.Response.Response)
	if len(models) != 3 || models[0].Name != "Default (recommended)" {
		t.Fatalf("claude models: %+v", models)
	}
	if claudeModels(map[string]any{}) != nil {
		t.Fatal("an initialize reply with no models gives a list")
	}
}

func TestClaudeSteer(t *testing.T) {
	peer := coaTestNewPeer(t, mdTestClaudePeer(t))
	proto := newClaudeProtocol(coaTestRuntime(), PermissionPolicy{})
	ws := wire.NewMemorySession("claude", proto, peer.inW, peer.out, context.Background(), wire.Config{})
	s := &claudeSession{Session: ws, p: proto}

	if err := s.Steer(context.Background(), Prompt{Text: "more"}); !errors.Is(err, ErrNoActiveTurn) {
		t.Fatalf("steer while idle: %v", err)
	}
	if err := s.Prompt(context.Background(), Prompt{Text: "start"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Steer(context.Background(), Prompt{Text: "more"}); err != nil {
		t.Fatalf("steer during a turn: %v", err)
	}
	peer.wait(func(f coaTestFrame) bool { return f.m["type"] == "user" && strings.Contains(string(f.raw), "more") })
	peer.push(map[string]any{"type": "result", "subtype": "success", "result": "ok"})
	deadline := time.Now().Add(3 * time.Second)
	for proto.active() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if err := s.Steer(context.Background(), Prompt{Text: "late"}); !errors.Is(err, ErrNoActiveTurn) {
		t.Fatalf("steer after the result: %v", err)
	}
}

// Codex.

func TestCodexModelPages(t *testing.T) {
	page1, next, err := codexModelPage(mdTestFixture(t, "codex_model_list_1.json"))
	if err != nil || next != "page-2" {
		t.Fatalf("page 1: next %q, %v", next, err)
	}
	page2, next, err := codexModelPage(mdTestFixture(t, "codex_model_list_2.json"))
	if err != nil || next != "" {
		t.Fatalf("page 2: next %q, %v", next, err)
	}
	models := append(page1, page2...)
	// The hidden model is left out.
	if !slices.Equal(mdTestIDs(models), []string{"glm-5.3", "glm-5-turbo"}) {
		t.Fatalf("codex models: %+v", models)
	}
	if mdTestDefault(models) != "glm-5.3" || models[1].Name != "GLM-5 Turbo" {
		t.Fatalf("codex model fields: %+v", models)
	}
}

func TestCodexSetModelNextTurn(t *testing.T) {
	peer, ws, proto := coaTestStartCodex(t, StartRequest{Model: "glm-5.3"}, coaTestCodexHandshake)
	s := &codexSession{Session: ws, p: proto}
	events := coaTestWatch(t, s.Events())
	events.next(EventInit)

	if err := s.Prompt(context.Background(), Prompt{Text: "one"}); err != nil {
		t.Fatal(err)
	}
	if got := peer.waitMethod("turn/start").params()["model"]; got != "glm-5.3" {
		t.Fatalf("first turn model: %v", got)
	}
	if err := s.SetModel(context.Background(), "glm-5-turbo"); err != nil {
		t.Fatal(err)
	}
	peer.notify("turn/completed", map[string]any{"turn": map[string]any{"id": "turn_1", "status": "completed"}})
	events.next(EventResult)
	if err := s.Prompt(context.Background(), Prompt{Text: "two"}); err != nil {
		t.Fatal(err)
	}
	if got := peer.waitMethod("turn/start").params()["model"]; got != "glm-5-turbo" {
		t.Fatalf("second turn model: %v", got)
	}
}

func TestCodexSteer(t *testing.T) {
	var steers atomic.Int32
	handler := func(p *coaTestPeer, f coaTestFrame) {
		if f.method() == "turn/steer" {
			if steers.Add(1) == 1 {
				p.reply(f.m["id"], map[string]any{"turnId": "turn_1"})
			}
			// The second steer gets no ack.
			return
		}
		coaTestCodexHandshake(p, f)
	}
	peer, ws, proto := coaTestStartCodex(t, StartRequest{}, handler)
	s := &codexSession{Session: ws, p: proto}
	events := coaTestWatch(t, s.Events())
	events.next(EventInit)

	if err := s.Steer(context.Background(), Prompt{Text: "more"}); !errors.Is(err, ErrNoActiveTurn) {
		t.Fatalf("steer while idle: %v", err)
	}
	if err := s.Prompt(context.Background(), Prompt{Text: "start"}); err != nil {
		t.Fatal(err)
	}
	peer.waitMethod("turn/start")
	peer.notify("turn/started", map[string]any{"turn": map[string]any{"id": "turn_1"}})
	events.next(EventStatus)
	if err := s.Steer(context.Background(), Prompt{Text: "more"}); err != nil {
		t.Fatalf("steer during a turn: %v", err)
	}
	steer := peer.waitMethod("turn/steer")
	if steer.params()["expectedTurnId"] != "turn_1" {
		t.Fatalf("turn/steer params: %s", steer.raw)
	}
	if err := s.Steer(context.Background(), Prompt{Text: "again"}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("steer with no ack: %v", err)
	}
	// A failed steer never interrupts the turn.
	for {
		select {
		case f := <-peer.seen:
			if f.method() == "turn/interrupt" {
				t.Fatal("steer sent turn/interrupt")
			}
			continue
		default:
		}
		break
	}
}

// OpenCode.

// mdTestOpenCodeServer starts one HTTP server with the given routes and
// returns a server handle for the wire.
func mdTestOpenCodeServer(t *testing.T, w openCodeWire, routes map[string]http.HandlerFunc) *openCodeServer {
	t.Helper()
	mux := http.NewServeMux()
	for path, h := range routes {
		mux.HandleFunc(path, h)
	}
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	u, _ := url.Parse(ts.URL)
	port, _ := strconv.Atoi(u.Port())
	return &openCodeServer{port: port, username: "u", password: "p", rt: coaTestRuntime(), wire: w, sessions: map[string]openCodeRoute{}}
}

// mdTestRaw answers every request with one body.
func mdTestRaw(body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}
}

func TestOpenCodeModelList(t *testing.T) {
	srv := mdTestOpenCodeServer(t, openCodeV1{}, map[string]http.HandlerFunc{
		"/config/providers": mdTestRaw(mdTestFixture(t, "opencode_providers.json")),
		"/config":           mdTestRaw([]byte(`{"model":"opencode-go/deepseek-v4.1-flash"}`)),
	})
	models, err := openCodeV1{}.models(context.Background(), srv, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(mdTestIDs(models), []string{"opencode-go/deepseek-v4.1-flash", "opencode-go/kimi-k3"}) {
		t.Fatalf("opencode models: %+v", models)
	}
	if mdTestDefault(models) != "opencode-go/deepseek-v4.1-flash" {
		t.Fatalf("opencode default: %+v", models)
	}
	if models[0].Description != "OpenCode Go · 1M context" || models[1].Description != "OpenCode Go · 256K context" {
		t.Fatalf("opencode descriptions: %+v", models)
	}
}

func TestOpenCode2ModelList(t *testing.T) {
	srv := mdTestOpenCodeServer(t, openCode2{}, map[string]http.HandlerFunc{
		"/api/model":         mdTestRaw(mdTestFixture(t, "opencode2_models.json")),
		"/api/model/default": mdTestRaw([]byte(`{"data":{"modelID":"kimi-k3","providerID":"opencode-go"}}`)),
	})
	models, err := openCode2{}.models(context.Background(), srv, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// The disabled model is left out.
	if !slices.Equal(mdTestIDs(models), []string{"opencode-go/deepseek-v4.1-flash", "opencode-go/kimi-k3"}) {
		t.Fatalf("opencode2 models: %+v", models)
	}
	if mdTestDefault(models) != "opencode-go/kimi-k3" || models[1].Description != "256K context" {
		t.Fatalf("opencode2 model fields: %+v", models)
	}
}

func TestOpenCodeSetModelNextPrompt(t *testing.T) {
	oc := coaTestNewOpenCode(t, coaTestRuntime())
	sess := oc.session("s1", t.TempDir(), "", "anthropic/claude-sonnet-4", "", PermissionPolicy{})
	oc.sse.wait(t)
	if err := sess.SetModel(context.Background(), "opencode-go/kimi-k3"); err != nil {
		t.Fatal(err)
	}
	if err := sess.Prompt(context.Background(), Prompt{Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	model, _ := oc.promptBodies()[0]["model"].(map[string]any)
	if model["providerID"] != "opencode-go" || model["modelID"] != "kimi-k3" {
		t.Fatalf("prompt model: %+v", model)
	}
}

func TestOpenCode2SetModelAndTurnActive(t *testing.T) {
	oc := oc2TestNewOpenCode(t, coaTestRuntime())
	sess := oc.session("s1", t.TempDir(), "", PermissionPolicy{})
	oc.sse.wait(t)
	if err := sess.SetModel(context.Background(), "opencode-go/kimi-k3"); err != nil {
		t.Fatal(err)
	}
	oc.mu.Lock()
	bodies := append([]map[string]any(nil), oc.models...)
	oc.mu.Unlock()
	if len(bodies) != 1 {
		t.Fatalf("model bodies: %+v", bodies)
	}
	if ref, _ := bodies[0]["model"].(map[string]any); ref["modelID"] != "kimi-k3" && ref["id"] != "kimi-k3" {
		t.Fatalf("model body: %+v", bodies[0])
	}
	if err := sess.Prompt(context.Background(), Prompt{Text: "one"}); err != nil {
		t.Fatal(err)
	}
	if err := sess.Prompt(context.Background(), Prompt{Text: "two"}); !errors.Is(err, ErrTurnActive) {
		t.Fatalf("prompt during a turn: %v, want ErrTurnActive", err)
	}
	if n := len(oc.promptBodies()); n != 1 {
		t.Fatalf("prompt bodies after a refused prompt: %d", n)
	}
	events := coaTestWatch(t, sess.Events())
	d, err := Send(context.Background(), sess, Prompt{Text: "three"})
	if err != nil || d != DeliveryHeld {
		t.Fatalf("send during a turn: %q %v, want held", d, err)
	}
	if n := len(oc.promptBodies()); n != 1 {
		t.Fatalf("prompt bodies after a held send: %d", n)
	}
	oc.sse.push(t, "session.execution.succeeded", map[string]any{"sessionID": "s1"})
	events.next(EventResult)
	deadline := time.Now().Add(3 * time.Second)
	for len(oc.promptBodies()) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	bodies2 := oc.promptBodies()
	if len(bodies2) != 2 || bodies2[1]["text"] != "three" {
		t.Fatalf("prompt bodies after the result: %+v", bodies2)
	}
}

// ACP.

func TestACPSessionModels(t *testing.T) {
	newReply := mdTestFixture(t, "acp_session_new.json")
	handler := func(p *coaTestPeer, f coaTestFrame) {
		switch f.method() {
		case "session/new":
			p.reply(f.m["id"], json.RawMessage(newReply))
		case "session/set_model":
			p.reply(f.m["id"], map[string]any{})
		default:
			coaTestACPAgent(p, f)
		}
	}
	peer, ws, proto := coaTestStartACP(t, acpCursor, StartRequest{}, handler)
	s := &acpSession{Session: ws, p: proto}
	coaTestWatch(t, s.Events()).next(EventInit)

	models := s.Models()
	if !slices.Equal(mdTestIDs(models), []string{"composer-2", "gpt-5.5"}) || mdTestDefault(models) != "composer-2" {
		t.Fatalf("acp models: %+v", models)
	}
	if err := s.SetModel(context.Background(), "gpt-5.5"); err != nil {
		t.Fatal(err)
	}
	call := peer.waitMethod("session/set_model")
	if call.params()["modelId"] != "gpt-5.5" || call.params()["sessionId"] != "ses_1" {
		t.Fatalf("set_model params: %s", call.raw)
	}
	if got := mdTestDefault(s.Models()); got != "gpt-5.5" {
		t.Fatalf("current model after SetModel: %q", got)
	}
}

func TestACPSetModelWithNoList(t *testing.T) {
	_, ws, proto := coaTestStartACP(t, acpCopilot, StartRequest{}, coaTestACPAgent)
	s := &acpSession{Session: ws, p: proto}
	coaTestWatch(t, s.Events()).next(EventInit)
	if s.Models() != nil {
		t.Fatalf("models with no list: %+v", s.Models())
	}
	if err := s.SetModel(context.Background(), "x"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("set model with no list: %v", err)
	}
}

// Pi.

func TestPiModelsParse(t *testing.T) {
	var avail, state struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(mdTestFixture(t, "pi_available_models.json"), &avail); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(mdTestFixture(t, "pi_state.json"), &state); err != nil {
		t.Fatal(err)
	}
	models, err := piModels(avail.Data)
	if err != nil {
		t.Fatal(err)
	}
	piMarkDefault(models, state.Data)
	if !slices.Equal(mdTestIDs(models), []string{"xai/grok-4.5", "zai/glm-5.3"}) {
		t.Fatalf("pi models: %+v", models)
	}
	if mdTestDefault(models) != "xai/grok-4.5" || models[0].Description != "256K context" {
		t.Fatalf("pi model fields: %+v", models)
	}
}

// mdTestPiPeer answers pi commands. set_model to provider "nope" and a steer
// with the text "refuse" fail.
func mdTestPiPeer(p *coaTestPeer, f coaTestFrame) {
	typ, _ := f.m["type"].(string)
	id := f.m["id"]
	if id == nil {
		return
	}
	reply := map[string]any{"type": "response", "command": typ, "success": true, "id": id}
	switch {
	case typ == "set_model" && f.m["provider"] == "nope":
		reply["success"] = false
		reply["error"] = "Model not found: nope/x"
	case typ == "steer" && f.m["message"] == "refuse":
		reply["success"] = false
		reply["error"] = "not streaming"
	}
	p.push(reply)
}

func TestPiSetModelAndSteer(t *testing.T) {
	peer := coaTestNewPeer(t, mdTestPiPeer)
	proto := newPiProtocol(PermissionPolicy{}, "")
	ws := wire.NewMemorySession("pi", proto, peer.inW, peer.out, context.Background(), wire.Config{})
	s := &piSession{Session: ws, p: proto}
	ctx := context.Background()

	if err := s.SetModel(ctx, "zai/glm-5.3"); err != nil {
		t.Fatalf("set model: %v", err)
	}
	call := peer.wait(func(f coaTestFrame) bool { return f.m["type"] == "set_model" })
	if call.m["provider"] != "zai" || call.m["modelId"] != "glm-5.3" {
		t.Fatalf("set_model frame: %s", call.raw)
	}
	if err := s.SetModel(ctx, "nope/x"); err == nil || !strings.Contains(err.Error(), "Model not found") {
		t.Fatalf("set model to a bad id: %v", err)
	}
	if err := s.SetModel(ctx, "noslash"); err == nil {
		t.Fatal("set model with no provider succeeded")
	}

	if err := s.Steer(ctx, Prompt{Text: "more"}); !errors.Is(err, ErrNoActiveTurn) {
		t.Fatalf("steer while idle: %v", err)
	}
	if err := s.Prompt(ctx, Prompt{Text: "start"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Steer(ctx, Prompt{Text: "more"}); err != nil {
		t.Fatalf("steer during a turn: %v", err)
	}
	steer := peer.wait(func(f coaTestFrame) bool { return f.m["type"] == "steer" })
	if steer.m["message"] != "more" {
		t.Fatalf("steer frame: %s", steer.raw)
	}
	if err := s.Steer(ctx, Prompt{Text: "refuse"}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("refused steer: %v", err)
	}
	peer.push(map[string]any{"type": "agent_end", "messages": []any{}})
	deadline := time.Now().Add(3 * time.Second)
	for proto.active() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if err := s.Steer(ctx, Prompt{Text: "late"}); !errors.Is(err, ErrNoActiveTurn) {
		t.Fatalf("steer after agent_end: %v", err)
	}
}

// Fake.

// mdTestFakeScript echoes the steer and set_model lines of one turn.
const mdTestFakeScript = `read -r a
echo '{"type":"assistant","text":"working"}'
read -r b
case "$b" in *'"steer"'*) echo '{"type":"assistant","text":"steered"}';; esac
read -r c
case "$c" in *'"set_model"'*) echo '{"type":"assistant","text":"model set"}';; esac
echo '{"type":"result","subtype":"success"}'
read -r d || true`

func TestFakeModelsAndSteer(t *testing.T) {
	rtTestSkipWithoutShell(t)
	rt := New(Options{})
	rt.SetDriver(Fake, FakeDriver{Script: mdTestFakeScript, ModelList: []ModelInfo{{ID: "fast", Default: true}, {ID: "smart"}}})
	models, err := rt.Models(context.Background(), Fake, ModelQuery{})
	if err != nil || !slices.Equal(mdTestIDs(models), []string{"fast", "smart"}) {
		t.Fatalf("fake models: %v %v", models, err)
	}

	sess, err := rt.Start(context.Background(), StartRequest{Harness: Fake, WorkingDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close(context.Background()) })
	st, ok := sess.(Steerer)
	if !ok {
		t.Fatalf("fake session %T is not a Steerer", sess)
	}
	ms, ok := sess.(ModelSetter)
	if !ok {
		t.Fatalf("fake session %T is not a ModelSetter", sess)
	}
	events := coaTestWatch(t, sess.Events())
	if err := st.Steer(context.Background(), Prompt{Text: "early"}); !errors.Is(err, ErrNoActiveTurn) {
		t.Fatalf("steer while idle: %v", err)
	}
	if err := sess.Prompt(context.Background(), Prompt{Text: "go"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Steer(context.Background(), Prompt{Text: "more"}); err != nil {
		t.Fatalf("steer: %v", err)
	}
	if err := ms.SetModel(context.Background(), "smart"); err != nil {
		t.Fatalf("set model: %v", err)
	}
	events.nextWhere(func(e Event) bool { return e.Type == EventAssistant && e.Text == "steered" })
	events.nextWhere(func(e Event) bool { return e.Type == EventAssistant && e.Text == "model set" })
	events.next(EventResult)
	if err := st.Steer(context.Background(), Prompt{Text: "late"}); !errors.Is(err, ErrNoActiveTurn) {
		t.Fatalf("steer after the result: %v", err)
	}
}

// mdTestSendScript runs two turns. The first turn reads one more line, so a
// steer is visible. The second turn reports whether it got the joined prompt.
const mdTestSendScript = `read -r a
echo '{"type":"assistant","text":"turn one"}'
read -r b
case "$b" in *'"steer"'*) echo '{"type":"assistant","text":"steered"}';; esac
echo '{"type":"result","subtype":"success"}'
read -r c
case "$c" in *'a\n\nb'*) echo '{"type":"assistant","text":"next: joined"}';; *) echo '{"type":"assistant","text":"next: other"}';; esac
echo '{"type":"result","subtype":"success"}'
read -r d || true`

func TestSendStates(t *testing.T) {
	rtTestSkipWithoutShell(t)
	ctx := context.Background()
	start := func(t *testing.T, d FakeDriver) (Session, *coaTestEvents) {
		t.Helper()
		rt := New(Options{})
		rt.SetDriver(Fake, d)
		sess, err := rt.Start(ctx, StartRequest{Harness: Fake, WorkingDir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = sess.Close(ctx) })
		return sess, coaTestWatch(t, sess.Events())
	}

	t.Run("idle prompts and active steers", func(t *testing.T) {
		sess, events := start(t, FakeDriver{Script: mdTestSendScript})
		if d, err := Send(ctx, sess, Prompt{Text: "go"}); err != nil || d != DeliveryPrompted {
			t.Fatalf("idle send: %q %v", d, err)
		}
		if d, err := Send(ctx, sess, Prompt{Text: "more"}); err != nil || d != DeliverySteered {
			t.Fatalf("active send: %q %v", d, err)
		}
		events.nextWhere(func(e Event) bool { return e.Type == EventAssistant && e.Text == "steered" })
		events.next(EventResult)
	})

	t.Run("refused steer holds until the result", func(t *testing.T) {
		sess, events := start(t, FakeDriver{Script: mdTestSendScript, NoSteer: true})
		if _, err := Send(ctx, sess, Prompt{Text: "go"}); err != nil {
			t.Fatal(err)
		}
		if d, err := Send(ctx, sess, Prompt{Text: "a"}); err != nil || d != DeliveryHeld {
			t.Fatalf("held send: %q %v", d, err)
		}
		if d, err := Send(ctx, sess, Prompt{Text: "b"}); err != nil || d != DeliveryHeld {
			t.Fatalf("second held send: %q %v", d, err)
		}
		// The script reads one line in the turn; a blank steer stand-in
		// keeps it moving, because a held prompt writes nothing.
		ws := sess.(*fakeSession)
		if err := ws.WriteFrame([]byte(`{"type":"noop"}`)); err != nil {
			t.Fatal(err)
		}
		first := events.next(EventResult)
		next := events.nextWhere(func(e Event) bool { return e.Type == EventAssistant && strings.HasPrefix(e.Text, "next:") })
		if next.Text != "next: joined" {
			t.Fatalf("held prompts were not joined into one turn: %q", next.Text)
		}
		second := events.next(EventResult)
		if first.Turn != 1 || second.Turn != 2 {
			t.Fatalf("turns: first %d second %d", first.Turn, second.Turn)
		}
	})

	t.Run("exit drops held prompts", func(t *testing.T) {
		script := `read -r a; echo '{"type":"assistant","text":"x"}'; read -r b; exit 0`
		sess, events := start(t, FakeDriver{Script: script, NoSteer: true})
		if _, err := Send(ctx, sess, Prompt{Text: "go"}); err != nil {
			t.Fatal(err)
		}
		events.next(EventAssistant)
		if d, err := Send(ctx, sess, Prompt{Text: "lost"}); err != nil || d != DeliveryHeld {
			t.Fatalf("held send: %q %v", d, err)
		}
		if err := sess.(*fakeSession).WriteFrame([]byte(`{"type":"noop"}`)); err != nil {
			t.Fatal(err)
		}
		dropped := events.nextWhere(func(e Event) bool { return e.Type == EventError && e.Code == CodeHeldPromptDropped })
		if dropped.Text != "lost" {
			t.Fatalf("dropped text: %q", dropped.Text)
		}
		events.next(EventExit)
	})
}
