package agentwire

import (
	"bufio"
	"os"
	"testing"
)

func TestFragmentClassifiers(t *testing.T) {
	cases := []struct {
		name string
		f    interface{ Fragment([]byte) bool }
		line string
		want bool
	}{
		{"claude thinking delta", &claudeProtocol{}, `{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"I"}}}`, true},
		{"claude tool input delta", &claudeProtocol{}, `{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"{"}}}`, true},
		{"claude text delta", &claudeProtocol{}, `{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}}`, true},
		{"claude thinking tokens", &claudeProtocol{}, `{"type":"system","subtype":"thinking_tokens","estimated_tokens":3}`, true},
		{"claude block start", &claudeProtocol{}, `{"type":"stream_event","event":{"type":"content_block_start","content_block":{"type":"tool_use"}}}`, false},
		{"claude message delta", &claudeProtocol{}, `{"type":"stream_event","event":{"type":"message_delta","usage":{}}}`, false},
		{"claude assistant", &claudeProtocol{}, `{"type":"assistant","message":{"content":[{"type":"text","text":"stream_event"}]}}`, false},
		{"claude init", &claudeProtocol{}, `{"type":"system","subtype":"init"}`, false},
		{"codex message delta", &codexProtocol{}, `{"method":"item/agentMessage/delta","params":{"delta":"h"}}`, true},
		{"codex reasoning delta", &codexProtocol{}, `{"method":"item/reasoning/summaryTextDelta","params":{"delta":"h"}}`, true},
		{"codex legacy delta", &codexProtocol{}, `{"method":"codex/event/agent_message_delta","params":{}}`, true},
		{"codex completed", &codexProtocol{}, `{"method":"item/completed","params":{"item":{"text":"Delta"}}}`, false},
		{"codex response", &codexProtocol{}, `{"id":3,"method":"item/agentMessage/delta","result":{}}`, false},
		{"pi update", &piProtocol{}, `{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"h"}}`, true},
		{"pi end", &piProtocol{}, `{"type":"message_end","message":{"role":"assistant"}}`, false},
		{"not json", &claudeProtocol{}, `stream_event thinking_tokens`, false},
	}
	for _, c := range cases {
		if got := c.f.Fragment([]byte(c.line)); got != c.want {
			t.Errorf("%s: Fragment = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestClaudeFixtureFragments checks that a recorded stream marks its deltas
// and keeps every complete frame.
func TestClaudeFixtureFragments(t *testing.T) {
	f, err := os.Open("testdata/claude/basic.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	p := &claudeProtocol{}
	var frag, kept int
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		if p.Fragment(sc.Bytes()) {
			frag++
		} else {
			kept++
		}
	}
	if kept == 0 {
		t.Fatal("no complete frame kept")
	}
	t.Logf("fragments %d, kept %d", frag, kept)
}
