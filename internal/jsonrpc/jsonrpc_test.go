package jsonrpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

// recorder records the frames a client writes. The write happens on the test
// goroutine and on the goroutine that runs Call, so a mutex guards the slice.
type recorder struct {
	mu     sync.Mutex
	frames [][]byte
}

func (r *recorder) write(b []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.frames = append(r.frames, append([]byte(nil), b...))
	return nil
}

// raw waits for the frame at index n and returns its bytes.
func (r *recorder) raw(t *testing.T, n int) []byte {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		r.mu.Lock()
		if len(r.frames) > n {
			b := append([]byte(nil), r.frames[n]...)
			r.mu.Unlock()
			return b
		}
		r.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatalf("frame %d was never written", n)
		}
		time.Sleep(time.Millisecond)
	}
}

// frame waits for the frame at index n and decodes it as a JSON object.
func (r *recorder) frame(t *testing.T, n int) map[string]json.RawMessage {
	t.Helper()
	b := r.raw(t, n)
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("frame %d is not a JSON object: %v (%s)", n, err, b)
	}
	return m
}

// newHarness builds a client whose writes land in a recorder.
func newHarness(t *testing.T) (*Client, *recorder) {
	t.Helper()
	r := &recorder{}
	return New(r.write, 0), r
}

// waitPending blocks until the client holds want waiters, or fails.
func waitPending(t *testing.T, c *Client, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for c.Pending() != want {
		if time.Now().After(deadline) {
			t.Fatalf("pending = %d, want %d", c.Pending(), want)
		}
		time.Sleep(time.Millisecond)
	}
}

// responseLine builds a response frame that carries id.
func responseLine(id json.RawMessage, result string) []byte {
	return []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":%s}`, id, result))
}

// errorLine builds an error response frame that carries id.
func errorLine(id json.RawMessage, code int, message string) []byte {
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(id),
		"error":   map[string]any{"code": code, "message": message},
	})
	return b
}

func TestCallSendsNumericID(t *testing.T) {
	c, r := newHarness(t)
	done := make(chan error, 1)
	var got json.RawMessage
	go func() {
		raw, err := c.Call(context.Background(), "session/new", map[string]any{"cwd": "/tmp"}, 0)
		got = raw
		done <- err
	}()

	frame := r.frame(t, 0)
	id := frame["id"]
	if len(id) == 0 || id[0] == '"' {
		t.Fatalf("id on the wire = %s, want a JSON number", id)
	}
	var n float64
	if err := json.Unmarshal(id, &n); err != nil {
		t.Fatalf("id on the wire is not a JSON number: %v (%s)", err, id)
	}
	if n < 1 || n != math.Trunc(n) {
		t.Fatalf("id = %v, want a positive integer", n)
	}
	var method string
	if err := json.Unmarshal(frame["method"], &method); err != nil || method != "session/new" {
		t.Fatalf("method on the wire = %s (err %v), want %q", frame["method"], err, "session/new")
	}

	if kind, _ := c.Handle(responseLine(id, `{"ok":true}`)); kind != KindResponse {
		t.Fatalf("kind = %v, want KindResponse", kind)
	}
	if err := <-done; err != nil {
		t.Fatalf("Call: %v", err)
	}
	var out struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(got, &out); err != nil || !out.OK {
		t.Fatalf("result = %s (err %v)", got, err)
	}
	if c.Pending() != 0 {
		t.Fatalf("pending = %d after the reply, want 0", c.Pending())
	}
}

func TestReplyEchoesStringID(t *testing.T) {
	c, r := newHarness(t)
	line := []byte(`{"jsonrpc":"2.0","id":"req-7","method":"session/request_permission","params":{"sessionId":"ses_1"}}`)
	kind, msg := c.Handle(line)
	if kind != KindRequest {
		t.Fatalf("kind = %v, want KindRequest", kind)
	}
	if got := msg.ID.String(); got != "req-7" {
		t.Fatalf("id = %q, want req-7", got)
	}
	if err := c.Reply(msg.ID, map[string]any{"outcome": "selected"}); err != nil {
		t.Fatalf("Reply: %v", err)
	}

	// The whole reply must be valid JSON, and the id must stay a quoted string.
	var reply struct {
		ID     json.RawMessage `json:"id"`
		Result struct {
			Outcome string `json:"outcome"`
		} `json:"result"`
	}
	if err := json.Unmarshal(r.raw(t, 0), &reply); err != nil {
		t.Fatalf("reply is not valid JSON: %v (%s)", err, r.raw(t, 0))
	}
	var id any
	if err := json.Unmarshal(reply.ID, &id); err != nil {
		t.Fatalf("reply id is not valid JSON: %v (%s)", err, reply.ID)
	}
	if s, ok := id.(string); !ok || s != "req-7" {
		t.Fatalf("reply id = %#v, want the quoted string %q", id, "req-7")
	}
	if reply.Result.Outcome != "selected" {
		t.Fatalf("result = %+v", reply.Result)
	}
}

