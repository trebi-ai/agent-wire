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

// openCodeStreamReadyTimeout bounds the wait for a new event stream to connect.
const openCodeStreamReadyTimeout = 10 * time.Second

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
	// wire is the HTTP surface this server speaks.
	wire openCodeWire

	mu       sync.Mutex
	refs     int
	idle     *time.Timer
	sessions map[string]openCodeRoute
	// streams holds the server's /event connections, keyed by the scope the
	// wire needs. V1 scopes its event bus by directory, so a stream opened
	// without one never sees a session that runs in another directory; V2
	// streams every location on one connection.
	streams map[string]*openCodeStream
	stopped bool
}

// openCodeStream is one event-stream connection of a server.
type openCodeStream struct {
	cancel context.CancelFunc
	// ready is closed once the connection is established.
	ready chan struct{}
	once  sync.Once
}

// markReady reports that the connection is live.
func (st *openCodeStream) markReady() {
	st.once.Do(func() { close(st.ready) })
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
	streams := s.streams
	s.streams = nil
	sessions := s.sessions
	s.sessions = map[string]openCodeRoute{}
	s.mu.Unlock()
	for _, st := range streams {
		st.cancel()
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
func (s *openCodeServer) subscribe(sess openCodeRoute) {
	s.mu.Lock()
	s.sessions[sess.sessionID()] = sess
	s.mu.Unlock()
}

// unsubscribe removes a session from the routing table.
func (s *openCodeServer) unsubscribe(id string) {
	s.mu.Lock()
	delete(s.sessions, id)
	s.mu.Unlock()
}

// lookup finds the session a bus event belongs to.
func (s *openCodeServer) lookup(id string) openCodeRoute {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[id]
}

// allSessions snapshots the routing table.
func (s *openCodeServer) allSessions() []openCodeRoute {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]openCodeRoute, 0, len(s.sessions))
	for _, sess := range s.sessions {
		out = append(out, sess)
	}
	return out
}

// ensureStream keeps one event stream open for the scope the wire needs: one
// per session directory on V1, one per server on V2.
// OpenCode 1.x publishes a session's events only to a stream opened for the
// same directory, so the stream is keyed by directory, not by server.
func (s *openCodeServer) ensureStream(directory string) *openCodeStream {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return nil
	}
	if s.streams == nil {
		s.streams = map[string]*openCodeStream{}
	}
	scope := directory
	if !s.wire.eventScoped() {
		scope = ""
	}
	if st, ok := s.streams[scope]; ok {
		s.mu.Unlock()
		return st
	}
	ctx, cancel := context.WithCancel(context.Background())
	st := &openCodeStream{cancel: cancel, ready: make(chan struct{})}
	s.streams[scope] = st
	s.mu.Unlock()
	go s.streamLoop(ctx, scope, st)
	return st
}

// ensureStreamReady opens the event stream for a scope and waits until it is
// connected. A session created before the stream is live loses every event the
// vendor publishes for it, including the one that carries the session id.
func (s *openCodeServer) ensureStreamReady(ctx context.Context, directory string) error {
	st := s.ensureStream(directory)
	if st == nil {
		return errors.New("opencode: server is stopped")
	}
	timer := time.NewTimer(openCodeStreamReadyTimeout)
	defer timer.Stop()
	select {
	case <-st.ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return errors.New("opencode: event stream did not connect")
	}
}

// streamLoop keeps one /event connection alive and routes frames by session.
func (s *openCodeServer) streamLoop(ctx context.Context, directory string, st *openCodeStream) {
	backoff := openCodeStreamRetryMin
	for ctx.Err() == nil {
		err := s.readStream(ctx, directory, st)
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

// readStream reads the event stream for one scope until the connection ends.
func (s *openCodeServer) readStream(ctx context.Context, directory string, st *openCodeStream) error {
	streamURL := s.baseURL() + s.wire.eventPath()
	scoped := s.wire.eventScoped() && directory != ""
	if scoped {
		streamURL += "?" + url.Values{"directory": {directory}}.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, streamURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	if scoped {
		req.Header.Set("x-opencode-directory", directory)
	}
	req.SetBasicAuth(s.username, s.password)
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("opencode %s: HTTP %d", s.wire.eventPath(), resp.StatusCode)
	}
	// The connection is live: a session may now be created without losing its
	// first events.
	st.markReady()
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
	typ, payload, id, ok := s.wire.frame(frame)
	if !ok || id == "" {
		return
	}
	if sess := s.lookup(id); sess != nil {
		sess.deliver(sess.parse(typ, payload))
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
	reqURL := s.baseURL() + s.wire.prefix() + path
	if s.wire.eventScoped() && directory != "" {
		reqURL += "?" + url.Values{"directory": {directory}}.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, reqURL, strings.NewReader(string(payload)))
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if s.wire.eventScoped() && directory != "" {
		req.Header.Set("x-opencode-directory", directory)
	}
	req.SetBasicAuth(s.username, s.password)
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, int64(s.rt.maxFr)))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("opencode %s %s: %s", method, path, s.wire.errorText(raw, resp.StatusCode))
	}
	return s.wire.decode(raw, out)
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
// it can serve: the wire, the binary, non-run-scoped variables, and extra
// arguments. Keys in Options.FingerprintIgnoreEnv and the server's own auth
// never split it.
func (m *openCodeManager) fingerprint(w openCodeWire, bin string, env map[string]string, extra []string) string {
	keys := make([]string, 0, len(env))
	for k := range env {
		if m.excluded(k) {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(string(w.harness()))
	b.WriteByte('\x00')
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
func (m *openCodeManager) acquireServer(ctx context.Context, w openCodeWire, bin string, env map[string]string, extraArgs []string) (*openCodeServer, error) {
	key := m.fingerprint(w, bin, env, extraArgs)
	m.mu.Lock()
	if s, ok := m.servers[key]; ok && s.alive() {
		m.mu.Unlock()
		s.acquire()
		return s, nil
	}
	m.mu.Unlock()

	s, err := m.startServer(ctx, key, w, bin, env, extraArgs)
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

func (m *openCodeManager) startServer(ctx context.Context, key string, w openCodeWire, bin string, env map[string]string, extraArgs []string) (*openCodeServer, error) {
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
			OnStderr: m.rt.stderrHook(w.harness(), nil),
		})
		if err != nil {
			return nil, fmt.Errorf("%s serve: %w", w.harness(), err)
		}
		s := &openCodeServer{
			key: key, bin: bin, port: port, password: password, wire: w,
			username: childEnv["OPENCODE_SERVER_USERNAME"], proc: p, refs: 1,
			argv: append([]string{bin}, args...), rt: m.rt,
			sessions: map[string]openCodeRoute{},
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
			m.rt.led.Track(string(w.harness())+":"+key, string(w.harness()), p.PID(), p.Pgid(), s.argv)
		}
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

// waitHealthy polls the wire's health endpoint until the server answers or the
// child dies.
func (s *openCodeServer) waitHealthy(ctx context.Context) error {
	deadline := time.Now().Add(openCodeReadyTimeout)
	client := &http.Client{Timeout: 2 * time.Second}
	for {
		if s.proc.ExitCode() != -1 {
			return fmt.Errorf("opencode serve exited early: %s", strings.TrimSpace(s.proc.Stderr()))
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL()+s.wire.healthPath(), nil)
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
