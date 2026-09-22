// Package jsonrpc is the JSON-RPC 2.0 client shared by the harness drivers
// that speak it (Codex app-server, ACP agents).
//
// It exists so the two drivers cannot drift on the parts that are easy to get
// wrong: request ids, waiter lifetime, and the exact echo of a peer's id back
// in a reply.
package jsonrpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Version is the JSON-RPC protocol version every frame carries.
const Version = "2.0"

// Error is a JSON-RPC error object.
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if len(e.Data) > 0 {
		return fmt.Sprintf("jsonrpc error %d: %s (%s)", e.Code, e.Message, string(e.Data))
	}
	return fmt.Sprintf("jsonrpc error %d: %s", e.Code, e.Message)
}

// ErrTransportClosed reports a call attempted after the transport died.
var ErrTransportClosed = errors.New("jsonrpc: transport closed")

// ID is a JSON-RPC id. It keeps the exact wire form so a reply echoes what the
// peer sent: a number stays a number, a string stays a quoted string.
type ID struct {
	raw json.RawMessage
}

// NumberID builds an id from an integer.
func NumberID(n int64) ID {
	b, _ := json.Marshal(n)
	return ID{raw: b}
}

// StringID builds an id from a string.
func StringID(s string) ID {
	b, _ := json.Marshal(s)
	return ID{raw: b}
}

// ParseID rebuilds an id from its wire form. Use it when an id travels
// through a layer that keeps only text, so the reply echoes the same type the
// peer sent.
func ParseID(raw []byte) (ID, error) {
	v := bytes.TrimSpace(raw)
	if len(v) == 0 {
		return ID{}, errors.New("jsonrpc: empty id")
	}
	if v[0] == '"' {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return ID{}, fmt.Errorf("jsonrpc: bad string id: %w", err)
		}
		return ID{raw: append(json.RawMessage(nil), v...)}, nil
	}
	var n json.Number
	if err := json.Unmarshal(v, &n); err != nil {
		return ID{}, errors.New("jsonrpc: id must be a string or a number")
	}
	return ID{raw: append(json.RawMessage(nil), v...)}, nil
}

// IsZero reports an absent id (a notification).
func (i ID) IsZero() bool { return len(i.raw) == 0 }

// MarshalJSON echoes the wire form.
func (i ID) MarshalJSON() ([]byte, error) {
	if len(i.raw) == 0 {
		return []byte("null"), nil
	}
	return i.raw, nil
}

// UnmarshalJSON records the wire form.
func (i *ID) UnmarshalJSON(b []byte) error {
	i.raw = append(i.raw[:0], b...)
	return nil
}

// Raw returns the id as it appeared on the wire.
func (i ID) Raw() json.RawMessage { return i.raw }

