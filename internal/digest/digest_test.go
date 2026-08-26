// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package digest

import (
	"encoding/json"
	"math"
	"math/rand"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"pgregory.net/rapid"

	"github.com/waveoffai/waveoff/api/v1alpha1"
)

func f(v float64) *float64 { return &v }
func i(v int64) *int64     { return &v }

func hash(n byte) string {
	return "sha256:" + strings.Repeat(string("0123456789abcdef"[n%16]), 64)
}

// fixture is a fully-populated manifest: every classified field is set, so the
// mutation tests below actually exercise each one.
func fixture() *v1alpha1.AgentManifestSpec {
	at := metav1.NewTime(time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC))
	return &v1alpha1.AgentManifestSpec{
		Agent: "support-agent",
		Code:  v1alpha1.CodeSpec{Image: "registry.internal/support-agent@" + hash(1)},
		Model: v1alpha1.ModelSpec{
			Provider: "anthropic",
			ID:       "claude-sonnet-4-6",
			Pin:      "2026-05-01",
			Params: v1alpha1.ModelParams{
				Temperature:   f(0.2),
				TopP:          f(1.0),
				TopK:          i(40),
				MaxTokens:     i(4096),
				StopSequences: []string{"</done>"},
			},
		},
		Prompts: []v1alpha1.PromptRef{
			{Name: "system", Source: "git+ssh://git.internal/prompts@a1b2c3d", Digest: hash(2)},
		},
		Tools: []v1alpha1.ToolRef{
			{Name: "jira.create_issue", Server: "https://jira-gw.internal/mcp", ContractDigest: hash(3),
				Effect: v1alpha1.EffectWrite, ReplayPolicy: v1alpha1.ReplayNoOp},
			{Name: "docs.search", Server: "https://docs-gw.internal/mcp", ContractDigest: hash(4),
				Effect: v1alpha1.EffectRead, ReplayPolicy: v1alpha1.ReplaySnapshot},
		},
		Retrieval: &v1alpha1.RetrievalSpec{IndexSnapshot: "snap-2026-08-19T04:00Z", EmbeddingModel: "voyage-3"},
		Policy:    &v1alpha1.PolicySpec{BundleDigest: hash(5)},
		Judges: []v1alpha1.JudgeSpec{
			{Name: "task-completion", Model: "claude-opus-4-1", RubricDigest: hash(6),
				Calibration: &v1alpha1.JudgeCalibration{Kappa: f(0.71), GoldSetDigest: hash(7), MeasuredAt: &at}},
		},
	}
}

func both(t *testing.T, s *v1alpha1.AgentManifestSpec) (string, string) {
	t.Helper()
	b, c, err := Both(s)
	if err != nil {
		t.Fatalf("Both: %v", err)
	}
	return b, c
}

