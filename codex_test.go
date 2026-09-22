package agentwire

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/trebi-ai/agent-wire/internal/proc"
	"github.com/trebi-ai/agent-wire/internal/wire"
)

// coaTestRuntime builds a runtime with no ledger and no shared server.
func coaTestRuntime() *Runtime {
	return New(Options{ClientName: "agentwire-test", ClientVersion: "0.0.0"})
}

// coaTestFrame is one frame a scripted peer received. It keeps the raw bytes,
// so a test can check the exact JSON types on the wire.
type coaTestFrame struct {
	raw []byte
	m   map[string]any
}

// method is the frame method. A response has none.
func (f coaTestFrame) method() string {
	s, _ := f.m["method"].(string)
	return s
}

// params are the frame params.
func (f coaTestFrame) params() map[string]any {
	p, _ := f.m["params"].(map[string]any)
	return p
}

// has reports whether the frame carries the key.
func (f coaTestFrame) has(key string) bool {
	_, ok := f.m[key]
	return ok
}

// coaTestPeer is a scripted peer for one in-memory wire session. It records
// every frame the session writes and answers through a per-test handler.
//
// The handler runs on the reader goroutine. It may write frames, and it must
// not call the test's Fatal methods.
type coaTestPeer struct {
	t    *testing.T
	in   *io.PipeReader
	inW  *io.PipeWriter
	out  *io.PipeReader
	outW *io.PipeWriter

	seen chan coaTestFrame

	mu      sync.Mutex
	handler func(*coaTestPeer, coaTestFrame)
}

// coaTestNewPeer starts a scripted peer. The handler runs on the reader
// goroutine, so it must not call the test's Fatal methods.
func coaTestNewPeer(t *testing.T, handler func(*coaTestPeer, coaTestFrame)) *coaTestPeer {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	p := &coaTestPeer{
		t: t, in: inR, inW: inW, out: outR, outW: outW,
		seen: make(chan coaTestFrame, 256), handler: handler,
	}
	go p.loop()
	t.Cleanup(func() {
		_ = inW.Close()
		_ = inR.Close()
		_ = outW.Close()
		_ = outR.Close()
	})
	return p
}

// loop reads the frames the session writes and hands each one to the handler.
func (p *coaTestPeer) loop() {
	sc := bufio.NewScanner(p.in)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		var m map[string]any
		_ = json.Unmarshal(line, &m)
		frame := coaTestFrame{raw: line, m: m}
		p.seen <- frame
		p.mu.Lock()
		h := p.handler
		p.mu.Unlock()
		if h != nil && m != nil {
			h(p, frame)
		}
	}
}

