package agentwire

import (
	"strings"
	"testing"
)

// TestClassifyVendorErrorTexts pins real vendor error strings to their family
// and machine code. A consumer maps the code to its own wording.
func TestClassifyVendorErrorTexts(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		text  string
		class FailureClass
		code  string
	}{
		{"claude bad key", "Invalid API key", FailAuth, "invalid_credentials"},
		{"provider rate limit", "rate_limit_error: too many requests", FailLimit, "rate_limited"},
		{"credits gone", "You are out of credits", FailLimit, "credits_exhausted"},
		{"plan refusal", "entitlement check failed", FailEntitlement, "plan_refused"},
		{"login prompt", "Please run /login", FailAuth, "not_logged_in"},
		{"revoked token", "token has been revoked", FailAuth, "token_revoked"},
		{"overload code", "overloaded_error", FailOverloaded, "capacity"},
		{"old cli", "unrecognized subcommand 'app-server'", FailProtocol, "cli_too_old"},
		{"wire drift", "parse error: invalid frame", FailProtocol, "wire_drift"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ClassifyError(tc.text, 0)
			if got.Class != tc.class || got.Code != tc.code {
				t.Fatalf("ClassifyError(%q) = %s/%s, want %s/%s",
					tc.text, got.Class, got.Code, tc.class, tc.code)
			}
			if got.Message != strings.TrimSpace(tc.text) {
				t.Fatalf("ClassifyError(%q).Message = %q, want the trimmed text", tc.text, got.Message)
			}
		})
	}
}

// TestClassifyNormalTextStaysUnknown pins the other side of the classifier: a
// sentence that quotes digits is neither an auth nor a limit error. A status
// number matches only when it is error-shaped.
func TestClassifyNormalTextStaysUnknown(t *testing.T) {
	t.Parallel()
	for _, text := range []string{
		"I read 429 files and found 401 issues",
		"the cache holds 1503 entries",
	} {
		got := ClassifyError(text, 0)
		if got.Class == FailAuth || got.Class == FailLimit {
			t.Fatalf("ClassifyError(%q) = %s: prose must not classify", text, got.Class)
		}
		if got.Class != FailUnknown {
			t.Fatalf("ClassifyError(%q) = %s, want unknown", text, got.Class)
		}
	}
}

// TestClassifyStatusNumberNeedsErrorContext documents the numeric rule. A bare
// number classifies when it stands alone or next to an error cue word, so a
// stderr tail such as "HTTP 429" still works while ordinary prose does not.
func TestClassifyStatusNumberNeedsErrorContext(t *testing.T) {
	t.Parallel()
	cases := []struct {
		text  string
		class FailureClass
	}{
		{"429", FailLimit},
		{"(429)", FailLimit},
		{"HTTP 429", FailLimit},
		{"api error (503)", FailOverloaded},
		{"status code: 401", FailAuth},
	}
	for _, tc := range cases {
		if got := ClassifyError(tc.text, 0); got.Class != tc.class {
			t.Errorf("ClassifyError(%q) = %s, want %s", tc.text, got.Class, tc.class)
		}
	}
}

// TestClassifyCapacityProseIsKnownFalsePositive documents the real limit of the
// classifier. Vendors use ordinary English words, so "capacity" also occurs in
// prose. The caller MUST pass error-shaped text only: a result error field, a
// stderr tail, or an HTTP status field. Assistant prose and tool output are
// out of contract.
func TestClassifyCapacityProseIsKnownFalsePositive(t *testing.T) {
	t.Parallel()
	got := ClassifyError("capacity planning", 0)
	if got.Class != FailOverloaded || got.Code != "capacity" {
		t.Fatalf("ClassifyError(%q) = %s/%s, want overloaded/capacity", "capacity planning", got.Class, got.Code)
	}
}

// TestClassifyStatusPath covers the empty-text case: the wire reported an HTTP
// status and no text, and the status alone classifies.
func TestClassifyStatusPath(t *testing.T) {
	t.Parallel()
	got := ClassifyError("", 401)
	if got.Class != FailAuth {
		t.Fatalf("ClassifyError(\"\", 401) = %s, want auth", got.Class)
	}
	if got.Code != "http_401" {
		t.Fatalf("ClassifyError(\"\", 401).Code = %q, want http_401", got.Code)
	}
	if got := ClassifyError("", 503); got.Class != FailOverloaded {
		t.Fatalf("ClassifyError(\"\", 503) = %s, want overloaded", got.Class)
	}
	if got := ClassifyError("", 0); got.Class != FailUnknown {
		t.Fatalf("ClassifyError(\"\", 0) = %s, want unknown", got.Class)
	}
}

// TestClassifyForPrefixesHarness covers the per-harness code prefix.
func TestClassifyForPrefixesHarness(t *testing.T) {
	t.Parallel()
	got := ClassifyFor(Codex, "rate_limit_error: too many requests", 0)
	if got.Class != FailLimit || got.Code != "codex.rate_limited" {
		t.Fatalf("ClassifyFor(codex, ...) = %s/%s, want limit/codex.rate_limited", got.Class, got.Code)
	}
	if got := ClassifyFor(Claude, "rate_limit_error", 0); got.Code != "claude.rate_limited" {
		t.Fatalf("ClassifyFor(claude, ...).Code = %q, want claude.rate_limited", got.Code)
	}
}

// TestClassifyResultSubtypeAndLimit covers the result frame path: a harness
// failure subtype is not an entitlement class, and an error result with limit
// text keeps the limit family.
func TestClassifyResultSubtypeAndLimit(t *testing.T) {
	t.Parallel()
	got := ClassifyResult(&Result{Subtype: "error_max_turns", IsError: true})
	if got.Class != FailNone {
		t.Fatalf("error_max_turns class = %s, want none", got.Class)
	}
	if !strings.Contains(got.Code, "error_max_turns") {
		t.Fatalf("error_max_turns code = %q, want the subtype name in it", got.Code)
	}
	got = ClassifyResult(&Result{Subtype: "error_during_execution", IsError: true, Text: "You are out of credits"})
	if got.Class != FailLimit {
		t.Fatalf("credit result class = %s, want limit", got.Class)
	}
	if got.Code != "credits_exhausted" {
		t.Fatalf("credit result code = %q, want credits_exhausted", got.Code)
	}
}