// mutators changes exactly one classified field. Keyed by registry path so that
// TestEveryClassifiedFieldIsMutationTested can assert full coverage: a new
// field cannot be added to the map without also being proved to affect (or not
// affect) the right digest.
var mutators = map[string]func(s *v1alpha1.AgentManifestSpec){
	"agent":                    func(s *v1alpha1.AgentManifestSpec) { s.Agent = "other-agent" },
	"code.image":               func(s *v1alpha1.AgentManifestSpec) { s.Code.Image = "registry.internal/support-agent@" + hash(9) },
	"model.provider":           func(s *v1alpha1.AgentManifestSpec) { s.Model.Provider = "openai" },
	"model.id":                 func(s *v1alpha1.AgentManifestSpec) { s.Model.ID = "claude-opus-5" },
	"model.pin":                func(s *v1alpha1.AgentManifestSpec) { s.Model.Pin = "2026-08-01" },
	"model.params.temperature": func(s *v1alpha1.AgentManifestSpec) { s.Model.Params.Temperature = f(0.7) },
	"model.params.topP":        func(s *v1alpha1.AgentManifestSpec) { s.Model.Params.TopP = f(0.9) },
	"model.params.topK":        func(s *v1alpha1.AgentManifestSpec) { s.Model.Params.TopK = i(20) },
	"model.params.maxTokens":   func(s *v1alpha1.AgentManifestSpec) { s.Model.Params.MaxTokens = i(8192) },
	"model.params.stopSequences": func(s *v1alpha1.AgentManifestSpec) {
		s.Model.Params.StopSequences = []string{"</halt>"}
	},
	"prompts[].name":             func(s *v1alpha1.AgentManifestSpec) { s.Prompts[0].Name = "preamble" },
	"prompts[].source":           func(s *v1alpha1.AgentManifestSpec) { s.Prompts[0].Source = "git+ssh://elsewhere/prompts@ffffff1" },
	"prompts[].digest":           func(s *v1alpha1.AgentManifestSpec) { s.Prompts[0].Digest = hash(10) },
	"tools[].name":               func(s *v1alpha1.AgentManifestSpec) { s.Tools[0].Name = "jira.transition_issue" },
	"tools[].server":             func(s *v1alpha1.AgentManifestSpec) { s.Tools[0].Server = "https://jira-staging.internal/mcp" },
	"tools[].contractDigest":     func(s *v1alpha1.AgentManifestSpec) { s.Tools[0].ContractDigest = hash(11) },
	"tools[].effect":             func(s *v1alpha1.AgentManifestSpec) { s.Tools[0].Effect = v1alpha1.EffectIdempotentWrite },
	"tools[].replayPolicy":       func(s *v1alpha1.AgentManifestSpec) { s.Tools[0].ReplayPolicy = v1alpha1.ReplaySnapshot },
	"retrieval.indexSnapshot":    func(s *v1alpha1.AgentManifestSpec) { s.Retrieval.IndexSnapshot = "snap-2026-08-26T04:00Z" },
	"retrieval.embeddingModel":   func(s *v1alpha1.AgentManifestSpec) { s.Retrieval.EmbeddingModel = "voyage-4" },
	"policy.bundleDigest":        func(s *v1alpha1.AgentManifestSpec) { s.Policy.BundleDigest = hash(12) },
	"judges[].name":              func(s *v1alpha1.AgentManifestSpec) { s.Judges[0].Name = "helpfulness" },
	"judges[].model":             func(s *v1alpha1.AgentManifestSpec) { s.Judges[0].Model = "claude-opus-5" },
	"judges[].rubricDigest":      func(s *v1alpha1.AgentManifestSpec) { s.Judges[0].RubricDigest = hash(13) },
	"judges[].calibration.kappa": func(s *v1alpha1.AgentManifestSpec) { s.Judges[0].Calibration.Kappa = f(0.83) },
	"judges[].calibration.goldSetDigest": func(s *v1alpha1.AgentManifestSpec) {
		s.Judges[0].Calibration.GoldSetDigest = hash(14)
	},
	"judges[].calibration.measuredAt": func(s *v1alpha1.AgentManifestSpec) {
		at := metav1.NewTime(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
		s.Judges[0].Calibration.MeasuredAt = &at
	},
}

func TestEveryClassifiedFieldIsMutationTested(t *testing.T) {
	for _, p := range Paths() {
		if _, ok := mutators[p]; !ok {
			t.Errorf("no mutator for %q: add one so its effect on each digest is proved, not assumed", p)
		}
	}
}

// TestClassificationHoldsUnderMutation is the central claim of the whole
// design. Every InBoth field must move behaviorDigest; every ContentOnly field
// must leave it alone while still moving contentDigest, so nothing escapes the
// record.
func TestClassificationHoldsUnderMutation(t *testing.T) {
	baseB, baseC := both(t, fixture())

	for _, fld := range Registry {
		t.Run(fld.Path, func(t *testing.T) {
			s := fixture()
			mutators[fld.Path](s)
			gotB, gotC := both(t, s)

			if gotC == baseC {
				t.Fatalf("contentDigest unchanged after mutating %s; nothing may escape the tamper-evidence hash", fld.Path)
			}
			switch fld.Inclusion {
			case InBoth:
				if gotB == baseB {
					t.Errorf("behaviorDigest unchanged after mutating %s, which is classified InBoth.\n"+
						"A real behavioural change would promote with no canary.", fld.Path)
				}
			case ContentOnly:
				if gotB != baseB {
					t.Errorf("behaviorDigest changed after mutating %s, which is classified ContentOnly.\n"+
						"Provenance churn is minting new agent identities and forcing needless canaries.", fld.Path)
				}
			}
		})
	}
}

// TestRegistryMigrationIsProvenanceOnly is the case the dual digest exists to
// serve: the same bytes served from a different registry are the same agent,
// and must promote without a canary while still being recorded.
func TestRegistryMigrationIsProvenanceOnly(t *testing.T) {
	a := fixture()
	b := fixture()
	b.Code.Image = "mirror.example.com/team/support-agent@" + hash(1)

	aB, aC := both(t, a)
	bB, bC := both(t, b)

	if aB != bB {
		t.Errorf("behaviorDigest moved on a registry change; the same sha256 is the same bytes:\n  %s\n  %s", aB, bB)
	}
	if aC == bC {
		t.Error("contentDigest did not move on a registry change; the move must still be recorded")
	}
}

func TestAbsentAndZeroHashDifferently(t *testing.T) {
	// An unset temperature takes the provider default. Zero is greedy
	// decoding. Collapsing them would hide a real behavioural change.
	unset := fixture()
	unset.Model.Params.Temperature = nil
	zero := fixture()
	zero.Model.Params.Temperature = f(0)

	uB, _ := both(t, unset)
	zB, _ := both(t, zero)
	if uB == zB {
		t.Error("temperature unset and temperature 0 produced the same behaviorDigest")
	}

	// And neither may collide with the populated fixture.
	baseB, _ := both(t, fixture())
	if uB == baseB || zB == baseB {
		t.Error("absent/zero temperature collided with a set temperature")
	}
}

func TestEmptyNormalisesToAbsent(t *testing.T) {
	cases := []struct {
		name        string
		empty, nil_ func(*v1alpha1.AgentManifestSpec)
	}{
		{"tools", func(s *v1alpha1.AgentManifestSpec) { s.Tools = []v1alpha1.ToolRef{} }, func(s *v1alpha1.AgentManifestSpec) { s.Tools = nil }},
		{"prompts", func(s *v1alpha1.AgentManifestSpec) { s.Prompts = []v1alpha1.PromptRef{} }, func(s *v1alpha1.AgentManifestSpec) { s.Prompts = nil }},
		{"judges", func(s *v1alpha1.AgentManifestSpec) { s.Judges = []v1alpha1.JudgeSpec{} }, func(s *v1alpha1.AgentManifestSpec) { s.Judges = nil }},
		{"stopSequences", func(s *v1alpha1.AgentManifestSpec) { s.Model.Params.StopSequences = []string{} }, func(s *v1alpha1.AgentManifestSpec) { s.Model.Params.StopSequences = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, n := fixture(), fixture()
			tc.empty(e)
			tc.nil_(n)
			eB, eC := both(t, e)
			nB, nC := both(t, n)
			if eB != nB || eC != nC {
				t.Errorf("empty and absent %s hashed differently; they describe the same agent", tc.name)
			}
		})
	}
}

// TestRequiredFieldsSurviveEmptiness pins the consequence of building the
// projection by hand rather than marshalling the struct: a Required field is
// emitted even when empty. Under json.Marshal, adding `omitempty` to any of
// these would silently change every digest ever issued.
func TestRequiredFieldsSurviveEmptiness(t *testing.T) {
	s := fixture()
	s.Tools[0].Effect = ""
	raw, err := CanonicalJSON(s, ScopeBehavior)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"effect":""`) {
		t.Errorf("an empty Required field was dropped from the projection; got:\n%s", raw)
	}
}

// TestListOrderIsIrrelevant: tools, prompts and judges are sets keyed by name.
// Reordering a YAML file must never change what the manifest identifies.
func TestListOrderIsIrrelevant(t *testing.T) {
	baseB, baseC := both(t, fixture())
	rapid.Check(t, func(rt *rapid.T) {
		seed := rapid.Int64().Draw(rt, "seed")
		r := rand.New(rand.NewSource(seed))
		s := fixture()
		r.Shuffle(len(s.Tools), func(i, j int) { s.Tools[i], s.Tools[j] = s.Tools[j], s.Tools[i] })
		r.Shuffle(len(s.Prompts), func(i, j int) { s.Prompts[i], s.Prompts[j] = s.Prompts[j], s.Prompts[i] })

		b, c, err := Both(s)
		if err != nil {
			rt.Fatal(err)
		}
		if b != baseB || c != baseC {
			rt.Fatalf("digest moved after reordering lists (seed %d)", seed)
		}
	})
}

// TestJSONRoundTripIsStable covers the hazard that motivated the whole
// canonicalisation choice: values arrive through the API server, which decodes
// integral JSON numbers as int64 and everything else as float64, then
// re-encodes. RFC 8785 renders 1.0 and 1 identically, so the round trip cannot
// move a digest — but only if we actually go through it.
func TestJSONRoundTripIsStable(t *testing.T) {
	for _, literal := range []string{"1", "1.0", "1.00", "1e0"} {
		t.Run(literal, func(t *testing.T) {
			base := fixture()
			base.Model.Params.TopP = f(1)
			want, _ := both(t, base)

			raw, err := json.Marshal(base)
			if err != nil {
				t.Fatal(err)
			}
			// Substitute the literal as it might have arrived over the wire.
			swapped := strings.Replace(string(raw), `"topP":1`, `"topP":`+literal, 1)

			var back v1alpha1.AgentManifestSpec
			if err := json.Unmarshal([]byte(swapped), &back); err != nil {
				t.Fatal(err)
			}
			got, _ := both(t, &back)
			if got != want {
				t.Errorf("topP written as %s produced a different digest\n want %s\n  got %s", literal, want, got)
			}
		})
	}
}

// TestAwkwardFloatsAreStable exercises the values that break naive float
// formatting.
func TestAwkwardFloatsAreStable(t *testing.T) {
	for _, v := range []float64{0.1 + 0.2, 1e21, 1e-7, math.SmallestNonzeroFloat64, math.MaxFloat64, math.Copysign(0, -1)} {
		s1, s2 := fixture(), fixture()
		s1.Model.Params.Temperature = f(v)
		s2.Model.Params.Temperature = f(v)
		a, _ := both(t, s1)
		b, _ := both(t, s2)
		if a != b {
			t.Errorf("temperature %v is not stable across two computations", v)
		}
		raw, err := CanonicalJSON(s1, ScopeBehavior)
		if err != nil {
			t.Fatalf("temperature %v: %v", v, err)
		}
		if len(raw) == 0 {
			t.Fatalf("temperature %v produced empty canonical form", v)
		}
	}
}

func TestNonFiniteIsRejected(t *testing.T) {
	for name, v := range map[string]float64{"NaN": math.NaN(), "+Inf": math.Inf(1), "-Inf": math.Inf(-1)} {
		s := fixture()
		s.Model.Params.Temperature = f(v)
		if _, _, err := Both(s); err == nil {
			t.Errorf("%s temperature was accepted; JSON cannot represent it", name)
		}
	}
}

func TestDigestIsDeterministicAcrossRuns(t *testing.T) {
	// Guards against map iteration order leaking into the hash.
	first, _ := both(t, fixture())
	for n := 0; n < 200; n++ {
		got, _ := both(t, fixture())
		if got != first {
			t.Fatalf("behaviorDigest is not deterministic: %s then %s", first, got)
		}
	}
}

func TestNameDerivation(t *testing.T) {
	b, c := both(t, fixture())
	name := Name("support-agent", b)
	if want := "support-agent-" + b[7:7+Prefix]; name != want {
		t.Errorf("Name = %q, want %q", name, want)
	}
	if got := len(Short(b, Prefix)); got != 12 {
		t.Errorf("prefix length = %d, want 12 (48 bits)", got)
	}
	dis := DisambiguatedName("support-agent", b, c)
	if !strings.HasPrefix(dis, name+".") {
		t.Errorf("DisambiguatedName %q must extend the canonical name %q", dis, name)
	}
}
