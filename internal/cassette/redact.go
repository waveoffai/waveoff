// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package cassette

import (
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
)

// Redactor strips credentials from traffic before it is recorded.
//
// This runs in flight, before anything reaches a span or the blob store, so a
// secret is never written down and then cleaned up. That ordering is the whole
// point: a corpus is kept for months and shared with eval vendors, and a
// credential that reaches disk has to be treated as disclosed no matter what
// happens afterwards.
//
// The default rules err towards over-redaction. A cassette missing a header it
// did not need is a small loss; a cassette carrying a live API key is an
// incident, and the operator will not find out from us.
type Redactor struct {
	headers  map[string]struct{}
	patterns []labelledPattern
}

type labelledPattern struct {
	label string
	re    *regexp.Regexp
	// replacement is an expansion template used instead of a flat placeholder,
	// for rules that must preserve part of what they match.
	replacement string
}

// DefaultRedactedHeaders are removed from every recorded request and response.
var DefaultRedactedHeaders = []string{
	"authorization",
	"proxy-authorization",
	"x-api-key",
	"api-key",
	"x-auth-token",
	"x-access-token",
	"x-session-token",
	"x-amz-security-token",
	"cookie",
	"set-cookie",
	"x-goog-api-key",
	"openai-organization",
	"mcp-session-id",
}

// defaultPatterns match credential shapes wherever they appear in a body.
//
// A key does not only travel in a header. It ends up pasted into a prompt, in a
// tool argument, in an error message echoed back by an upstream service. Header
// stripping alone would leave all of those in the corpus.
var defaultPatterns = []labelledPattern{
	{label: "anthropic-key", re: regexp.MustCompile(`sk-ant-[A-Za-z0-9_\-]{16,}`)},
	{label: "openai-key", re: regexp.MustCompile(`sk-(?:proj-|svcacct-)?[A-Za-z0-9_\-]{20,}`)},
	{label: "google-key", re: regexp.MustCompile(`AIza[0-9A-Za-z_\-]{35}`)},
	{label: "aws-access-key", re: regexp.MustCompile(`(?:AKIA|ASIA|ABIA|ACCA)[0-9A-Z]{16}`)},
	{label: "github-token", re: regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{36,}`)},
	{label: "slack-token", re: regexp.MustCompile(`xox[baprs]-[A-Za-z0-9\-]{10,}`)},
	{label: "stripe-key", re: regexp.MustCompile(`(?:sk|rk)_(?:live|test)_[A-Za-z0-9]{16,}`)},
	{label: "bearer-token", re: regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._\-~+/]{20,}={0,2}`)},
	{label: "jwt", re: regexp.MustCompile(`eyJ[A-Za-z0-9_\-]{8,}\.eyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}`)},
	{label: "private-key", re: regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`)},
	// A JSON field whose name says it is a secret, whatever its value looks
	// like. This is the rule that catches the credential shapes nobody has
	// published a regex for yet.
	//
	// The optional backslashes matter more than they look. Agent traffic is
	// full of JSON nested inside JSON — a tool result arrives as a string field
	// containing a whole encoded document — so the same member appears as both
	// "api_key":"..." and \"api_key\":\"...\". A rule that only handles the
	// first form looks like coverage and leaks every tool result.
	//
	// The key, the separator and the quote style are all preserved, so the
	// payload stays valid JSON at whatever nesting depth it was found and a
	// reader can still see the shape of the request.
	{
		label:       "secret-field",
		re:          regexp.MustCompile(`(?i)(\\?"(?:api[_-]?key|access[_-]?token|refresh[_-]?token|auth[_-]?token|client[_-]?secret|password|passwd|secret|private[_-]?key|credential|token)\\?"\s*:\s*)(\\?)"(?:[^"\\]|\\.)*?(\\?)"`),
		replacement: `${1}${2}"` + placeholderFor("secret-field") + `${3}"`,
	},
}

// placeholderFor exists so the replacement templates above and Placeholder
// below cannot drift apart.
func placeholderFor(label string) string { return "[REDACTED:" + label + "]" }

// NewRedactor builds a redactor from the defaults plus any operator additions.
//
// extraHeaders are matched case-insensitively. extraPatterns are regular
// expressions; an invalid one is an error rather than a silently skipped rule,
// because a redaction rule that quietly does nothing is worse than no rule at
// all — the operator believes they are covered.
func NewRedactor(extraHeaders []string, extraPatterns []string) (*Redactor, error) {
	r := &Redactor{headers: map[string]struct{}{}}
	for _, h := range DefaultRedactedHeaders {
		r.headers[strings.ToLower(h)] = struct{}{}
	}
	for _, h := range extraHeaders {
		if h = strings.TrimSpace(strings.ToLower(h)); h != "" {
			r.headers[h] = struct{}{}
		}
	}

	r.patterns = append(r.patterns, defaultPatterns...)
	for i, p := range extraPatterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("redaction pattern %d (%q): %w", i, p, err)
		}
		r.patterns = append(r.patterns, labelledPattern{label: fmt.Sprintf("custom-%d", i), re: re})
	}
	return r, nil
}

// MustRedactor is NewRedactor with the defaults only.
func MustRedactor() *Redactor {
	r, err := NewRedactor(nil, nil)
	if err != nil {
		panic(err)
	}
	return r
}

// Placeholder replaces a redacted value. It is deliberately visible: a reader
// must be able to tell a stripped value from one that was never there, or they
// will misread a cassette as evidence that no credential was sent.
func Placeholder(label string) string { return placeholderFor(label) }

// Headers returns a copy of h with credential-bearing headers replaced, along
// with the sorted names of whatever was removed.
func (r *Redactor) Headers(h http.Header) (http.Header, []string) {
	out := make(http.Header, len(h))
	var removed []string
	for name, values := range h {
		if _, secret := r.headers[strings.ToLower(name)]; secret {
			out[name] = []string{Placeholder("header")}
			removed = append(removed, strings.ToLower(name))
			continue
		}
		out[name] = append([]string(nil), values...)
	}
	sort.Strings(removed)
	return out, removed
}

// Body scrubs credential shapes out of a payload, returning the cleaned bytes
// and the sorted labels of the rules that fired.
//
// The returned slice is always a distinct allocation when anything changed, so
// a caller cannot accidentally hold the unredacted original.
func (r *Redactor) Body(b []byte) ([]byte, []string) {
	if len(b) == 0 {
		return b, nil
	}
	out := b
	var fired []string
	for _, p := range r.patterns {
		if !p.re.Match(out) {
			continue
		}
		fired = append(fired, p.label)
		if p.replacement != "" {
			out = p.re.ReplaceAll(out, []byte(p.replacement))
			continue
		}
		out = p.re.ReplaceAll(out, []byte(Placeholder(p.label)))
	}
	sort.Strings(fired)
	return out, dedupe(fired)
}

func dedupe(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := in[:1]
	for _, s := range in[1:] {
		if s != out[len(out)-1] {
			out = append(out, s)
		}
	}
	return out
}
