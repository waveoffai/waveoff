// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package cassette

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
)

func TestHeadersAreStripped(t *testing.T) {
	r := MustRedactor()
	h := http.Header{
		"Authorization":  {"Bearer sk-ant-api03-REALKEYMATERIALHERE1234567890"},
		"X-Api-Key":      {"sk-ant-api03-anotherrealkey1234567890"},
		"Cookie":         {"session=abc123"},
		"Mcp-Session-Id": {"3428dc7f-2281-4e99-8a8e-d0db36973e66"},
		"Content-Type":   {"application/json"},
		"User-Agent":     {"langgraph/0.2"},
	}

	got, removed := r.Headers(h)

	for _, secret := range []string{"Authorization", "X-Api-Key", "Cookie", "Mcp-Session-Id"} {
		if v := got.Get(secret); !strings.HasPrefix(v, "[REDACTED:") {
			t.Errorf("%s survived redaction as %q", secret, v)
		}
	}
	// Non-credential headers are part of the recording and must survive: they
	// are how a replay reproduces the same request.
	if got.Get("Content-Type") != "application/json" {
		t.Error("Content-Type was redacted; it carries no credential")
	}
	if got.Get("User-Agent") != "langgraph/0.2" {
		t.Error("User-Agent was redacted")
	}
	if len(removed) != 4 {
		t.Errorf("reported %v removed, want 4 entries", removed)
	}

	// The caller's header map must not be mutated: it is still in flight to
	// the upstream, which needs the real credential.
	if h.Get("Authorization") == got.Get("Authorization") {
		t.Error("redaction mutated the caller's headers; the upstream request would lose its credential")
	}
}

// TestCredentialShapesInBodies is the case header stripping does not cover. A
// key ends up pasted into a prompt, in a tool argument, or echoed back inside
// an upstream error message, and all of those reach the corpus.
func TestCredentialShapesInBodies(t *testing.T) {
	cases := map[string]string{
		"anthropic":   `{"prompt":"use sk-ant-api03-Zx8fK2mQpL9vN4bR7tY1wE5uI0oP3aS6dF"}`,
		"openai":      `{"note":"key is sk-proj-abcdefghijklmnopqrstuvwxyz0123456789"}`,
		"google":      `{"url":"https://x/?key=AIzaSyD-1234567890abcdefghijklmnopqrstuv"}`,
		"aws":         `{"creds":"AKIAIOSFODNN7EXAMPLE"}`,
		"github":      `{"token":"ghp_abcdefghijklmnopqrstuvwxyz0123456789"}`,
		"slack":       `{"t":"xoxb-1234567890-abcdefghijkl"}`,
		"stripe":      `{"k":"sk_live_abcdefghijklmnopqrstuvwx"}`,
		"bearer":      `{"h":"Bearer abcdefghijklmnopqrstuvwxyz012345"}`,
		"jwt":         `{"t":"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U"}`,
		"private-key": "{\"k\":\"-----BEGIN RSA PRIVATE KEY-----\\nMIIEowIBAAKCAQEA\\n-----END RSA PRIVATE KEY-----\"}",
	}

	r := MustRedactor()
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			out, fired := r.Body([]byte(body))
			if len(fired) == 0 {
				t.Fatalf("no rule fired for %s:\n%s", name, body)
			}
			if !strings.Contains(string(out), "[REDACTED:") {
				t.Errorf("nothing was redacted:\n%s", out)
			}
			// The specific secret material must be gone.
			for _, secret := range []string{
				"Zx8fK2mQpL9vN4bR7tY1wE5uI0oP3aS6dF", "abcdefghijklmnopqrstuvwxyz0123456789",
				"AIzaSyD-1234567890abcdefghijklmnopqrstuv", "AKIAIOSFODNN7EXAMPLE",
				"MIIEowIBAAKCAQEA", "dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U",
			} {
				if strings.Contains(body, secret) && strings.Contains(string(out), secret) {
					t.Errorf("%s survived redaction:\n%s", secret, out)
				}
			}
		})
	}
}

// TestSecretFieldsByName is the rule that catches credential shapes nobody has
// published a regex for: the field name says it is a secret.
func TestSecretFieldsByName(t *testing.T) {
	r := MustRedactor()
	body := `{"api_key":"whatever-shape-this-is","client_secret":"hunter2","password":"p","refresh_token":"rt","harmless":"keep me"}`

	out, fired := r.Body([]byte(body))
	got := string(out)

	for _, secret := range []string{"whatever-shape-this-is", "hunter2", `"rt"`} {
		if strings.Contains(got, secret) {
			t.Errorf("%s survived:\n%s", secret, got)
		}
	}
	// The shape of the payload must survive so a reader can still tell what
	// the request looked like.
	for _, key := range []string{"api_key", "client_secret", "password", "refresh_token"} {
		if !strings.Contains(got, key) {
			t.Errorf("field name %q was destroyed; the payload shape should survive:\n%s", key, got)
		}
	}
	if !strings.Contains(got, `"harmless":"keep me"`) {
		t.Errorf("a non-secret field was redacted:\n%s", got)
	}
	if len(fired) == 0 {
		t.Error("no rule reported firing")
	}
}