func TestCallTimeoutRemovesWaiter(t *testing.T) {
	c, _ := newHarness(t)
	_, err := c.Call(context.Background(), "turn/start", nil, 20*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("err = %v, want a timeout error", err)
	}
	if c.Pending() != 0 {
		t.Fatalf("pending = %d after the timeout, want 0", c.Pending())
	}
}

func TestCallContextCancelRemovesWaiter(t *testing.T) {
	c, _ := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.Call(ctx, "initialize", nil, 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if c.Pending() != 0 {
		t.Fatalf("pending = %d after the cancel, want 0", c.Pending())
	}
}

func TestCallSendErrorRemovesWaiter(t *testing.T) {
	boom := errors.New("stdin is closed")
	c := New(func([]byte) error { return boom }, 0)
	_, err := c.Call(context.Background(), "session/new", nil, 0)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	if c.Pending() != 0 {
		t.Fatalf("pending = %d after the send error, want 0", c.Pending())
	}
}

func TestHandleIgnoresUnknownResponseID(t *testing.T) {
	c, r := newHarness(t)
	kind, msg := c.Handle([]byte(`{"jsonrpc":"2.0","id":999,"result":{"stale":true}}`))
	if kind != KindResponse {
		t.Fatalf("kind = %v, want KindResponse", kind)
	}
	if got := msg.ID.String(); got != "999" {
		t.Fatalf("id = %q, want 999", got)
	}
	if c.Pending() != 0 {
		t.Fatalf("pending = %d, want 0", c.Pending())
	}

	// The stale frame must not disturb a later call.
	done := make(chan error, 1)
	go func() {
		_, err := c.Call(context.Background(), "session/new", nil, 0)
		done <- err
	}()
	frame := r.frame(t, 0)
	if kind, _ := c.Handle(responseLine(frame["id"], `{"ok":true}`)); kind != KindResponse {
		t.Fatalf("kind = %v, want KindResponse", kind)
	}
	if err := <-done; err != nil {
		t.Fatalf("Call after the stale frame: %v", err)
	}
}

func TestFailReleasesEveryWaiter(t *testing.T) {
	c, _ := newHarness(t)
	boom := errors.New("boom")
	const calls = 2
	errs := make(chan error, calls)
	for range calls {
		go func() {
			_, err := c.Call(context.Background(), "session/prompt", nil, 0)
			errs <- err
		}()
	}
	waitPending(t, c, calls)
	c.Fail(boom)
	for range calls {
		if err := <-errs; !errors.Is(err, boom) {
			t.Fatalf("err = %v, want %v", err, boom)
		}
	}
	if c.Pending() != 0 {
		t.Fatalf("pending = %d after Fail, want 0", c.Pending())
	}
	if !errors.Is(c.Dead(), boom) {
		t.Fatalf("dead = %v, want %v", c.Dead(), boom)
	}

	// Fail is idempotent: the first error stays.
	c.Fail(errors.New("second"))
	if !errors.Is(c.Dead(), boom) {
		t.Fatalf("dead = %v after a second Fail, want %v", c.Dead(), boom)
	}
	if err := c.Notify("session/cancel", nil); !errors.Is(err, boom) {
		t.Fatalf("send after Fail = %v, want %v", err, boom)
	}
}

func TestReplyErrorForStringID(t *testing.T) {
	c, r := newHarness(t)
	if err := c.ReplyError(StringID("acp-3"), -32601, "method not found"); err != nil {
		t.Fatalf("ReplyError: %v", err)
	}
	var frame struct {
		ID    string `json:"id"`
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(r.raw(t, 0), &frame); err != nil {
		t.Fatalf("error reply is not valid JSON: %v (%s)", err, r.raw(t, 0))
	}
	if frame.ID != "acp-3" {
		t.Fatalf("id = %q, want acp-3", frame.ID)
	}
	if frame.Error.Code != -32601 || frame.Error.Message != "method not found" {
		t.Fatalf("error = %+v, want code -32601 and message %q", frame.Error, "method not found")
	}
}

func TestHandleClassification(t *testing.T) {
	c, _ := newHarness(t)
	cases := []struct {
		name string
		line string
		want Kind
	}{
		{"notification", `{"jsonrpc":"2.0","method":"session/update","params":{}}`, KindNotification},
		{"request", `{"jsonrpc":"2.0","id":4,"method":"session/request_permission","params":{}}`, KindRequest},
		{"string id request", `{"jsonrpc":"2.0","id":"p1","method":"fs/read_text_file","params":{}}`, KindRequest},
		{"garbage", `not json at all`, KindUnknown},
		{"empty", ``, KindUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind, _ := c.Handle([]byte(tc.line))
			if kind != tc.want {
				t.Fatalf("Handle(%s) = %v, want %v", tc.line, kind, tc.want)
			}
		})
	}
}

func TestHandleDeliversResponseOnce(t *testing.T) {
	c, r := newHarness(t)
	results := make(chan json.RawMessage, 1)
	fails := make(chan error, 1)
	go func() {
		raw, err := c.Call(context.Background(), "initialize", nil, 0)
		if err != nil {
			fails <- err
			return
		}
		results <- raw
	}()

	line := responseLine(r.frame(t, 0)["id"], `{"ok":true}`)
	kind, msg := c.Handle(line)
	if kind != KindResponse {
		t.Fatalf("kind = %v, want KindResponse", kind)
	}
	if len(msg.Result) == 0 {
		t.Fatalf("delivered message carries no result: %+v", msg)
	}
	select {
	case err := <-fails:
		t.Fatalf("Call: %v", err)
	case raw := <-results:
		var out struct {
			OK bool `json:"ok"`
		}
		if err := json.Unmarshal(raw, &out); err != nil || !out.OK {
			t.Fatalf("result = %s (err %v)", raw, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Call never returned")
	}
	if c.Pending() != 0 {
		t.Fatalf("pending = %d after delivery, want 0", c.Pending())
	}

	// The waiter is gone. A second frame with the same id is ignored.
	if kind, _ := c.Handle(line); kind != KindResponse {
		t.Fatalf("second Handle = %v, want KindResponse", kind)
	}
	if c.Pending() != 0 {
		t.Fatalf("pending = %d after a second frame, want 0", c.Pending())
	}

	next := make(chan error, 1)
	go func() {
		_, err := c.Call(context.Background(), "session/new", nil, 0)
		next <- err
	}()
	if kind, _ := c.Handle(responseLine(r.frame(t, 1)["id"], `{}`)); kind != KindResponse {
		t.Fatalf("kind = %v, want KindResponse", kind)
	}
	if err := <-next; err != nil {
		t.Fatalf("Call after the duplicate frame: %v", err)
	}
}

func TestCallIntoDecodesResult(t *testing.T) {
	c, r := newHarness(t)
	var out struct {
		SessionID string   `json:"sessionId"`
		Modes     []string `json:"modes"`
	}
	done := make(chan error, 1)
	go func() {
		done <- c.CallInto(context.Background(), "session/new", map[string]any{"cwd": "/tmp"}, 0, &out)
	}()

	frame := r.frame(t, 0)
	if kind, _ := c.Handle(responseLine(frame["id"], `{"sessionId":"ses_9","modes":["ask","auto"]}`)); kind != KindResponse {
		t.Fatalf("kind = %v, want KindResponse", kind)
	}
	if err := <-done; err != nil {
		t.Fatalf("CallInto: %v", err)
	}
	if out.SessionID != "ses_9" || len(out.Modes) != 2 || out.Modes[1] != "auto" {
		t.Fatalf("out = %+v, want the decoded session result", out)
	}
}

func TestCallIntoReturnsRPCError(t *testing.T) {
	c, r := newHarness(t)
	var out map[string]any
	done := make(chan error, 1)
	go func() {
		done <- c.CallInto(context.Background(), "session/prompt", nil, 0, &out)
	}()

	frame := r.frame(t, 0)
	if kind, _ := c.Handle(errorLine(frame["id"], -32000, "usage limit reached")); kind != KindResponse {
		t.Fatalf("kind = %v, want KindResponse", kind)
	}
	err := <-done
	var rpcErr *Error
	if !errors.As(err, &rpcErr) {
		t.Fatalf("err = %v (%T), want a *jsonrpc.Error", err, err)
	}
	if rpcErr.Code != -32000 || rpcErr.Message != "usage limit reached" {
		t.Fatalf("err = %+v, want code -32000 and message %q", rpcErr, "usage limit reached")
	}
}
