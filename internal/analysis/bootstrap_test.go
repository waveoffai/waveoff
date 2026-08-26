// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package analysis_test

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/waveoffai/waveoff/internal/analysis"
)

// paired builds observations from two aligned slices.
func paired(metric string, incumbent, candidate []float64) []analysis.Observation {
	out := make([]analysis.Observation, len(incumbent))
	for i := range incumbent {
		out[i] = analysis.Observation{
			Item:      fmt.Sprintf("item-%02d", i),
			Incumbent: map[string]float64{metric: incumbent[i]},
			Candidate: map[string]float64{metric: candidate[i]},
		}
	}
	return out
}

// noisyPair generates a corpus where items vary a lot in difficulty but the two
// arms differ by a fixed amount. This is the shape real agent evaluation takes:
// enormous between-item variance, a small effect to detect.
func noisyPair(n int, effect float64, seed int64) []analysis.Observation {
	rng := rand.New(rand.NewSource(seed))
	inc := make([]float64, n)
	cand := make([]float64, n)
	for i := range inc {
		// Item difficulty, shared by both arms — the variance pairing removes.
		difficulty := rng.Float64()
		inc[i] = clamp(difficulty + rng.NormFloat64()*0.02)
		cand[i] = clamp(difficulty + effect + rng.NormFloat64()*0.02)
	}
	return paired("task-completion", inc, cand)
}

