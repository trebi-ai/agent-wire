package agentwire

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/trebi-ai/agent-wire/internal/proc"
)

// openCodeReadyTimeout bounds the serve health wait.
const openCodeReadyTimeout = 30 * time.Second

// openCodeServerIdle is how long a refcount-zero shared server lingers before
// shutdown.
const openCodeServerIdle = 60 * time.Second

// openCodeStreamRetry bounds the reconnect backoff of the shared event stream.
const (
	openCodeStreamRetryMin = 250 * time.Millisecond
	openCodeStreamRetryMax = 10 * time.Second
)

// openCodeManager owns the shared `opencode serve` processes of one Runtime,
// keyed by binary, non-run-scoped environment and extra arguments.
type openCodeManager struct {
	rt      *Runtime
	idle    time.Duration
	mu      sync.Mutex
	servers map[string]*openCodeServer
}

func newOpenCodeManager(rt *Runtime) *openCodeManager {
	return &openCodeManager{rt: rt, idle: openCodeServerIdle, servers: map[string]*openCodeServer{}}
}

// shutdown stops every managed server.
func (m *openCodeManager) shutdown() {
	m.mu.Lock()
	servers := m.servers
	m.servers = map[string]*openCodeServer{}
	m.mu.Unlock()
	for _, s := range servers {
		s.stop()
	}
}

// release drops one refcount and stops the server after the idle window.
func (m *openCodeManager) release(srv *openCodeServer) {
	srv.release(m.idle, func() {
		m.mu.Lock()
		if m.servers[srv.key] == srv {
			delete(m.servers, srv.key)
		}
		m.mu.Unlock()
		srv.stop()
	})
}

// openCodeServer is one runtime-owned `opencode serve` process.
type openCodeServer struct {
	key      string
	bin      string
	port     int
	username string
	password string
	proc     *proc.Proc
	argv     []string
	rt       *Runtime

	mu       sync.Mutex
	refs     int
	idle     *time.Timer
	sessions map[string]*openCodeSession
	stream   context.CancelFunc
	stopped  bool
}

// baseURL is the loopback origin.
func (s *openCodeServer) baseURL() string {
	return "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(s.port))
}

func (s *openCodeServer) acquire() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refs++
	if s.idle != nil {
		s.idle.Stop()
		s.idle = nil
	}
}

func (s *openCodeServer) release(idle time.Duration, onStop func()) {
	s.mu.Lock()
	s.refs--
	refs := s.refs
	if refs <= 0 && s.idle == nil {
		if idle <= 0 {
			idle = openCodeServerIdle
		}
		s.idle = time.AfterFunc(idle, func() {
			s.mu.Lock()
			stop := s.refs <= 0
			s.idle = nil
			s.mu.Unlock()
			if stop {
				onStop()
			}
		})
	}
	s.mu.Unlock()
}

// stop ends the shared stream, kills the child and drops the ledger row.
func (s *openCodeServer) stop() {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	cancel := s.stream
	s.stream = nil
	sessions := s.sessions
	s.sessions = map[string]*openCodeSession{}
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	for _, sess := range sessions {
		sess.serverGone("opencode serve stopped")
	}
	if s.rt != nil && s.rt.led != nil {
		s.rt.led.Remove("opencode:" + s.key)
	}
	if s.proc != nil {
		_ = s.proc.GracefulClose(proc.GracefulCloseTimeout)
	}
}

// subscribe adds a session to the routing table.
func (s *openCodeServer) subscribe(sess *openCodeSession) {
	s.mu.Lock()
	s.sessions[sess.id] = sess
	s.mu.Unlock()
}

// unsubscribe removes a session from the routing table.
func (s *openCodeServer) unsubscribe(id string) {
	s.mu.Lock()
	delete(s.sessions, id)
	s.mu.Unlock()
}

// lookup finds the session a bus event belongs to.
func (s *openCodeServer) lookup(id string) *openCodeSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[id]
}

// allSessions snapshots the routing table.
func (s *openCodeServer) allSessions() []*openCodeSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*openCodeSession, 0, len(s.sessions))
	for _, sess := range s.sessions {
		out = append(out, sess)
	}
	return out
}

