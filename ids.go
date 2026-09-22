package agentwire

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// requestID mirrors the vendor SDK shape: req_<n>_<hex>. The counter belongs
// to the Runtime, and the random suffix survives a process restart, so a
// resumed session never reuses an id.
func (rt *Runtime) requestID() string {
	return fmt.Sprintf("req_%d_%s", rt.reqSeq.Add(1), randomHex(4))
}

// randomHex returns n random bytes as lowercase hex.
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "00000000"
	}
	return hex.EncodeToString(b)
}

// randomToken is a short opaque secret, used for the OpenCode server password.
func randomToken() string { return randomHex(16) }

// orDefault returns s unless it is empty or the string form of nil.
func orDefault(s, def string) string {
	if s == "" || s == "<nil>" {
		return def
	}
	return s
}
