package agentwire

import (
	"fmt"
	"strings"
)

// FailureClass is the entitlement/quota classification family. No pricing
// logic lives here, and no operator-facing text: a consumer maps the class and
// the code to its own wording.
type FailureClass string

const (
	FailNone        FailureClass = ""
	FailAuth        FailureClass = "auth"
	FailLimit       FailureClass = "limit"
	FailEntitlement FailureClass = "entitlement"
	FailOverloaded  FailureClass = "overloaded"
	FailProtocol    FailureClass = "protocol"
	FailUnknown     FailureClass = "unknown"
)

// Classification is the family plus a stable machine code.
type Classification struct {
	Class   FailureClass
	Code    string
	Message string
}

// EndReason maps a class to the run end-reason vocabulary
// (auth|limit|failed|launch).
func (c Classification) EndReason() string {
	switch c.Class {
	case FailAuth, FailEntitlement:
		return "auth"
	case FailLimit:
		return "limit"
	case FailProtocol:
		return "launch"
	}
	return "failed"
}

// classRule is one ordered classifier rule.
type classRule struct {
	class   FailureClass
	code    string
	phrases []string
}

// classRules are ordered most specific first. Every pattern is matched on word
// boundaries, so "429" never matches "14290" and "credentials" never matches
// "credentials_cache_hit_note". A pattern that is only an HTTP status number
// must also stand in error context (see containsWord), so ordinary prose such
// as "I read 429 files" is not a limit error.
var classRules = []classRule{
	{FailLimit, "credits_exhausted", []string{
		"out of credits", "credit balance", "credit exhausted", "no credits",
		"insufficient credits", "agent credit",
	}},
	{FailLimit, "rate_limited", []string{
		"rate limit", "rate_limit", "429", "too many requests",
		"usage limit", "session limit", "weekly limit", "5-hour limit",
		"quota exceeded", "quota reached", "limit reached", "limit exceeded",
		"over usage", "usage cap",
	}},
	{FailEntitlement, "plan_refused", []string{
		"entitlement", "not entitled", "upgrade required", "subscription required",
		"no active subscription", "plan does not include", "access denied for your plan",
	}},
	{FailAuth, "invalid_credentials", []string{
		"invalid api key", "invalid_api_key", "authentication_error", "authentication failed",
		"unauthorized", "401",
	}},
	{FailAuth, "token_revoked", []string{"token has been revoked", "token revoked"}},
	{FailAuth, "not_logged_in", []string{
		"not logged in", "please run /login", "please login", "login required",
		"credentials", "oauth token",
	}},
	{FailAuth, "token_refresh_failed", []string{"oauth refresh failed", "invalid_grant"}},
	{FailOverloaded, "capacity", []string{"overloaded", "529", "503", "server error", "capacity"}},
	{FailProtocol, "cli_too_old", []string{
		"unrecognized subcommand", "unknown subcommand", "invalid subcommand",
	}},
	{FailProtocol, "wire_drift", []string{
		"protocol", "json-rpc", "parse error", "unexpected message", "invalid frame",
	}},
}

// ClassifyError inspects error-shaped text and returns the family and code.
//
// text MUST be error-shaped: a result error field, a stderr tail, an HTTP
// status field. Free-form assistant prose or tool output yields false
// positives, because vendors quote ordinary words such as "capacity" and
// "credentials" in normal text. status is an optional HTTP status from the
// wire (0 = unknown).
func ClassifyError(text string, status int) Classification {
	return classify("", text, status)
}

// ClassifyFor is ClassifyError with the harness recorded in the code, so a
// consumer maps "claude.limit.rate_limited" and "codex.limit.rate_limited" to
// different text.
func ClassifyFor(h Harness, text string, status int) Classification {
	return classify(h, text, status)
}