// ensureStream starts the single event stream this server needs, whatever the
// number of sessions on it.
func (s *openCodeServer) ensureStream() {
	s.mu.Lock()
	if s.stream != nil || s.stopped {
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.stream = cancel
	s.mu.Unlock()
	go s.streamLoop(ctx)
}

// streamLoop keeps one /event connection alive and routes frames by session.
func (s *openCodeServer) streamLoop(ctx context.Context) {
	backoff := openCodeStreamRetryMin
	for ctx.Err() == nil {
		err := s.readStream(ctx)
		if ctx.Err() != nil {
			return
		}
		if s.proc != nil && s.proc.ExitCode() != -1 {
			// The child is gone; every session on it is gone too.
			for _, sess := range s.allSessions() {
				sess.serverGone(openCodeExitText(s.proc))
			}
			return
		}
		if err != nil {
			s.rt.log.Debug("opencode event stream ended", "error", err, "retry_in", backoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < openCodeStreamRetryMax {
			backoff *= 2
		}
	}
}

// readStream reads /event until the connection ends.
func (s *openCodeServer) readStream(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL()+"/event", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.SetBasicAuth(s.username, s.password)
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("opencode /event: HTTP %d", resp.StatusCode)
	}
	br := bufio.NewReaderSize(resp.Body, 64<<10)
	var data strings.Builder
	flush := func() {
		if data.Len() == 0 {
			return
		}
		frame := []byte(strings.TrimRight(data.String(), "\n"))
		data.Reset()
		s.dispatch(frame)
	}
	for {
		line, err := readSSELine(br, s.rt.maxFr)
		if err != nil {
			flush()
			return err
		}
		switch {
		case strings.HasPrefix(line, "event:"):
			// The event name is not needed: the payload carries its own type.
		case strings.HasPrefix(line, "data:"):
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			data.WriteByte('\n')
		case strings.TrimSpace(line) == "":
			flush()
		}
	}
}

// readSSELine reads one line with the same frame cap the wire uses. A line over
// the cap is discarded whole so the stream resynchronises.
func readSSELine(br *bufio.Reader, max int) (string, error) {
	if max <= 0 {
		max = 1 << 20
	}
	var buf []byte
	overflow := false
	for {
		chunk, err := br.ReadSlice('\n')
		if !overflow {
			if len(buf)+len(chunk) > max {
				overflow = true
				buf = nil
			} else {
				buf = append(buf, chunk...)
			}
		}
		switch {
		case err == nil:
			return strings.TrimRight(string(buf), "\r\n"), nil
		case errors.Is(err, bufio.ErrBufferFull):
		case errors.Is(err, io.EOF):
			if len(buf) > 0 && !overflow {
				return strings.TrimRight(string(buf), "\r\n"), nil
			}
			return "", io.EOF
		default:
			return "", err
		}
	}
}

// dispatch routes one bus frame to the session it names.
func (s *openCodeServer) dispatch(frame []byte) {
	var m map[string]any
	if json.Unmarshal(frame, &m) != nil {
		return
	}
	typ := str(m["type"])
	props, _ := m["properties"].(map[string]any)
	if props == nil {
		return
	}
	id := openCodeSessionID(props)
	if id == "" {
		return
	}
	if sess := s.lookup(id); sess != nil {
		sess.deliver(sess.parse(typ, props))
	}
}

// call runs one HTTP request against the server with directory and basic auth.
func (s *openCodeServer) call(ctx context.Context, method, path, directory string, body any, out any) error {
	var payload []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = b
	}
	reqURL := s.baseURL() + path
	if directory != "" {
		reqURL += "?" + url.Values{"directory": {directory}}.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, reqURL, strings.NewReader(string(payload)))
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if directory != "" {
		req.Header.Set("x-opencode-directory", directory)
	}
	req.SetBasicAuth(s.username, s.password)
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
			Data  struct {
				Message string `json:"message"`
			} `json:"data"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		msg := firstNonEmpty(e.Error, e.Data.Message, fmt.Sprintf("HTTP %d", resp.StatusCode))
		return fmt.Errorf("opencode %s %s: %s", method, path, msg)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// openCodeExitText describes why a serve child died.
func openCodeExitText(p *proc.Proc) string {
	code := p.ExitCode()
	if tail := strings.TrimSpace(p.Stderr()); tail != "" {
		return fmt.Sprintf("opencode serve exited with code %d: %s", code, proc.FirstLine(tail))
	}
	return fmt.Sprintf("opencode serve exited with code %d", code)
}

// openCodeFingerprint keys a shared server by the environment that changes what
// it can serve: binary, non-run-scoped variables, and extra arguments. Keys in
// Options.FingerprintIgnoreEnv and the server's own auth never split it.
func (m *openCodeManager) fingerprint(bin string, env map[string]string, extra []string) string {
	keys := make([]string, 0, len(env))
	for k := range env {
		if m.excluded(k) {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(bin)
	b.WriteByte('\x00')
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(env[k])
		b.WriteByte('\x00')
	}
	b.WriteString(strings.Join(extra, "\x00"))
	return b.String()
}

func (m *openCodeManager) excluded(key string) bool {
	switch key {
	case "OPENCODE_SERVER_PASSWORD", "OPENCODE_SERVER_USERNAME":
		return true
	}
	for _, k := range m.rt.opts.FingerprintIgnoreEnv {
		if strings.EqualFold(k, key) || k == "*" {
			return true
		}
	}
	return false
}

// acquireServer returns a live shared server, starting one when needed.
func (m *openCodeManager) acquireServer(ctx context.Context, bin string, env map[string]string, extraArgs []string) (*openCodeServer, error) {
	key := m.fingerprint(bin, env, extraArgs)
	m.mu.Lock()
	if s, ok := m.servers[key]; ok && s.alive() {
		m.mu.Unlock()
		s.acquire()
		return s, nil
	}
	m.mu.Unlock()

	s, err := m.startServer(ctx, key, bin, env, extraArgs)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	if existing, ok := m.servers[key]; ok && existing.alive() {
		// A concurrent acquire won the race; keep the first.
		m.mu.Unlock()
		s.stop()
		existing.acquire()
		return existing, nil
	}
	m.servers[key] = s
	m.mu.Unlock()
	return s, nil
}

func (s *openCodeServer) alive() bool {
	s.mu.Lock()
	stopped := s.stopped
	s.mu.Unlock()
	return !stopped && s.proc != nil && s.proc.ExitCode() == -1
}

func (m *openCodeManager) startServer(ctx context.Context, key, bin string, env map[string]string, extraArgs []string) (*openCodeServer, error) {
	childEnv := make(map[string]string, len(env)+2)
	for k, v := range env {
		childEnv[k] = v
	}
	password := randomToken()
	childEnv["OPENCODE_SERVER_PASSWORD"] = password
	if childEnv["OPENCODE_SERVER_USERNAME"] == "" {
		childEnv["OPENCODE_SERVER_USERNAME"] = "opencode"
	}
	// freePort has a check-then-bind race; retry on a lost bind.
	var lastErr error
	for attempt := range 3 {
		port, err := freePort()
		if err != nil {
			return nil, err
		}
		args := append([]string{"serve", "--port", strconv.Itoa(port), "--hostname", "127.0.0.1"}, extraArgs...)
		p, err := proc.Start(ctx, proc.Opts{
			Path: bin, Args: args, Env: proc.ChildEnv(childEnv),
			OnStderr: m.rt.stderrHook(OpenCode, nil),
		})
		if err != nil {
			return nil, fmt.Errorf("opencode serve: %w", err)
		}
		s := &openCodeServer{
			key: key, bin: bin, port: port, password: password,
			username: childEnv["OPENCODE_SERVER_USERNAME"], proc: p, refs: 1,
			argv: append([]string{bin}, args...), rt: m.rt,
			sessions: map[string]*openCodeSession{},
		}
		if err := s.waitHealthy(ctx); err != nil {
			_ = p.KillTree()
			lastErr = err
			if attempt < 2 {
				continue
			}
			return nil, err
		}
		if m.rt.led != nil {
			m.rt.led.Track("opencode:"+key, string(OpenCode), p.PID(), p.Pgid(), s.argv)
		}
		s.ensureStream()
		return s, nil
	}
	return nil, lastErr
}

// freePort asks the OS for an unused loopback port.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// waitHealthy polls /global/health until the server answers or the child dies.
func (s *openCodeServer) waitHealthy(ctx context.Context) error {
	deadline := time.Now().Add(openCodeReadyTimeout)
	client := &http.Client{Timeout: 2 * time.Second}
	for {
		if s.proc.ExitCode() != -1 {
			return fmt.Errorf("opencode serve exited early: %s", strings.TrimSpace(s.proc.Stderr()))
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL()+"/global/health", nil)
		req.SetBasicAuth(s.username, s.password)
		if resp, err := client.Do(req); err == nil {
			var body struct {
				Healthy bool   `json:"healthy"`
				Version string `json:"version"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&body)
			resp.Body.Close()
			if body.Healthy {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return errors.New("opencode serve: health timeout")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}
