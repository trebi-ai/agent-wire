package native

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Store persists the provider-neutral message log of one session. The loop
// appends after every model and tool step, so a resume replays exactly.
type Store interface {
	// Load returns the stored messages. ErrNotFound means the store has no
	// log for the session and the caller starts fresh.
	Load(ctx context.Context, sessionID string) ([]Message, error)
	Append(ctx context.Context, sessionID string, msgs ...Message) error
	Replace(ctx context.Context, sessionID string, msgs []Message) error
}

// FileStore is the NDJSON message log under dir/<session>.messages.jsonl.
// It reads the trebi legacy llm.Message shape as well, so stored native
// sessions survive the migration (plan 2026-09-26 I.7 step 6).
func FileStore(dir string) Store { return fileStore{dir: dir} }

type fileStore struct{ dir string }

func (s fileStore) path(sessionID string) string {
	return filepath.Join(s.dir, sanitizeSession(sessionID)+".messages.jsonl")
}

func sanitizeSession(id string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		}
		return '_'
	}, id)
}

func (s fileStore) Load(ctx context.Context, sessionID string) ([]Message, error) {
	f, err := os.Open(s.path(sessionID))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	defer f.Close()
	var out []Message
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		m, err := unmarshalMessage([]byte(line))
		if err != nil {
			// One broken line never sinks the session.
			continue
		}
		out = append(out, m)
	}
	return out, sc.Err()
}

func (s fileStore) Append(ctx context.Context, sessionID string, msgs ...Message) error {
	if len(msgs) == 0 {
		return nil
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(s.path(sessionID), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	for _, m := range msgs {
		body, err := json.Marshal(m)
		if err != nil {
			return err
		}
		if _, err := w.Write(append(body, '\n')); err != nil {
			return err
		}
	}
	return w.Flush()
}

func (s fileStore) Replace(ctx context.Context, sessionID string, msgs []Message) error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	tmp := s.path(sessionID) + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	for _, m := range msgs {
		body, err := json.Marshal(m)
		if err != nil {
			f.Close()
			os.Remove(tmp)
			return err
		}
		if _, err := w.Write(append(body, '\n')); err != nil {
			f.Close()
			os.Remove(tmp)
			return err
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, s.path(sessionID))
}

// unmarshalMessage decodes one log line. It accepts the current native
// shape (tagged "parts") and the legacy trebi llm.Message shape.
func unmarshalMessage(line []byte) (Message, error) {
	if bytes.Contains(line, []byte(`"parts"`)) {
		var m Message
		if err := json.Unmarshal(line, &m); err == nil && m.Role != "" {
			return m, nil
		}
	}
	return legacyMessage(line)
}

// legacyLine is the pre-migration log shape (untagged trebi llm.Message
// fields).
type legacyLine struct {
	Role       string        `json:"Role"`
	Content    string        `json:"Content"`
	ToolCallID string        `json:"ToolCallID"`
	Name       string        `json:"Name"`
	ToolCalls  []struct {
		ID        string          `json:"ID"`
		Name      string          `json:"Name"`
		Arguments json.RawMessage `json:"Arguments"`
	} `json:"ToolCalls"`
}

// legacyMessage converts one legacy log line.
func legacyMessage(line []byte) (Message, error) {
	var l legacyLine
	if err := json.Unmarshal(line, &l); err != nil {
		return Message{}, fmt.Errorf("native: unreadable log line: %w", err)
	}
	if l.Role == "" {
		return Message{}, errors.New("native: log line has no role")
	}
	switch l.Role {
	case "user", "assistant":
		m := Message{Role: Role(l.Role)}
		if l.Content != "" {
			m.Parts = append(m.Parts, Part{Text: &TextPart{Text: l.Content}})
		}
		for _, tc := range l.ToolCalls {
			m.Parts = append(m.Parts, Part{ToolCall: &ToolCall{ID: tc.ID, Name: tc.Name, Input: tc.Arguments}})
		}
		return m, nil
	case "tool":
		return Message{Role: RoleTool, Parts: []Part{{ToolResult: &ToolResult{
			CallID:  l.ToolCallID,
			Name:    l.Name,
			Content: []Part{{Text: &TextPart{Text: l.Content}}},
		}}}}, nil
	}
	return Message{}, fmt.Errorf("native: unknown legacy role %q", l.Role)
}

// MemoryStore is an in-process Store, for tests and ephemeral sessions.
type MemoryStore struct {
	mu   sync.Mutex
	logs map[string][]Message
}

func NewMemoryStore() *MemoryStore { return &MemoryStore{logs: map[string][]Message{}} }

func (m *MemoryStore) Load(_ context.Context, sessionID string) ([]Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	msgs, ok := m.logs[sessionID]
	if !ok {
		return nil, ErrNotFound
	}
	return append([]Message(nil), msgs...), nil
}

func (m *MemoryStore) Append(_ context.Context, sessionID string, msgs ...Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.logs[sessionID] = append(m.logs[sessionID], msgs...)
	return nil
}

func (m *MemoryStore) Replace(_ context.Context, sessionID string, msgs []Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.logs[sessionID] = append([]Message(nil), msgs...)
	return nil
}