func classify(h Harness, text string, status int) Classification {
	low := strings.ToLower(text)
	for _, r := range classRules {
		for _, phrase := range r.phrases {
			if containsWord(low, phrase) {
				return Classification{
					Class:   r.class,
					Code:    codeFor(h, r.code),
					Message: strings.TrimSpace(text),
				}
			}
		}
	}
	if status > 0 {
		if c, ok := classForStatus(status); ok {
			return Classification{Class: c, Code: codeFor(h, "http_"+fmt.Sprint(status)), Message: strings.TrimSpace(text)}
		}
	}
	return Classification{Class: FailUnknown, Code: codeFor(h, "unknown"), Message: strings.TrimSpace(text)}
}

// classForStatus maps an HTTP status the wire reported on its own, where no
// text was available to match.
func classForStatus(status int) (FailureClass, bool) {
	switch status {
	case 401, 403:
		return FailAuth, true
	case 402, 429:
		return FailLimit, true
	case 502, 503, 504, 529:
		return FailOverloaded, true
	}
	return FailNone, false
}

func codeFor(h Harness, reason string) string {
	if h == "" {
		return reason
	}
	return string(h) + "." + reason
}

// containsWord reports whether needle appears in haystack delimited by
// non-alphanumeric characters. Underscores count as delimiters, so a vendor
// token such as "error_max_turns" still yields its words.
//
// A needle that is only an HTTP status number ("429") is ambiguous: the same
// digits occur in ordinary text. It matches only when it is error-shaped: the
// number is the only token in the text, or an error cue word stands in front
// of it. The caller contract is unchanged; pass error-shaped text.
func containsWord(haystack, needle string) bool {
	if needle == "" {
		return false
	}
	numeric := isAllDigits(needle)
	for off := 0; ; {
		i := strings.Index(haystack[off:], needle)
		if i < 0 {
			return false
		}
		start := off + i
		end := start + len(needle)
		if boundaryBefore(haystack, start) && boundaryAfter(haystack, end) &&
			(!numeric || isStatusNumber(haystack, start, end)) {
			return true
		}
		off = start + 1
	}
}

// statusCueWords mark a bare number as an error-shaped HTTP status.
var statusCueWords = map[string]bool{
	"http": true, "https": true, "status": true, "code": true,
	"api": true, "error": true, "err": true, "response": true,
}

// isStatusNumber reports whether the number at [start,end) is error-shaped: it
// is the only token in the text, or an error cue word sits in front of it.
func isStatusNumber(s string, start, end int) bool {
	if !hasToken(s[:start]) && !hasToken(s[end:]) {
		return true
	}
	i := start
	for i > 0 && !isWordByte(s[i-1]) {
		i--
	}
	j := i
	for j > 0 && isWordByte(s[j-1]) {
		j--
	}
	if j == i {
		return false
	}
	return statusCueWords[s[j:i]]
}

// hasToken reports whether s holds at least one alphanumeric character.
func hasToken(s string) bool {
	for i := range len(s) {
		if isWordByte(s[i]) {
			return true
		}
	}
	return false
}

// isAllDigits reports whether s is one or more ASCII digits.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func boundaryBefore(s string, i int) bool {
	if i == 0 {
		return true
	}
	return !isWordByte(s[i-1])
}

func boundaryAfter(s string, i int) bool {
	if i >= len(s) {
		return true
	}
	return !isWordByte(s[i])
}

func isWordByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// ClassifyResult classifies an error result frame.
func ClassifyResult(r *Result) Classification {
	return classifyResult("", r)
}

// ClassifyResultFor is ClassifyResult with the harness recorded in the code.
func ClassifyResultFor(h Harness, r *Result) Classification {
	return classifyResult(h, r)
}

func classifyResult(h Harness, r *Result) Classification {
	if r == nil {
		return Classification{Class: FailUnknown, Code: codeFor(h, "unknown")}
	}
	text := strings.TrimSpace(r.Text)
	if text == "" {
		text = r.Subtype
	}
	c := classify(h, text, 0)
	if c.Class == FailUnknown && r.Subtype != "" && r.Subtype != "success" {
		// error_max_turns / error_during_execution are harness failures, not
		// entitlement classes.
		c = Classification{Class: FailNone, Code: codeFor(h, r.Subtype), Message: text}
	}
	if c.Message == "" {
		c.Message = text
	}
	return c
}
