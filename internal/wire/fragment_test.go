package wire

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fragAdapter marks {"type":"delta"} frames as fragments.
type fragAdapter struct{ testAdapter }

func (a *fragAdapter) Fragment(line []byte) bool {
	return bytes.Contains(line, []byte(`"type":"delta"`))
}

type logLine struct {
	Dir   string          `json:"dir"`
	Frame json.RawMessage `json:"frame"`
	Error *struct {
		Code      string `json:"code"`
		Fragments int    `json:"fragments"`
	} `json:"error"`
}

func readLog(t *testing.T, path string) []logLine {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []logLine
	for _, l := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		if len(l) == 0 {
			continue
		}
		var e logLine
		if err := json.Unmarshal(l, &e); err != nil {
			t.Fatalf("bad line %q: %v", l, err)
		}
		out = append(out, e)
	}
	return out
}

func runFrames(t *testing.T, cfg Config, stream string) {
	t.Helper()
	s := NewMemorySession("test", &fragAdapter{}, io.Discard, strings.NewReader(stream), context.Background(), cfg)
	collect(t, s)
	waitDone(t, s)
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func paths(t *testing.T) (string, string) {
	dir := t.TempDir()
	return filepath.Join(dir, "frames.ndjson"), filepath.Join(dir, "fragments.ndjson")
}

func count(lines []logLine, dir string) int {
	n := 0
	for _, l := range lines {
		if l.Dir == dir {
			n++
		}
	}
	return n
}

const deltas = `{"type":"delta","text":"h"}` + "\n" + `{"type":"delta","text":"i"}` + "\n"

func TestFrameLogLeavesOutFragments(t *testing.T) {
	log, frag := paths(t)
	runFrames(t, Config{LogPath: log, FragmentLogPath: frag}, deltas+`{"type":"assistant","text":"hi"}`+"\n")
	lines := readLog(t, log)
	if len(lines) != 1 || count(lines, "in") != 1 || strings.Contains(string(lines[0].Frame), "delta") {
		t.Fatalf("log = %+v, want only the complete frame", lines)
	}
	if _, err := os.Stat(frag); !os.IsNotExist(err) {
		t.Fatalf("fragment file exists after a clean end: %v", err)
	}
}

func TestFrameLogKeepsFragmentsOfIncompleteStream(t *testing.T) {
	log, frag := paths(t)
	runFrames(t, Config{LogPath: log, FragmentLogPath: frag}, `{"type":"assistant","text":"a"}`+"\n"+deltas)
	lines := readLog(t, log)
	if count(lines, "error") != 1 || lines[len(lines)-1].Error.Code != "incomplete_stream" || lines[len(lines)-1].Error.Fragments != 2 {
		t.Fatalf("log = %+v, want one incomplete_stream error with 2 fragments", lines)
	}
	kept := readLog(t, frag)
	if count(kept, "error") != 1 || count(kept, "in") != 2 {
		t.Fatalf("fragments = %+v, want the error and 2 fragments", kept)
	}
	if fi, err := os.Stat(frag); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("fragment file mode: %v %v", fi, err)
	}
}

func TestFrameLogKeepsFragmentsOnError(t *testing.T) {
	log, frag := paths(t)
	runFrames(t, Config{LogPath: log, FragmentLogPath: frag}, deltas+`{"type":"error","error":"boom","code":"overloaded"}`+"\n")
	lines := readLog(t, log)
	if count(lines, "error") != 1 || count(lines, "in") != 1 {
		t.Fatalf("log = %+v, want the error frame and one error line", lines)
	}
	for _, l := range lines {
		if l.Error != nil && (l.Error.Code != "overloaded" || l.Error.Fragments != 2) {
			t.Fatalf("error line = %+v", l.Error)
		}
	}
	if kept := readLog(t, frag); count(kept, "in") != 2 {
		t.Fatalf("fragments = %+v, want 2", kept)
	}
}

func TestFrameLogErrorWithNoFragments(t *testing.T) {
	log, frag := paths(t)
	runFrames(t, Config{LogPath: log, FragmentLogPath: frag}, `{"type":"error","error":"boom"}`+"\n")
	if lines := readLog(t, log); count(lines, "error") != 1 {
		t.Fatalf("log = %+v, want one error line", lines)
	}
	if _, err := os.Stat(frag); !os.IsNotExist(err) {
		t.Fatal("fragment file written with no fragments")
	}
}

func TestFrameLogWritesFragmentsInDebug(t *testing.T) {
	log, frag := paths(t)
	runFrames(t, Config{LogPath: log, FragmentLogPath: frag, LogFragments: true}, deltas)
	lines := readLog(t, log)
	if count(lines, "in") != 2 || count(lines, "error") != 0 {
		t.Fatalf("log = %+v, want both fragments and no error", lines)
	}
	if _, err := os.Stat(frag); !os.IsNotExist(err) {
		t.Fatal("fragment file written in debug mode")
	}
}

func TestFrameLogCapDropsOldestFragments(t *testing.T) {
	log, frag := paths(t)
	l, err := openFrameLog(Config{LogPath: log, FragmentLogPath: frag})
	if err != nil {
		t.Fatal(err)
	}
	l.max = 1
	for _, txt := range []string{"a", "b", "c"} {
		l.write("in", []byte(`{"type":"delta","text":"`+txt+`"}`), true)
	}
	l.fail("test", "cap")
	l.close()
	kept := readLog(t, frag)
	if count(kept, "in") != 1 || !strings.Contains(string(kept[1].Frame), `"c"`) {
		t.Fatalf("fragments = %+v, want only the newest", kept)
	}
}
