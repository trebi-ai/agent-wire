package agentwire

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/trebi-ai/agent-wire/internal/proc"
)

// LedgerEntry is one spawned child, recorded so a crashed process can reconcile
// leftovers on boot. StartToken is the positive-match guard against pid reuse.
type LedgerEntry struct {
	Key        string    `json:"key"`
	Harness    string    `json:"harness"`
	PID        int       `json:"pid"`
	PGID       int       `json:"pgid,omitempty"`
	StartToken string    `json:"start_token,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	Argv       []string  `json:"argv,omitempty"`
}

// Registry owns the live sessions and the pid ledger. A Runtime builds one
// when Options.LedgerPath is set.
type Registry struct {
	mu       sync.Mutex
	path     string
	sessions map[string]Session
}

// NewRegistry builds a registry whose ledger lives at path. An empty path
// disables persistence, which is what tests want.
func NewRegistry(path string) *Registry {
	return &Registry{path: path, sessions: map[string]Session{}}
}

// Path is the ledger file, empty when the registry is memory-only.
func (r *Registry) Path() string {
	if r == nil {
		return ""
	}
	return r.path
}

// Add registers a live session under key and records its pid.
func (r *Registry) Add(key string, s Session) {
	if r == nil || key == "" || s == nil {
		return
	}
	r.mu.Lock()
	r.sessions[key] = s
	r.mu.Unlock()
	r.Track(key, s.Provider(), s.PID(), pgidOf(s), argvOf(s))
}

// Track records a process without a live Session. Use it for a daemon-owned
// helper such as an OpenCode server.
func (r *Registry) Track(key, harness string, pid, pgid int, argv []string) {
	if r == nil || key == "" || pid <= 0 {
		return
	}
	r.writeEntry(LedgerEntry{
		Key:        key,
		Harness:    harness,
		PID:        pid,
		PGID:       pgid,
		StartedAt:  time.Now().UTC(),
		Argv:       argv,
		StartToken: proc.StartToken(pid),
	})
}

// Remove unregisters a session and drops its ledger row.
func (r *Registry) Remove(key string) {
	if r == nil || key == "" {
		return
	}
	r.mu.Lock()
	delete(r.sessions, key)
	r.mu.Unlock()
	r.deleteEntry(key)
}

// Get returns the live session for a key.
func (r *Registry) Get(key string) (Session, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[key]
	return s, ok
}

// Keys lists live session keys in no particular order.
func (r *Registry) Keys() []string {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.sessions))
	for k := range r.sessions {
		out = append(out, k)
	}
	return out
}

// CloseAll gracefully closes every live session and empties the registry.
func (r *Registry) CloseAll(ctx context.Context) {
	if r == nil {
		return
	}
	for _, key := range r.Keys() {
		if s, ok := r.Get(key); ok {
			_ = s.Close(ctx)
		}
		r.Remove(key)
	}
}

// List reads the persisted ledger.
func (r *Registry) List() []LedgerEntry {
	if r == nil || r.path == "" {
		return nil
	}
	data, err := os.ReadFile(r.path)
	if err != nil {
		return nil
	}
	var out []LedgerEntry
	if json.Unmarshal(data, &out) != nil {
		return nil
	}
	return out
}

// Reconcile is the boot path: for each ledger row, kill the process only when
// it is positively matched (alive and start token equal); every row is then
// dropped. It returns the run keys whose processes were killed and every key
// that needs a session_gone row.
func (r *Registry) Reconcile() (killed []string, stale []string) {
	if r == nil || r.path == "" {
		return nil, nil
	}
	for _, e := range r.List() {
		stale = append(stale, e.Key)
		if e.PID <= 0 || !proc.Alive(e.PID) {
			continue
		}
		if e.StartToken != "" {
			if tok := proc.StartToken(e.PID); tok != "" && tok != e.StartToken {
				// The pid was reused by an unrelated process: never kill it.
				continue
			}
		}
		proc.KillGroup(e.PGID, e.PID)
		killed = append(killed, e.Key)
	}
	_ = os.Remove(r.path)
	return killed, stale
}

func (r *Registry) writeEntry(e LedgerEntry) {
	if r == nil || r.path == "" || e.PID <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	entries := r.listLocked()
	replaced := false
	for i := range entries {
		if entries[i].Key == e.Key {
			entries[i] = e
			replaced = true
			break
		}
	}
	if !replaced {
		entries = append(entries, e)
	}
	r.writeLocked(entries)
}

func (r *Registry) deleteEntry(key string) {
	if r == nil || r.path == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	entries := r.listLocked()
	out := entries[:0]
	for _, e := range entries {
		if e.Key != key {
			out = append(out, e)
		}
	}
	if len(out) == 0 {
		_ = os.Remove(r.path)
		return
	}
	r.writeLocked(out)
}

func (r *Registry) listLocked() []LedgerEntry {
	data, err := os.ReadFile(r.path)
	if err != nil {
		return nil
	}
	var out []LedgerEntry
	if json.Unmarshal(data, &out) != nil {
		return nil
	}
	return out
}

func (r *Registry) writeLocked(entries []LedgerEntry) {
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		return
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, r.path)
}

// pgidOf reports a session's process group when it has one.
func pgidOf(s Session) int {
	if v, ok := s.(interface{ Pgid() int }); ok {
		return v.Pgid()
	}
	return 0
}

// argvOf reports a session's command line when it has one.
func argvOf(s Session) []string {
	if v, ok := s.(interface{ Argv() []string }); ok {
		return v.Argv()
	}
	return nil
}

// sessionKey is the ledger key of a session: harness plus pid.
func sessionKey(s Session) string {
	return fmt.Sprintf("%s:%d", s.Provider(), s.PID())
}

// track records a session in the runtime ledger. It is a no-op without a
// ledger path. A session that can report its own close drops its row there; a
// session that cannot leaves a row that the next Reconcile clears.
func (rt *Runtime) track(s Session) {
	if rt.led == nil || s == nil {
		return
	}
	key := sessionKey(s)
	rt.led.Add(key, s)
	if c, ok := s.(interface{ OnClose(func()) }); ok {
		c.OnClose(func() { rt.led.Remove(key) })
	}
}

// untrack drops a session's ledger row.
func (rt *Runtime) untrack(s Session) {
	if rt.led == nil || s == nil {
		return
	}
	rt.led.Remove(sessionKey(s))
}
