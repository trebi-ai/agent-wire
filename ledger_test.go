package agentwire

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/trebi-ai/agent-wire/internal/proc"
)

// rtTestFakeScriptAlive is a fake-driver program that stays alive until its
// stdin closes, so the session keeps a live pid for the length of a test.
const rtTestFakeScriptAlive = "read -r line || true"

// rtTestSkipWithoutShell skips a test whose fake driver needs a POSIX shell.
func rtTestSkipWithoutShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake driver runs sh")
	}
}

// rtTestFakeSession starts a fake-driver session that stays alive.
func rtTestFakeSession(t *testing.T, script string) Session {
	t.Helper()
	rtTestSkipWithoutShell(t)
	s, err := FakeDriver{Script: script}.Start(context.Background(), StartRequest{
		Harness:    Fake,
		WorkingDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("start fake session: %v", err)
	}
	t.Cleanup(func() { rtTestCloseSession(t, s) })
	return s
}

// rtTestCloseSession closes a session and fails the test only on an error.
func rtTestCloseSession(t *testing.T, s Session) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.Close(ctx); err != nil {
		t.Errorf("close session: %v", err)
	}
}

// rtTestLedgerPath is a fresh ledger file inside the test's temp dir.
func rtTestLedgerPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "procs.json")
}

// rtTestWriteLedger writes rows straight to the ledger file, the way a crashed
// process leaves them.
func rtTestWriteLedger(t *testing.T, path string, rows []LedgerEntry) {
	t.Helper()
	data, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		t.Fatalf("marshal ledger: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write ledger: %v", err)
	}
}