// String renders the id for logs: strings lose their quotes, numbers keep
// their digits.
func (i ID) String() string {
	if len(i.raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(i.raw, &s) == nil {
		return s
	}
	return string(i.raw)
}

// key is the canonical waiter key. The kind prefix keeps the number 7 and the
// string "7" apart.
func (i ID) key() string {
	if len(i.raw) == 0 {
		return ""
	}
	if len(i.raw) > 0 && i.raw[0] == '"' {
		return "s:" + i.String()
	}
	return "n:" + string(i.raw)
}

// Kind classifies one incoming frame.
type Kind int

const (
	// KindUnknown is a frame that is not a JSON-RPC message.
	KindUnknown Kind = iota
	// KindResponse answers a request this client sent.
	KindResponse
	// KindRequest is a peer request; the caller must reply.
	KindRequest
	// KindNotification is a peer notification; no reply is owed.
	KindNotification
)

// Message is one decoded JSON-RPC frame.
type Message struct {
	// Raw is the verbatim frame. Handle fills it after the decode.
	Raw    json.RawMessage `json:"-"`
	ID     ID              `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Err    *Error          `json:"error"`
}

// Client is a JSON-RPC 2.0 client over one line-delimited transport.
type Client struct {
	write   func([]byte) error
	timeout time.Duration

	mu      sync.Mutex
	next    int64
	waiters map[string]chan Message
	dead    error
}

// New builds a client that writes frames with write.
func New(write func([]byte) error, timeout time.Duration) *Client {
	return &Client{write: write, timeout: timeout, waiters: map[string]chan Message{}}
}

// SetWrite replaces the frame writer (the child's stdin arrives after the
// client is constructed).
func (c *Client) SetWrite(write func([]byte) error) {
	c.mu.Lock()
	c.write = write
	c.mu.Unlock()
}

// NextID returns a fresh numeric id from the shared counter.
func (c *Client) NextID() ID {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.next++
	return NumberID(c.next)
}

// Pending is the number of unanswered requests.
func (c *Client) Pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.waiters)
}

// Dead returns the transport failure, if any.
func (c *Client) Dead() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dead
}

// Fail marks the transport dead and releases every waiter. It is idempotent.
func (c *Client) Fail(err error) {
	if err == nil {
		err = ErrTransportClosed
	}
	c.mu.Lock()
	if c.dead != nil {
		c.mu.Unlock()
		return
	}
	c.dead = err
	waiters := c.waiters
	c.waiters = map[string]chan Message{}
	c.mu.Unlock()
	for _, ch := range waiters {
		close(ch)
	}
}

// send writes one frame. It reports ErrTransportClosed after Fail.
func (c *Client) send(frame map[string]any) error {
	c.mu.Lock()
	write := c.write
	dead := c.dead
	c.mu.Unlock()
	if dead != nil {
		return dead
	}
	if write == nil {
		return ErrTransportClosed
	}
	b, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if err := write(b); err != nil {
		c.Fail(err)
		return err
	}
	return nil
}

// Notify sends a notification (no id, no reply).
func (c *Client) Notify(method string, params any) error {
	frame := map[string]any{"jsonrpc": Version, "method": method}
	if params != nil {
		frame["params"] = params
	}
	return c.send(frame)
}

// Call sends a request and waits for its response. The waiter is always
// removed: on reply, on timeout, on context end, and on send error.
func (c *Client) Call(ctx context.Context, method string, params any, timeout time.Duration) (json.RawMessage, error) {
	id := c.NextID()
	ch, err := c.expect(id)
	if err != nil {
		return nil, err
	}
	defer c.forget(id)
	frame := map[string]any{"jsonrpc": Version, "id": id, "method": method}
	if params != nil {
		frame["params"] = params
	}
	if err := c.send(frame); err != nil {
		return nil, err
	}
	if timeout <= 0 {
		timeout = c.timeout
	}
	var timer *time.Timer
	var timeoutCh <-chan time.Time
	if timeout > 0 {
		timer = time.NewTimer(timeout)
		defer timer.Stop()
		timeoutCh = timer.C
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case msg, ok := <-ch:
		if !ok {
			if dead := c.Dead(); dead != nil {
				return nil, dead
			}
			return nil, ErrTransportClosed
		}
		if msg.Err != nil {
			return nil, msg.Err
		}
		return msg.Result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timeoutCh:
		return nil, fmt.Errorf("%s: timeout", method)
	}
}

// CallInto sends a request and decodes a successful result into out.
func (c *Client) CallInto(ctx context.Context, method string, params any, timeout time.Duration, out any) error {
	raw, err := c.Call(ctx, method, params, timeout)
	if err != nil {
		return err
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s: decode result: %w", method, err)
	}
	return nil
}

// Reply answers a peer request with a result.
func (c *Client) Reply(id ID, result any) error {
	if id.IsZero() {
		return errors.New("jsonrpc: reply without an id")
	}
	return c.send(map[string]any{"jsonrpc": Version, "id": id, "result": result})
}

// ReplyError answers a peer request with an error object.
func (c *Client) ReplyError(id ID, code int, message string) error {
	if id.IsZero() {
		return errors.New("jsonrpc: error reply without an id")
	}
	return c.send(map[string]any{
		"jsonrpc": Version, "id": id,
		"error": map[string]any{"code": code, "message": message},
	})
}

// expect registers a waiter for id.
func (c *Client) expect(id ID) (chan Message, error) {
	key := id.key()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dead != nil {
		return nil, c.dead
	}
	ch := make(chan Message, 1)
	c.waiters[key] = ch
	return ch, nil
}

// forget drops a waiter. The channel stays valid for a racing delivery.
func (c *Client) forget(id ID) {
	c.mu.Lock()
	delete(c.waiters, id.key())
	c.mu.Unlock()
}

// Handle decodes one incoming frame and routes it. A response is delivered to
// its waiter; an unknown or already-abandoned id is ignored.
func (c *Client) Handle(line []byte) (Kind, Message) {
	var m Message
	if err := json.Unmarshal(bytes.TrimSpace(line), &m); err != nil {
		return KindUnknown, Message{}
	}
	m.Raw = append(json.RawMessage(nil), line...)
	if m.Method != "" {
		if m.ID.IsZero() {
			return KindNotification, m
		}
		return KindRequest, m
	}
	if m.ID.IsZero() {
		return KindUnknown, m
	}
	key := m.ID.key()
	c.mu.Lock()
	ch, ok := c.waiters[key]
	if ok {
		delete(c.waiters, key)
	}
	c.mu.Unlock()
	if ok {
		ch <- m
	}
	return KindResponse, m
}

// UnmarshalParams decodes a message's params into out.
func (m Message) UnmarshalParams(out any) error {
	if len(m.Params) == 0 {
		return nil
	}
	return json.Unmarshal(m.Params, out)
}
