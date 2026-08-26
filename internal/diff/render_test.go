// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package diff

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/waveoffai/waveoff/api/v1alpha1"
)

var update = flag.Bool("update", false, "rewrite the golden files")

var scenarios = map[string]func() (*aSpec, *aSpec){
	"behavioural": scenarioBehavioural,
	"provenance":  scenarioProvenance,
	"identical":   scenarioIdentical,
	"cross-agent": scenarioCrossAgent,
}

func compare(t *testing.T, name string) *Result {
	t.Helper()
	a, b := scenarios[name]()
	r, err := Compare(a, b, Options{NameA: "support-agent-6b1d4e8f2a01", NameB: "support-agent-7f3a9c2b4d10"})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run: go test ./internal/diff -update)", err)
	}
	if !bytes.Equal(want, got) {
		t.Errorf("%s does not match.\n--- want ---\n%s\n--- got ---\n%s", path, want, got)
	}
}

func TestRenderTextGolden(t *testing.T) {
	for name := range scenarios {
		t.Run(name, func(t *testing.T) {
			var b bytes.Buffer
			if err := RenderText(&b, compare(t, name), NoColor); err != nil {
				t.Fatal(err)
			}
			golden(t, name+".txt", b.Bytes())
		})
	}
}

func TestRenderJSONGolden(t *testing.T) {
	for name := range scenarios {
		t.Run(name, func(t *testing.T) {
			r := compare(t, name)
			// Digests are stable but unreadable in a golden file, and they are
			// already covered by the digest package's own tests.
			r.BehaviorDigest = Digests{A: "sha256:<a>", B: "sha256:<b>"}
			r.ContentDigest = Digests{A: "sha256:<a>", B: "sha256:<b>"}
			var b bytes.Buffer
			if err := RenderJSON(&b, r); err != nil {
				t.Fatal(err)
			}
			golden(t, name+".json", b.Bytes())
		})
	}
}

func TestVerdictExitCodes(t *testing.T) {
	want := map[string]int{"identical": 0, "provenance": 1, "behavioural": 2, "cross-agent": ExitUsage}
	for name, code := range want {
		if got := compare(t, name).Verdict.ExitCode(); got != code {
			t.Errorf("%s: exit code %d, want %d", name, got, code)
		}
	}
}

// TestCrossAgentRefusesByDefault: a comparison between two different agents
// produces a verdict-shaped answer that means nothing, and someone will act on
// it. Refusing is the useful behaviour; --force is the escape hatch.
func TestCrossAgentRefusesByDefault(t *testing.T) {
	a, b := scenarioCrossAgent()
	r, err := Compare(a, b, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Verdict != VerdictCrossAgent {
		t.Fatalf("verdict = %s, want cross-agent", r.Verdict)
	}
	if len(r.Changes) != 0 {
		t.Error("a refused comparison must not report changes")
	}

	forced, err := Compare(a, b, Options{AllowCrossAgent: true})
	if err != nil {
		t.Fatal(err)
	}
	if forced.Verdict == VerdictCrossAgent {
		t.Error("--force must produce a real verdict")
	}
}

// TestSecuritySeverityIsMarked: a rewritten tool description is the shape of an
// MCP tool-poisoning attack. It must never render as an ordinary change.
func TestSecuritySeverityIsMarked(t *testing.T) {
	a, b := base(), base()
	b.Tools[0].ContractDigest = h(9)

	r, err := Compare(a, b, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Changes) != 1 {
		t.Fatalf("expected one change, got %d", len(r.Changes))
	}
	c := r.Changes[0]
	if !c.Severity || !c.has(TagSecurity) {
		t.Errorf("a contract-digest change must be marked severe and security-relevant, got %+v", c)
	}

	var out bytes.Buffer
	RenderText(&out, r, NoColor)
	if !strings.Contains(out.String(), "\n! tools") {
		t.Errorf("the tools plane must carry the ! marker:\n%s", out.String())
	}
}

func TestEffectWideningIsFlagged(t *testing.T) {
	for _, tc := range []struct {
		from, to v1alpha1.ToolEffect
		widened  bool
	}{
		{v1alpha1.EffectRead, v1alpha1.EffectWrite, true},
		{v1alpha1.EffectRead, v1alpha1.EffectIdempotentWrite, true},
		{v1alpha1.EffectIdempotentWrite, v1alpha1.EffectWrite, true},
		{v1alpha1.EffectWrite, v1alpha1.EffectRead, false},
	} {
		a, b := base(), base()
		a.Tools[1].Effect = tc.from
		b.Tools[1].Effect = tc.to
		r, err := Compare(a, b, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if len(r.Changes) != 1 {
			t.Fatalf("%s→%s: expected one change, got %d", tc.from, tc.to, len(r.Changes))
		}
		if got := r.Changes[0].has(TagSecurity); got != tc.widened {
			t.Errorf("%s→%s: security tag = %v, want %v", tc.from, tc.to, got, tc.widened)
		}
	}
}

// TestStaleCalibrationIsFlagged covers gating the gate: if the judge changed
// and its agreement with the human gold set was not re-measured, the number the
// gate is about to trust was measured against a judge that no longer exists.
func TestStaleCalibrationIsFlagged(t *testing.T) {
	a, b := base(), base()
	b.Judges[0].Model = "claude-opus-5" // judge swapped, calibration untouched

	r, err := Compare(a, b, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, c := range r.Changes {
		for _, d := range c.Detail {
			if strings.Contains(d, "stale") {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("swapping a judge model without re-calibrating must be flagged stale; got %+v", r.Changes)
	}
}

func TestPromptLineCountsWhenBodiesResolve(t *testing.T) {
	a, b := base(), base()
	b.Prompts[0].Digest = h(11)
	bodies := func(p v1alpha1.PromptRef) (string, bool) {
		if p.Digest == h(2) {
			return "one\ntwo\nthree\n", true
		}
		return "one\ntwo-changed\nthree\nfour\n", true
	}
	r, err := Compare(a, b, Options{Bodies: bodies})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Changes) != 1 || len(r.Changes[0].Detail) == 0 {
		t.Fatalf("expected a line-count detail, got %+v", r.Changes)
	}
	if got := r.Changes[0].Detail[0]; got != "(+2 −1)" {
		t.Errorf("line delta = %q, want %q", got, "(+2 −1)")
	}
}

// TestDiffDoesNotRequireResolvableBodies: the diff must work when a git remote
// is unreachable, which at 2am it often is.
func TestDiffDoesNotRequireResolvableBodies(t *testing.T) {
	a, b := base(), base()
	b.Prompts[0].Digest = h(11)
	unreachable := func(v1alpha1.PromptRef) (string, bool) { return "", false }
	r, err := Compare(a, b, Options{Bodies: unreachable})
	if err != nil {
		t.Fatalf("diff failed when prompt bodies were unreachable: %v", err)
	}
	if len(r.Changes) != 1 {
		t.Fatalf("expected the digest change to still be reported, got %+v", r.Changes)
	}
}