// TestRedactionIsVisible: a reader must be able to tell a stripped value from
// one that was never sent, or they will misread the cassette as evidence that
// no credential was involved.
func TestRedactionIsVisible(t *testing.T) {
	r := MustRedactor()
	out, fired := r.Body([]byte(`{"k":"sk-ant-api03-Zx8fK2mQpL9vN4bR7tY1wE5uI0oP3aS6dF"}`))
	if !strings.Contains(string(out), "[REDACTED:anthropic-key]") {
		t.Errorf("the placeholder should name the rule that fired:\n%s", out)
	}
	if fired[0] != "anthropic-key" {
		t.Errorf("fired = %v", fired)
	}
}

func TestCleanBodyIsUntouched(t *testing.T) {
	r := MustRedactor()
	body := []byte(`{"messages":[{"role":"user","content":"what is the refund policy?"}]}`)
	out, fired := r.Body(body)
	if string(out) != string(body) {
		t.Errorf("a clean body was altered:\n want %s\n  got %s", body, out)
	}
	if len(fired) != 0 {
		t.Errorf("rules fired on a clean body: %v", fired)
	}
}

func TestOperatorRulesAreAdditive(t *testing.T) {
	r, err := NewRedactor([]string{"X-Internal-Trace"}, []string{`CUST-[0-9]{6}`})
	if err != nil {
		t.Fatal(err)
	}
	h, removed := r.Headers(http.Header{"X-Internal-Trace": {"abc"}, "Authorization": {"Bearer xyz"}})
	if !strings.HasPrefix(h.Get("X-Internal-Trace"), "[REDACTED:") {
		t.Error("the operator's extra header was not redacted")
	}
	if len(removed) != 2 {
		t.Errorf("removed = %v; operator rules must add to the defaults, not replace them", removed)
	}

	out, fired := r.Body([]byte(`{"account":"CUST-123456"}`))
	if strings.Contains(string(out), "CUST-123456") {
		t.Errorf("the operator's pattern did not fire:\n%s", out)
	}
	if len(fired) == 0 {
		t.Error("no rule reported firing")
	}
}

// TestInvalidPatternIsRejected: a redaction rule that silently does nothing is
// worse than no rule, because the operator believes they are covered.
func TestInvalidPatternIsRejected(t *testing.T) {
	if _, err := NewRedactor(nil, []string{"([unclosed"}); err == nil {
		t.Fatal("an invalid redaction pattern was accepted")
	}
}

// TestNoDefaultPatternIsVacuous guards against a rule that can never match,
// which would look like coverage in review and provide none.
func TestNoDefaultPatternIsVacuous(t *testing.T) {
	for _, p := range defaultPatterns {
		if p.re.String() == "" {
			t.Errorf("%s has an empty pattern", p.label)
		}
		if _, err := regexp.Compile(p.re.String()); err != nil {
			t.Errorf("%s does not compile: %v", p.label, err)
		}
	}
}

// TestRealisticSessionLeaksNothing is the canary: a payload shaped like real
// agent traffic, carrying credentials in the several places they actually turn
// up, must come out clean in one pass.
func TestRealisticSessionLeaksNothing(t *testing.T) {
	body := `{
      "model": "claude-sonnet-4-6",
      "messages": [
        {"role": "system", "content": "You are a support agent."},
        {"role": "user", "content": "curl -H 'Authorization: Bearer sk-ant-api03-LEAKED1234567890abcdefgh' https://api"},
        {"role": "assistant", "content": "I cannot use that. Your AWS key AKIAIOSFODNN7EXAMPLE is also exposed."},
        {"role": "tool", "content": "{\"error\":\"invalid token\",\"sent\":{\"api_key\":\"deadbeefcafe\"}}"}
      ]
    }`

	out, fired := MustRedactor().Body([]byte(body))
	got := string(out)

	for _, secret := range []string{"sk-ant-api03-LEAKED1234567890abcdefgh", "AKIAIOSFODNN7EXAMPLE", "deadbeefcafe"} {
		if strings.Contains(got, secret) {
			t.Errorf("%q survived a realistic session:\n%s\n\nrules fired: %v", secret, got, fired)
		}
	}
	// The conversation must still be legible, or the cassette is worthless.
	if !strings.Contains(got, "You are a support agent.") {
		t.Error("redaction destroyed the prompt content")
	}
	if !strings.Contains(got, "claude-sonnet-4-6") {
		t.Error("redaction destroyed the model id")
	}
}