func clamp(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func request(obs []analysis.Observation, margin float64) analysis.Request {
	return analysis.Request{
		Agent: "support-agent", Incumbent: "sha256:a", Candidate: "sha256:b",
		Primary: analysis.Metric{
			Name: "task-completion", Test: analysis.TestPairedBootstrap,
			Margin: margin, Alpha: 0.05, Direction: analysis.Higher,
		},
		Observations: obs,
		Seed:         42,
	}
}

func analyze(t *testing.T, req analysis.Request) analysis.Verdict {
	t.Helper()
	v, err := (&analysis.PairedBootstrap{Resamples: 2000}).Analyze(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// TestEquivalentCandidateIsPromoted is the case that has to work, because it is
// most releases. A candidate that changed nothing measurable must not be waved
// off, or the gate blocks everything and gets switched off.
func TestEquivalentCandidateIsPromoted(t *testing.T) {
	v := analyze(t, request(noisyPair(60, 0.0, 1), -0.02))
	if v.Outcome != analysis.OutcomePromote {
		t.Errorf("an equivalent candidate was not promoted: %s — %s", v.Outcome, v.Reason)
	}
}

// TestRealRegressionIsWavedOff: a drop well past the margin must be caught.
func TestRealRegressionIsWavedOff(t *testing.T) {
	v := analyze(t, request(noisyPair(60, -0.15, 2), -0.02))
	if v.Outcome != analysis.OutcomeWaveOff {
		t.Errorf("a 15-point regression was not waved off: %s — %s", v.Outcome, v.Reason)
	}
	if !strings.Contains(v.Reason, "task-completion") {
		t.Errorf("the reason should name the metric: %q", v.Reason)
	}
}

// TestImprovementIsPromoted: a better candidate is non-inferior by definition.
func TestImprovementIsPromoted(t *testing.T) {
	v := analyze(t, request(noisyPair(60, 0.10, 3), -0.02))
	if v.Outcome != analysis.OutcomePromote {
		t.Errorf("an improved candidate was waved off: %s — %s", v.Outcome, v.Reason)
	}
	if v.Primary.Delta <= 0 {
		t.Errorf("delta = %v, want positive", v.Primary.Delta)
	}
}

// TestMarginIsRespected: the margin is the operator's judgement about their own
// product, and the test must honour it rather than a default of ours. The same
// data decides differently under different margins.
func TestMarginIsRespected(t *testing.T) {
	obs := noisyPair(80, -0.05, 4)

	strict := analyze(t, request(obs, -0.01)) // a 1-point drop is intolerable
	if strict.Outcome != analysis.OutcomeWaveOff {
		t.Errorf("a 5-point drop passed a 1-point margin: %s", strict.Reason)
	}

	tolerant := analyze(t, request(obs, -0.20)) // a 20-point drop is acceptable
	if tolerant.Outcome != analysis.OutcomePromote {
		t.Errorf("a 5-point drop failed a 20-point margin: %s", tolerant.Reason)
	}
}

// TestPairingBeatsUnpaired is the quantitative claim the design rests on: with
// large between-item variance and a small effect, the paired difference is
// detectable where comparing two independent means is not.
func TestPairingBeatsUnpaired(t *testing.T) {
	obs := noisyPair(60, -0.10, 5)

	v := analyze(t, request(obs, -0.02))
	if v.Outcome != analysis.OutcomeWaveOff {
		t.Fatalf("the paired test missed a 10-point regression in 40 items: %s", v.Reason)
	}

	// The same data, unpaired: the spread of item difficulty swamps the effect.
	inc, cand, _ := v2Values(obs)
	pooled := spread(inc) + spread(cand)
	effect := mean(cand) - mean(inc)
	if pooled < absF(effect)*4 {
		t.Skip("the generated corpus is not noisy enough for this comparison to be meaningful")
	}
	// The paired interval must be far tighter than the raw spread.
	width := v.Primary.CIUpper - v.Primary.CILower
	if width >= pooled {
		t.Errorf("the paired interval (%.4f wide) is no tighter than the unpaired spread (%.4f); "+
			"pairing is supposed to remove item-level variance", width, pooled)
	}
}

// TestLowerIsBetterFlipsTheComparison: cost and escalation rate are metrics
// where an increase is the regression, and getting the direction wrong means
// the gate promotes exactly what it should block.
func TestLowerIsBetterFlipsTheComparison(t *testing.T) {
	// Candidate costs more.
	inc := make([]float64, 60)
	cand := make([]float64, 60)
	rng := rand.New(rand.NewSource(7))
	for i := range inc {
		base := 0.04 + rng.Float64()*0.02
		inc[i] = base
		cand[i] = base + 0.03 // 3 cents more per task
	}

	req := analysis.Request{
		Primary: analysis.Metric{
			Name: "cost-per-completed-task", Test: analysis.TestPairedBootstrap,
			Margin: 0.01, Alpha: 0.05, Direction: analysis.Lower,
		},
		Observations: paired("cost-per-completed-task", inc, cand),
		Seed:         1,
	}
	v := analyze(t, req)
	if v.Outcome != analysis.OutcomeWaveOff {
		t.Errorf("a cost increase past the margin was promoted: %s — %s", v.Outcome, v.Reason)
	}

	// And a cost reduction must pass.
	for i := range cand {
		cand[i] = inc[i] - 0.01
	}
	req.Observations = paired("cost-per-completed-task", inc, cand)
	if v := analyze(t, req); v.Outcome != analysis.OutcomePromote {
		t.Errorf("a cost reduction was waved off: %s — %s", v.Outcome, v.Reason)
	}
}

// TestGuardrailIsDecisive: no amount of improvement in the primary metric buys
// a policy violation.
func TestGuardrailIsDecisive(t *testing.T) {
	obs := noisyPair(60, 0.20, 8) // the candidate is much better
	for i := range obs {
		obs[i].Incumbent["policy-violations"] = 0
		obs[i].Candidate["policy-violations"] = 0
	}
	obs[3].Candidate["policy-violations"] = 1 // one violation

	req := request(obs, -0.02)
	req.Guardrails = []analysis.Metric{{
		Name: "policy-violations", Test: analysis.TestThreshold,
		Threshold: 0, Direction: analysis.Lower,
	}}

	v := analyze(t, req)
	if v.Outcome != analysis.OutcomeWaveOff {
		t.Fatalf("a policy violation was outweighed by a better primary metric: %s", v.Reason)
	}
	if !strings.Contains(v.Reason, "policy-violations") {
		t.Errorf("the reason should name the guardrail: %q", v.Reason)
	}
}

// TestTooFewItemsIsInconclusiveNotAPass. "We cannot tell" is not "it is fine",
// and a gate that promotes on three items is a coin toss dressed as a decision.
func TestTooFewItemsIsInconclusiveNotAPass(t *testing.T) {
	v := analyze(t, request(noisyPair(3, 0.0, 9), -0.02))
	if v.Outcome != analysis.OutcomeInconclusive {
		t.Errorf("a three-item corpus produced %s, want inconclusive: %s", v.Outcome, v.Reason)
	}
}

// TestAWideIntervalCannotDecide is the replacement for a minimum-sample
// constant: an interval that straddles the margin cannot separate acceptable
// from worse, whatever the point estimate says, and that adapts to the metric
// and its variance instead of being argued about as a number.
func TestAWideIntervalCannotDecide(t *testing.T) {
	// Enough items to clear the bootstrap floor, but the arms differ so
	// erratically that the interval spans the margin.
	rng := rand.New(rand.NewSource(31))
	inc := make([]float64, 60)
	cand := make([]float64, 60)
	for i := range inc {
		inc[i] = rng.Float64()
		cand[i] = rng.Float64()
	}
	v := analyze(t, request(paired("task-completion", inc, cand), -0.02))

	if v.Outcome != analysis.OutcomeInconclusive {
		t.Errorf("a straddling interval produced %s, want inconclusive: %s", v.Outcome, v.Reason)
	}
	if !v.Primary.Indeterminate {
		t.Error("the metric was not marked indeterminate")
	}
	if v.Primary.Inferior {
		t.Error("an undecidable result was also marked inferior")
	}
}

// TestAGenuineRegressionIsNotMerelyUndecidable: the third outcome must not
// swallow real findings.
func TestAGenuineRegressionIsNotMerelyUndecidable(t *testing.T) {
	v := analyze(t, request(noisyPair(60, -0.30, 32), -0.02))
	if v.Outcome != analysis.OutcomeWaveOff {
		t.Fatalf("a 30-point regression produced %s: %s", v.Outcome, v.Reason)
	}
	if !v.Primary.Inferior {
		t.Error("a clear regression was not marked inferior")
	}
	if v.Primary.Indeterminate {
		t.Error("a clear regression was marked indeterminate")
	}
}

// TestUnmeasuredMetricIsNotAPass: a gate that reads an unmeasured metric as
// satisfied promotes on a test that never ran.
func TestUnmeasuredMetricIsNotAPass(t *testing.T) {
	obs := noisyPair(60, 0.0, 10)
	req := request(obs, -0.02)
	req.Primary.Name = "a-metric-nobody-scored"

	v := analyze(t, req)
	if v.Outcome == analysis.OutcomePromote {
		t.Errorf("an unmeasured primary metric was promoted: %s", v.Reason)
	}
	if v.Primary.Pass {
		t.Error("an unmeasured metric was marked as passing")
	}
}

// TestReproducible: a promotion decision that cannot be recomputed from its
// inputs is not evidence.
func TestReproducible(t *testing.T) {
	req := request(noisyPair(60, -0.03, 11), -0.02)
	first := analyze(t, req)
	for i := 0; i < 5; i++ {
		again := analyze(t, req)
		if again.Outcome != first.Outcome ||
			again.Primary.CILower != first.Primary.CILower ||
			again.Primary.CIUpper != first.Primary.CIUpper {
			t.Fatalf("the same request gave a different answer:\n %+v\n %+v", first.Primary, again.Primary)
		}
	}
}

// TestGuardrailsRunAtFullAlpha.
//
// Every metric here is a non-inferiority test — the null is "this is breached"
// and the candidate must prove otherwise — so promotion requires rejecting all
// k nulls. That is an intersection-union test, whose size is bounded by the
// largest of its parts, so the false-promotion rate is already controlled at
// alpha without any correction.
//
// Correcting anyway would shrink alpha, widen every interval and make
// guardrails harder to satisfy, costing power to prove non-inferiority and
// waving off good releases to buy error control the structure already gives.
func TestGuardrailsRunAtFullAlpha(t *testing.T) {
	obs := noisyPair(60, 0.0, 12)
	for i := range obs {
		for _, name := range []string{"g1", "g2", "g3", "g4"} {
			obs[i].Incumbent[name] = 0.5
			obs[i].Candidate[name] = 0.5
		}
	}
	req := request(obs, -0.02)
	for _, name := range []string{"g1", "g2", "g3", "g4"} {
		req.Guardrails = append(req.Guardrails, analysis.Metric{
			Name: name, Test: analysis.TestPairedBootstrap, Margin: -0.05, Direction: analysis.Higher,
		})
	}

	v := analyze(t, req)
	if len(v.Guardrails) != 4 {
		t.Fatalf("guardrails = %d", len(v.Guardrails))
	}
	for _, g := range v.Guardrails {
		if g.Alpha != 0.05 {
			t.Errorf("guardrail %s ran at alpha %v; an intersection-union test needs no correction, "+
				"and shrinking alpha only costs power to prove non-inferiority", g.Name, g.Alpha)
		}
	}
}

// TestAnUndecidableGuardrailHoldsRatherThanPasses.
//
// The failure mode a detection framing would have: a guardrail with too little
// evidence to decide passes by default, and the rollout promotes. An
// underpowered guardrail is an unmeasured one wearing a p-value, and this
// project already refuses to treat an unmeasured metric as a pass.
func TestAnUndecidableGuardrailHoldsRatherThanPasses(t *testing.T) {
	// A primary metric that is clearly fine, and a guardrail whose data is
	// far too noisy to separate acceptable from worse.
	obs := noisyPair(60, 0.0, 20)
	rng := rand.New(rand.NewSource(21))
	for i := range obs {
		obs[i].Incumbent["flaky"] = rng.Float64() * 10
		obs[i].Candidate["flaky"] = rng.Float64() * 10
	}

	req := request(obs, -0.02)
	req.Guardrails = []analysis.Metric{{
		Name: "flaky", Test: analysis.TestPairedBootstrap,
		Margin: -0.01, Direction: analysis.Higher,
	}}

	v := analyze(t, req)
	if v.Outcome == analysis.OutcomePromote {
		t.Fatalf("an undecidable guardrail was treated as passing: %s", v.Reason)
	}
	if v.Outcome != analysis.OutcomeInconclusive {
		t.Errorf("outcome = %s, want inconclusive: %s", v.Outcome, v.Reason)
	}
}

func TestValidation(t *testing.T) {
	a := &analysis.PairedBootstrap{}
	if _, err := a.Analyze(context.Background(), analysis.Request{}); err == nil {
		t.Error("a request with no primary metric was accepted")
	}
	if _, err := a.Analyze(context.Background(), analysis.Request{
		Primary: analysis.Metric{Name: "m"},
	}); err == nil {
		t.Error("a request with no observations was accepted")
	}
}

// helpers

func v2Values(obs []analysis.Observation) (inc, cand []float64, items []string) {
	r := analysis.Request{Observations: obs}
	return r.Values("task-completion")
}

func mean(xs []float64) float64 {
	s := 0.0
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

func spread(xs []float64) float64 {
	m := mean(xs)
	v := 0.0
	for _, x := range xs {
		v += (x - m) * (x - m)
	}
	return sqrt(v / float64(len(xs)))
}

func sqrt(v float64) float64 {
	if v <= 0 {
		return 0
	}
	x := v
	for i := 0; i < 40; i++ {
		x = (x + v/x) / 2
	}
	return x
}

func absF(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
