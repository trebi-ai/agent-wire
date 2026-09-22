package proc

import (
	"os"
	"strings"
)

// claudeCodeMarker is dropped from every child environment: a parent Claude
// Code process sets it, and a harness child that inherits it mistrusts the
// wire.
const claudeCodeMarker = "CLAUDECODE"

// ChildEnv merges the process environment with overrides. Override keys win;
// an empty or reserved key is ignored. The result is in "K=V" form.
func ChildEnv(overrides map[string]string) []string {
	merged := make([]string, 0, len(overrides)+32)
	index := make(map[string]int, len(overrides)+32)
	add := func(k, v string) {
		if k == "" || k == claudeCodeMarker {
			return
		}
		key := envKey(k)
		if i, dup := index[key]; dup {
			merged[i] = k + "=" + v
			return
		}
		index[key] = len(merged)
		merged = append(merged, k+"="+v)
	}
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		add(k, v)
	}
	for k, v := range overrides {
		add(k, v)
	}
	return merged
}