// push writes one frame to the session's stdout. A []byte or json.RawMessage is
// written verbatim, so a test can send a frame that is not valid JSON.
func (p *coaTestPeer) push(v any) {
	var b []byte
	switch t := v.(type) {
	case []byte:
		b = t
	case json.RawMessage:
		b = t
	default:
		var err error
		b, err = json.Marshal(v)
		if err != nil {
			p.t.Errorf("coaTestPeer: encode: %v", err)
			return
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, err := p.outW.Write(append(b, '\n')); err != nil {
		p.t.Errorf("coaTestPeer: write: %v", err)
	}
}

// notify writes one notification.
func (p *coaTestPeer) notify(method string, params any) {
	p.push(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

// request writes one server to client request.
func (p *coaTestPeer) request(id any, method string, params any) {
	p.push(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
}

// reply answers a request.
func (p *coaTestPeer) reply(id any, result any) {
	p.push(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

// replyErr answers a request with a JSON-RPC error.
func (p *coaTestPeer) replyErr(id any, code int, message string) {
	p.push(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
}

// wait returns the next frame that matches.
func (p *coaTestPeer) wait(match func(coaTestFrame) bool) coaTestFrame {
	p.t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case f := <-p.seen:
			if match(f) {
				return f
			}
		case <-deadline:
			p.t.Fatal("coaTestPeer: timeout waiting for a frame")
		}
	}
}

// waitMethod returns the next frame with the wanted method.
func (p *coaTestPeer) waitMethod(method string) coaTestFrame {
	p.t.Helper()
	return p.wait(func(f coaTestFrame) bool { return f.method() == method })
}

// waitReply returns the next frame that answers a request.
func (p *coaTestPeer) waitReply() coaTestFrame {
	p.t.Helper()
	return p.wait(func(f coaTestFrame) bool { return f.has("result") || f.has("error") })
}

// waitFrame returns the next frame.
func (p *coaTestPeer) waitFrame() coaTestFrame {
	p.t.Helper()
	return p.wait(func(coaTestFrame) bool { return true })
}

// drain drops the frames received so far. The handshake frames are not part of
// the assertion that follows.
func (p *coaTestPeer) drain() {
	for {
		select {
		case <-p.seen:
		default:
			return
		}
	}
}

// coaTestEvents reads one session's event stream in order and buffers the
// events the test does not ask for yet.
type coaTestEvents struct {
	t   *testing.T
	ch  <-chan Event
	buf []Event
}

// coaTestWatch starts reading a session event stream.
func coaTestWatch(t *testing.T, ch <-chan Event) *coaTestEvents {
	t.Helper()
	return &coaTestEvents{t: t, ch: ch}
}

// next returns the first event of the wanted type and buffers the others.
func (e *coaTestEvents) next(want EventType) Event {
	e.t.Helper()
	return e.nextWhere(func(ev Event) bool { return ev.Type == want })
}

// nextWhere returns the first event that matches.
func (e *coaTestEvents) nextWhere(match func(Event) bool) Event {
	e.t.Helper()
	for i, ev := range e.buf {
		if match(ev) {
			e.buf = append(e.buf[:i], e.buf[i+1:]...)
			return ev
		}
	}
	deadline := time.After(3 * time.Second)
	for {
		select {
		case ev, ok := <-e.ch:
			if !ok {
				e.t.Fatalf("coaTestEvents: stream closed with %d buffered events: %+v", len(e.buf), e.buf)
			}
			if match(ev) {
				return ev
			}
			e.buf = append(e.buf, ev)
		case <-deadline:
			e.t.Fatalf("coaTestEvents: timeout with %d buffered events: %+v", len(e.buf), e.buf)
		}
	}
}

// coaTestCodexHandshake answers the codex app-server handshake.
func coaTestCodexHandshake(p *coaTestPeer, m coaTestFrame) {
	switch m.method() {
	case "initialize":
		p.reply(m.m["id"], map[string]any{"userAgent": "codex-test"})
	case "thread/start":
		p.reply(m.m["id"], map[string]any{"threadId": "thr_1"})
	case "thread/resume":
		p.reply(m.m["id"], map[string]any{"threadId": m.params()["threadId"]})
	case "turn/start":
		p.reply(m.m["id"], map[string]any{"turnId": "turn_1"})
	}
}

// coaTestStartCodex opens an in-memory codex session against a scripted peer.
func coaTestStartCodex(t *testing.T, req StartRequest, handler func(*coaTestPeer, coaTestFrame)) (*coaTestPeer, *wire.Session, *codexProtocol) {
	t.Helper()
	if req.Harness == "" {
		req.Harness = Codex
	}
	if req.HandshakeTimeout <= 0 {
		req.HandshakeTimeout = 2 * time.Second
	}
	peer := coaTestNewPeer(t, handler)
	proto := newCodexProtocol(coaTestRuntime(), req, &launch{})
	s := wire.NewMemorySession(string(Codex), proto, peer.inW, peer.out, context.Background(), wire.Config{})
	return peer, s, proto
}

// TestCodexHandshakeAndTurn drives the handshake, the turn status stream, the
// agent text delta, and every terminal result of one codex turn.
func TestCodexHandshakeAndTurn(t *testing.T) {
	peer, s, _ := coaTestStartCodex(t, StartRequest{Model: "gpt-5"}, coaTestCodexHandshake)
	events := coaTestWatch(t, s.Events())

	// The handshake event carries the thread id the server reported.
	init := events.next(EventInit)
	if init.SessionID != "thr_1" {
		t.Fatalf("init event: %+v", init)
	}
	if s.ID() != "thr_1" {
		t.Fatalf("session id: %q", s.ID())
	}
	start := peer.waitMethod("thread/start")
	if got := start.params()["cwd"]; got == nil {
		t.Fatalf("thread/start params: %+v", start.params())
	}
	if start.params()["approvalPolicy"] != "on-request" {
		t.Fatalf("thread/start approvalPolicy: %+v", start.params())
	}

	// A thread/started notification reports the same thread id.
	peer.notify("thread/started", map[string]any{"thread": map[string]any{"id": "thr_1"}})
	note := events.next(EventInit)
	if note.SessionID != "thr_1" {
		t.Fatalf("thread/started event: %+v", note)
	}

	peer.notify("turn/started", map[string]any{"turn": map[string]any{"id": "turn_1"}})
	status := events.next(EventStatus)
	if status.Status != StatusRunning {
		t.Fatalf("turn/started status: %+v", status)
	}

	peer.notify("item/agentMessage/delta", map[string]any{"delta": "po"})
	delta := events.next(EventAssistant)
	if delta.Text != "po" || !delta.Delta {
		t.Fatalf("delta event: %+v", delta)
	}

	peer.notify("turn/completed", map[string]any{"turn": map[string]any{"id": "turn_1", "status": "completed"}})
	res := events.next(EventResult)
	if res.Result == nil || res.Result.IsError || res.Result.Subtype != "success" {
		t.Fatalf("completed result: %+v", res.Result)
	}

	peer.notify("turn/completed", map[string]any{"turn": map[string]any{
		"id": "turn_2", "status": "failed",
		"error": map[string]any{"message": "model overloaded"},
	}})
	failed := events.next(EventResult)
	if failed.Result == nil || !failed.Result.IsError {
		t.Fatalf("failed result: %+v", failed.Result)
	}
	if failed.Result.EndReason == "" {
		t.Fatalf("failed result has no end reason: %+v", failed.Result)
	}
	if !strings.Contains(failed.Result.Text, "overloaded") {
		t.Fatalf("failed result text: %q", failed.Result.Text)
	}

	peer.notify("error", map[string]any{"message": "stream error 503"})
	errEv := events.next(EventError)
	if !strings.HasPrefix(errEv.Code, "codex.") {
		t.Fatalf("error code: %q", errEv.Code)
	}
}

// TestCodexItemEvents maps the codex item notifications to tool events.
func TestCodexItemEvents(t *testing.T) {
	peer, s, _ := coaTestStartCodex(t, StartRequest{}, coaTestCodexHandshake)
	events := coaTestWatch(t, s.Events())

	peer.notify("item/started", map[string]any{"item": map[string]any{
		"id": "i1", "type": "commandExecution", "command": "ls -la", "aggregatedOutput": "total 0",
	}})
	started := events.next(EventTool)
	if started.Tool == nil || started.Tool.Kind != ToolExec || started.Tool.Status != "started" {
		t.Fatalf("command started: %+v", started.Tool)
	}
	if started.Tool.Input != "ls -la" {
		t.Fatalf("command input: %+v", started.Tool)
	}

	peer.notify("item/completed", map[string]any{"item": map[string]any{
		"id": "i1", "type": "commandExecution", "command": "ls -la", "exitCode": 2,
	}})
	completed := events.next(EventTool)
	if completed.Tool == nil || completed.Tool.Status != "failed" {
		t.Fatalf("command completed: %+v", completed.Tool)
	}

	peer.notify("item/started", map[string]any{"item": map[string]any{
		"id": "i2", "type": "fileChange",
		"changes": []any{
			map[string]any{"path": "/tmp/coa-a.txt"},
			map[string]any{"path": "/tmp/coa-b.txt"},
		},
	}})
	edit := events.next(EventTool)
	if edit.Tool == nil || edit.Tool.Kind != ToolEdit {
		t.Fatalf("fileChange tool: %+v", edit.Tool)
	}
	if !slices.Equal(edit.Tool.Paths, []string{"/tmp/coa-a.txt", "/tmp/coa-b.txt"}) {
		t.Fatalf("fileChange paths: %v", edit.Tool.Paths)
	}

	peer.notify("item/started", map[string]any{"item": map[string]any{
		"id": "i3", "type": "mcpToolCall", "tool": "echo", "arguments": map[string]any{"text": "hi"},
	}})
	mcp := events.next(EventTool)
	if mcp.Tool == nil || mcp.Tool.Kind != ToolMCP || mcp.Tool.Name != "mcp:echo" {
		t.Fatalf("mcp tool: %+v", mcp.Tool)
	}
}

// TestCodexUsage maps the token usage notification.
func TestCodexUsage(t *testing.T) {
	peer, s, _ := coaTestStartCodex(t, StartRequest{}, coaTestCodexHandshake)
	events := coaTestWatch(t, s.Events())

	peer.notify("thread/tokenUsage/updated", map[string]any{"tokenUsage": map[string]any{
		"total": map[string]any{"inputTokens": 11, "outputTokens": 4, "cachedInputTokens": 3},
	}})
	usage := events.next(EventUsage)
	if usage.Usage == nil {
		t.Fatalf("usage event: %+v", usage)
	}
	if usage.Usage.Input != 11 || usage.Usage.Output != 4 || usage.Usage.CacheRead != 3 {
		t.Fatalf("usage counters: %+v", usage.Usage)
	}
}

// TestCodexApprovalRoundtrip maps a server approval request to a permission
// event and answers it with the same id type the server used.
func TestCodexApprovalRoundtrip(t *testing.T) {
	peer, s, _ := coaTestStartCodex(t, StartRequest{}, coaTestCodexHandshake)
	events := coaTestWatch(t, s.Events())
	peer.drain()

	peer.request(7, "item/commandExecution/requestApproval", map[string]any{"command": "touch /tmp/coa"})
	perm := events.next(EventPermission)
	if perm.Permission == nil {
		t.Fatalf("permission event: %+v", perm)
	}
	if perm.Permission.ID != "7" {
		t.Fatalf("permission id: %q", perm.Permission.ID)
	}
	if perm.Permission.Kind != ToolExec {
		t.Fatalf("permission kind: %q", perm.Permission.Kind)
	}
	if !strings.Contains(perm.Permission.Question, "touch /tmp/coa") {
		t.Fatalf("permission question: %q", perm.Permission.Question)
	}

	if err := s.AnswerPermission(context.Background(), perm.Permission.ID, Decision{Allow: true}); err != nil {
		t.Fatalf("answer permission: %v", err)
	}
	reply := peer.waitReply()
	var decoded struct {
		ID     json.Number    `json:"id"`
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(reply.raw, &decoded); err != nil {
		t.Fatalf("decision frame is not a numeric id reply: %s (%v)", reply.raw, err)
	}
	if decoded.ID.String() != "7" {
		t.Fatalf("decision id: %q", decoded.ID)
	}
	if decoded.Result["decision"] != "accept" {
		t.Fatalf("decision body: %s", reply.raw)
	}
}

// TestCodexAutoEditApproval proves the auto_edit policy answers a file change
// in the library: no permission event, and an accept reply on the wire.
func TestCodexAutoEditApproval(t *testing.T) {
	req := StartRequest{Permissions: PermissionPolicy{Mode: PermissionAutoEdit}}
	peer, s, _ := coaTestStartCodex(t, req, coaTestCodexHandshake)
	events := coaTestWatch(t, s.Events())
	events.next(EventInit)
	peer.drain()

	peer.request(8, "item/fileChange/requestApproval", map[string]any{"changes": []any{map[string]any{"path": "/tmp/coa"}}})
	peer.notify("thread/tokenUsage/updated", map[string]any{"tokenUsage": map[string]any{
		"total": map[string]any{"inputTokens": 5},
	}})
	// The marker arrives first only when no permission event was emitted.
	events.next(EventUsage)
	if len(events.buf) != 0 {
		t.Fatalf("unexpected events before the marker: %+v", events.buf)
	}

	reply := peer.waitReply()
	var decoded struct {
		ID     json.Number    `json:"id"`
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(reply.raw, &decoded); err != nil {
		t.Fatalf("auto edit reply: %s (%v)", reply.raw, err)
	}
	if decoded.ID.String() != "8" || decoded.Result["decision"] != "accept" {
		t.Fatalf("auto edit reply: %s", reply.raw)
	}
}

// TestCodexUnsupportedRequest proves a server request the library cannot
// render a reply for is refused instead of left unanswered.
func TestCodexUnsupportedRequest(t *testing.T) {
	peer, s, _ := coaTestStartCodex(t, StartRequest{}, coaTestCodexHandshake)
	events := coaTestWatch(t, s.Events())
	events.next(EventInit)
	peer.drain()

	peer.request(9, "session/not_an_approval", map[string]any{})
	peer.notify("thread/tokenUsage/updated", map[string]any{"tokenUsage": map[string]any{
		"total": map[string]any{"inputTokens": 5},
	}})
	events.next(EventUsage)
	if len(events.buf) != 0 {
		t.Fatalf("unexpected events before the marker: %+v", events.buf)
	}

	var decoded struct {
		ID    json.Number    `json:"id"`
		Error map[string]any `json:"error"`
	}
	reply := peer.waitFrame()
	if err := json.Unmarshal(reply.raw, &decoded); err != nil {
		t.Fatalf("refusal frame: %s (%v)", reply.raw, err)
	}
	if decoded.ID.String() != "9" || decoded.Error == nil {
		t.Fatalf("refusal frame: %s", reply.raw)
	}
}

// coaTestHelperPeer answers the first frame with a JSON-RPC error and exits.
// TestCodexHandshakeFailure runs it by re-executing the test binary.
func coaTestHelperPeer() {
	sc := bufio.NewScanner(os.Stdin)
	if !sc.Scan() {
		os.Exit(0)
	}
	var m map[string]any
	_ = json.Unmarshal(sc.Bytes(), &m)
	fmt.Fprintf(os.Stdout, "{\"jsonrpc\":\"2.0\",\"id\":%v,\"error\":{\"code\":-32603,\"message\":\"initialize failed\"}}\n", m["id"])
	os.Exit(0)
}

// TestCodexHandshakeFailure proves a failed handshake is an error, never a
// live session.
func TestCodexHandshakeFailure(t *testing.T) {
	if os.Getenv("COA_TEST_HELPER_PEER") == "1" {
		coaTestHelperPeer()
		return
	}

	t.Run("wire start returns the error", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		p, err := proc.Start(ctx, proc.Opts{
			Path: os.Args[0],
			Args: []string{"-test.run=TestCodexHandshakeFailure"},
			Env:  append(os.Environ(), "COA_TEST_HELPER_PEER=1"),
		})
		if err != nil {
			t.Fatalf("helper start: %v", err)
		}
		req := StartRequest{Harness: Codex, HandshakeTimeout: 5 * time.Second}
		proto := newCodexProtocol(coaTestRuntime(), req, &launch{})
		s, err := wire.Start(ctx, string(Codex), p, proto, wire.Config{})
		if err == nil {
			t.Fatal("wire.Start accepted a failed handshake")
		}
		if s != nil {
			t.Fatal("wire.Start returned a session for a failed handshake")
		}
		if !strings.Contains(err.Error(), "initialize failed") {
			t.Fatalf("handshake error: %v", err)
		}
	})

	t.Run("memory session reports the error", func(t *testing.T) {
		handler := func(p *coaTestPeer, m coaTestFrame) {
			p.replyErr(m.m["id"], -32603, "initialize failed")
		}
		_, s, proto := coaTestStartCodex(t, StartRequest{}, handler)
		events := coaTestWatch(t, s.Events())
		ev := events.nextWhere(func(ev Event) bool { return ev.Code == "handshake_failed" })
		if ev.Type != EventError || ev.Error == "" {
			t.Fatalf("handshake event: %+v", ev)
		}
		if s.Err() == nil {
			t.Fatal("session recorded no error")
		}
		if _, err := proto.Handshake(context.Background(), nil); err == nil {
			t.Fatal("direct handshake returned no error")
		}
	})
}

// TestCodexEffortFallback proves effort is sent on turn/start and dropped for
// the rest of the session when the pin answers with an unknown-field error.
func TestCodexEffortFallback(t *testing.T) {
	var starts atomic.Int32
	handler := func(p *coaTestPeer, m coaTestFrame) {
		switch m.method() {
		case "initialize":
			p.reply(m.m["id"], map[string]any{"userAgent": "codex-test"})
		case "thread/start":
			p.reply(m.m["id"], map[string]any{"threadId": "thr_1"})
		case "turn/start":
			if starts.Add(1) == 1 {
				p.replyErr(m.m["id"], -32602, "unknown field `effort`")
				return
			}
			p.reply(m.m["id"], map[string]any{"turnId": "turn_1"})
		}
	}
	peer, s, _ := coaTestStartCodex(t, StartRequest{Effort: "high"}, handler)
	peer.drain()

	if err := s.Prompt(context.Background(), Prompt{Text: "say pong"}); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	first := peer.waitMethod("turn/start")
	if first.params()["effort"] != "high" {
		t.Fatalf("first turn/start params: %+v", first.params())
	}
	second := peer.waitMethod("turn/start")
	if _, ok := second.params()["effort"]; ok {
		t.Fatalf("retry carries effort: %+v", second.params())
	}
	if got := starts.Load(); got != 2 {
		t.Fatalf("turn/start count: %d", got)
	}
}