// TestLedgerAddRecordsProcessEvidence covers Registry.Add: one row per live
// session, carrying the process evidence that reconcile needs.
func TestLedgerAddRecordsProcessEvidence(t *testing.T) {
	path := rtTestLedgerPath(t)
	reg := NewRegistry(path)
	sess := rtTestFakeSession(t, rtTestFakeScriptAlive)
	reg.Add("fake:one", sess)

	rows := reg.List()
	if len(rows) != 1 {
		t.Fatalf("ledger rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if row.Key != "fake:one" || row.Harness != "fake" {
		t.Fatalf("row identity = %q/%q, want fake:one/fake", row.Key, row.Harness)
	}
	if row.PID != sess.PID() || row.PID <= 0 {
		t.Fatalf("row pid = %d, want the live child pid %d", row.PID, sess.PID())
	}
	wantPGID := sess.(interface{ Pgid() int }).Pgid()
	if wantPGID <= 0 {
		t.Fatalf("session pgid = %d, want a live process group", wantPGID)
	}
	if row.PGID != wantPGID {
		t.Fatalf("row pgid = %d, want %d", row.PGID, wantPGID)
	}
	if len(row.Argv) != 3 || !strings.Contains(row.Argv[0], "sh") || row.Argv[1] != "-c" || row.Argv[2] != rtTestFakeScriptAlive {
		t.Fatalf("row argv = %q, want the child command line", row.Argv)
	}
	if row.StartedAt.IsZero() {
		t.Fatal("row started_at is zero")
	}
	if want := proc.StartToken(row.PID); row.StartToken != want {
		t.Fatalf("row start token = %q, want %q", row.StartToken, want)
	}
	if _, ok := reg.Get("fake:one"); !ok {
		t.Fatal("Add did not register the live session")
	}
}

// TestLedgerRemoveDropsRowAndFile covers Registry.Remove: the file goes away
// with the last row, and earlier rows survive.
func TestLedgerRemoveDropsRowAndFile(t *testing.T) {
	path := rtTestLedgerPath(t)
	reg := NewRegistry(path)
	sess := rtTestFakeSession(t, rtTestFakeScriptAlive)
	reg.Add("fake:one", sess)
	reg.Track("helper:one", "helper", os.Getpid(), os.Getpid(), []string{"helper"})
	if rows := reg.List(); len(rows) != 2 {
		t.Fatalf("ledger rows before the removal = %d, want 2", len(rows))
	}
	reg.Remove("fake:one")
	rows := reg.List()
	if len(rows) != 1 || rows[0].Key != "helper:one" {
		t.Fatalf("ledger after removing one row = %+v, want only helper:one", rows)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("ledger file gone with a row left: %v", err)
	}
	if _, ok := reg.Get("fake:one"); ok {
		t.Fatal("Remove left the session registered")
	}
	reg.Remove("helper:one")
	if rows := reg.List(); len(rows) != 0 {
		t.Fatalf("ledger after the last removal = %+v, want no rows", rows)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("ledger file survived the last removal: err = %v", err)
	}
}

// TestLedgerReconcileSparesReusedPID covers the pid-reuse guard: a row whose
// start token differs from the live process's token describes a pid that now
// belongs to somebody else, so Reconcile drops the row without a kill.
func TestLedgerReconcileSparesReusedPID(t *testing.T) {
	rtTestSkipWithoutShell(t)
	cmd := exec.Command("sh", "-c", "sleep 30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the unrelated helper: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	pid := cmd.Process.Pid
	if proc.StartToken(pid) == "" {
		t.Skip("this platform reports no start token")
	}
	path := rtTestLedgerPath(t)
	rtTestWriteLedger(t, path, []LedgerEntry{{
		Key: "claude:4242", Harness: "claude", PID: pid, PGID: pid,
		StartToken: "not-the-token-of-this-process", StartedAt: time.Now(),
	}})

	killed, stale := NewRegistry(path).Reconcile()
	if len(killed) != 0 {
		t.Fatalf("Reconcile killed a reused pid: %q", killed)
	}
	if len(stale) != 1 || stale[0] != "claude:4242" {
		t.Fatalf("stale keys = %q, want [claude:4242]", stale)
	}
	if !proc.Alive(pid) {
		t.Fatal("the unrelated process died: Reconcile killed a reused pid")
	}
}

// TestLedgerReconcileDropsDeadRow covers the ordinary boot case: a row whose
// process is gone disappears without a kill attempt.
func TestLedgerReconcileDropsDeadRow(t *testing.T) {
	rtTestSkipWithoutShell(t)
	gone := exec.Command("sh", "-c", "exit 0")
	if err := gone.Run(); err != nil {
		t.Fatalf("run the short-lived helper: %v", err)
	}
	pid := gone.Process.Pid
	if proc.Alive(pid) {
		t.Skip("the helper pid is still alive")
	}
	path := rtTestLedgerPath(t)
	rtTestWriteLedger(t, path, []LedgerEntry{{
		Key: "codex:7", Harness: "codex", PID: pid, PGID: pid,
		StartToken: proc.StartToken(pid), StartedAt: time.Now(),
	}})

	killed, stale := NewRegistry(path).Reconcile()
	if len(killed) != 0 {
		t.Fatalf("Reconcile reported kills for a dead pid: %q", killed)
	}
	if len(stale) != 1 || stale[0] != "codex:7" {
		t.Fatalf("stale keys = %q, want [codex:7]", stale)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("ledger file survived reconcile: err = %v", err)
	}
}

// TestLedgerRuntimeReconcileWithoutLedger covers Runtime.Reconcile: a runtime
// with no ledger path and one with an empty ledger both return no error.
func TestLedgerRuntimeReconcileWithoutLedger(t *testing.T) {
	plain := New(Options{})
	t.Cleanup(func() { _ = plain.Close(context.Background()) })
	if err := plain.Reconcile(); err != nil {
		t.Fatalf("Reconcile without a ledger path: %v", err)
	}
	ledgered := New(Options{LedgerPath: rtTestLedgerPath(t)})
	t.Cleanup(func() { _ = ledgered.Close(context.Background()) })
	if err := ledgered.Reconcile(); err != nil {
		t.Fatalf("Reconcile with an empty ledger: %v", err)
	}
}

// TestLedgerRuntimeTrackAndSessionClose covers Runtime.track and untrack: a
// session lands in the ledger under harness:pid, and closing it drops the row
// through the OnClose hook that track registers.
//
// The standalone FakeDriver does not call the runtime's private bindSession,
// so a caller wires a fake session into the ledger with track. A built-in
// driver does it itself.
func TestLedgerRuntimeTrackAndSessionClose(t *testing.T) {
	rtTestSkipWithoutShell(t)
	path := rtTestLedgerPath(t)
	rt := New(Options{LedgerPath: path})
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	rt.SetDriver(Claude, FakeDriver{Script: rtTestFakeScriptAlive})

	sess, err := rt.Start(context.Background(), StartRequest{Harness: Claude, WorkingDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Start through the fake override: %v", err)
	}
	t.Cleanup(func() { rtTestCloseSession(t, sess) })
	rt.track(sess)

	wantKey := sess.Provider() + ":" + strconv.Itoa(sess.PID())
	rows := rt.led.List()
	if len(rows) != 1 || rows[0].Key != wantKey || rows[0].PID != sess.PID() {
		t.Fatalf("ledger after track = %+v, want one row keyed %q", rows, wantKey)
	}
	if keys := rt.led.Keys(); len(keys) != 1 || keys[0] != wantKey {
		t.Fatalf("registry keys after track = %q, want [%s]", keys, wantKey)
	}
	rtTestCloseSession(t, sess)
	if rows := rt.led.List(); len(rows) != 0 {
		t.Fatalf("ledger after the session closed = %+v, want no rows", rows)
	}
	if keys := rt.led.Keys(); len(keys) != 0 {
		t.Fatalf("registry keys after the session closed = %q, want none", keys)
	}
	// untrack on an already-removed row stays quiet.
	rt.untrack(sess)
	if rows := rt.led.List(); len(rows) != 0 {
		t.Fatalf("ledger after untrack = %+v, want no rows", rows)
	}
}

// TestLedgerRuntimeCloseClosesSessions covers the shutdown path: Runtime.Close
// stops every registered session and empties the registry.
func TestLedgerRuntimeCloseClosesSessions(t *testing.T) {
	rtTestSkipWithoutShell(t)
	rt := New(Options{LedgerPath: rtTestLedgerPath(t)})
	rt.SetDriver(Claude, FakeDriver{Script: rtTestFakeScriptAlive})
	sess, err := rt.Start(context.Background(), StartRequest{Harness: Claude, WorkingDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Start through the fake override: %v", err)
	}
	t.Cleanup(func() { rtTestCloseSession(t, sess) })
	rt.track(sess)
	if keys := rt.led.Keys(); len(keys) != 1 {
		t.Fatalf("registry keys before Close = %q, want one", keys)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := rt.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if keys := rt.led.Keys(); len(keys) != 0 {
		t.Fatalf("registry keys after Close = %q, want none", keys)
	}
	if rows := rt.led.List(); len(rows) != 0 {
		t.Fatalf("ledger rows after Close = %+v, want none", rows)
	}
	select {
	case <-sess.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Close left the registered session running")
	}
}
