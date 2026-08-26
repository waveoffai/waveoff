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

// stream builds a request from the first n observations of a generated canary.
func stream(obs []analysis.Observation, n int, margin float64) analysis.Request {
	return analysis.Request{
		Primary: analysis.Metric{
			Name: "task-completion", Test: analysis.TestSequential,
			Margin: margin, Alpha: 0.05, Direction: analysis.Higher,
		},
		Observations: obs[:n],
	}
}

// canary generates paired observations arriving one at a time.
func canary(n int, effect float64, seed int64) []analysis.Observation {
	rng := rand.New(rand.NewSource(seed))
	out := make([]analysis.Observation, n)
	for i := range out {
		base := rng.Float64()
		inc := clamp(base + rng.NormFloat64()*0.1)
		cand := clamp(base + effect + rng.NormFloat64()*0.1)
		out[i] = analysis.Observation{
			Item:      fmt.Sprintf("req-%05d", i),
			Incumbent: map[string]float64{"task-completion": inc},
			Candidate: map[string]float64{"task-completion": cand},
		}
	}
	return out
}

func sequential() *analysis.Sequential {
	return &analysis.Sequential{}
}

// TestPeekingBreaksTheFixedSampleTestAndNotTheSequenceIsTheWholePoint.
//
// This is the claim §7 rests on, checked rather than asserted. A canary is
// looked at continuously, and a fixed-horizon interval's guarantee only covers
// looking once. Run many null canaries, peek at every step, and count how often
// each method ever calls a perfectly good candidate a regression.
//
// The fixed-sample interval should fail far more often than its nominal alpha.
// The confidence sequence should not, because its guarantee holds at every time
// step simultaneously.
func TestPeekingBreaksTheFixedSampleTestAndNotTheSequence(t *testing.T) {
	if testing.Short() {
		t.Skip("simulation is slow under -short")
	}

	const (
		runs  = 200
		steps = 300
		alpha = 0.05
	)

	seq := sequential()
	fixed := &analysis.PairedBootstrap{Resamples: 400}
	ctx := context.Background()

	var seqFalse, fixedFalse int
	for run := 0; run < runs; run++ {
		// A null canary: the candidate is identical to the incumbent.
		obs := canary(steps, 0.0, int64(run)+1000)

		seqTripped, fixedTripped := false, false
		// Peek from a point where both tests are willing to answer.
		for n := 40; n <= steps; n += 10 {
			// A margin of zero makes this a pure test of whether the interval
			// ever excludes the truth, which is exactly the coverage question.
			req := stream(obs, n, 0)

			if !seqTripped {
				v, err := seq.Analyze(ctx, req)
				if err != nil {
					t.Fatal(err)
				}
				if v.Primary.Inferior {
					seqTripped = true
				}
			}
			if !fixedTripped {
				req.Primary.Test = analysis.TestPairedBootstrap
				req.Seed = int64(run)
				v, err := fixed.Analyze(ctx, req)
				if err != nil {
					t.Fatal(err)
				}
				if v.Primary.Inferior {
					fixedTripped = true
				}
			}
		}
		if seqTripped {
			seqFalse++
		}
		if fixedTripped {
			fixedFalse++
		}
	}

	seqRate := float64(seqFalse) / runs
	fixedRate := float64(fixedFalse) / runs
	t.Logf("over %d null canaries peeked at ~27 times each:", runs)
	t.Logf("  fixed-sample bootstrap called a good candidate a regression %.1f%% of the time", fixedRate*100)
	t.Logf("  confidence sequence did so %.1f%% of the time (nominal alpha %.0f%%)", seqRate*100, alpha*100)

	// The confidence sequence must hold its nominal rate under peeking. A
	// little slack for simulation noise, but nowhere near the inflation a
	// fixed-horizon test shows.
	if seqRate > 3*alpha {
		t.Errorf("the confidence sequence lost coverage under peeking: %.1f%% against a nominal %.0f%%",
			seqRate*100, alpha*100)
	}
	// And the demonstration that the problem is real: the fixed-sample test
	// must be visibly worse, or there would be no reason for this analyzer to
	// exist.
	if fixedRate <= seqRate {
		t.Errorf("the fixed-sample test did not inflate under peeking (%.1f%% vs %.1f%%); "+
			"either the simulation is not peeking hard enough or the sequence is not tighter",
			fixedRate*100, seqRate*100)
	}
}

// TestASequenceStopsEarlyOnARealRegression is the other half of the trade: an
// anytime-valid interval is wider at any given n, and it buys the ability to
// act the moment the evidence is there rather than at a planned horizon.
func TestASequenceStopsEarlyOnARealRegression(t *testing.T) {
	obs := canary(2000, -0.25, 7)
	seq := sequential()

	stopped := 0
	for n := 20; n <= 2000; n += 10 {
		v, err := seq.Analyze(context.Background(), stream(obs, n, -0.02))
		if err != nil {
			t.Fatal(err)
		}
		if v.Outcome == analysis.OutcomeWaveOff {
			stopped = n
			break
		}
	}
	if stopped == 0 {
		t.Fatal("a 25-point regression was never established")
	}
	t.Logf("waved off after %d observations", stopped)
	if stopped > 600 {
		t.Errorf("took %d observations to establish a 25-point regression; too slow to be useful", stopped)
	}
}

// TestAnUndecidedCanaryKeepsWatching. Early in a canary there is not enough
// evidence either way, and that is the normal state rather than a fault: the
// controller should keep going, not decide.
func TestAnUndecidedCanaryKeepsWatching(t *testing.T) {
	obs := canary(2000, 0.0, 11)
	v, err := sequential().Analyze(context.Background(), stream(obs, 15, -0.02))
	if err != nil {
		t.Fatal(err)
	}
	if v.Outcome != analysis.OutcomeInconclusive {
		t.Errorf("a canary with 15 observations decided %s: %s", v.Outcome, v.Reason)
	}
	if !v.Primary.Indeterminate {
		t.Error("the metric should be indeterminate this early")
	}
}

// TestTheIntervalTightensWithEvidence: without this the sequence would never
// conclude anything.
func TestTheIntervalTightensWithEvidence(t *testing.T) {
	obs := canary(4000, 0.0, 13)
	seq := sequential()

	var previous float64
	for _, n := range []int{50, 200, 1000, 4000} {
		v, err := seq.Analyze(context.Background(), stream(obs, n, -0.02))
		if err != nil {
			t.Fatal(err)
		}
		width := v.Primary.CIUpper - v.Primary.CILower
		t.Logf("n=%-5d width=%.4f", n, width)
		if previous != 0 && width >= previous {
			t.Errorf("the interval did not tighten between %d observations and fewer: %.4f -> %.4f",
				n, previous, width)
		}
		previous = width
	}
}

// TestAnEquivalentCandidateEventuallyPromotes: a canary that runs long enough
// on a good candidate has to conclude, or nothing ever ships.
func TestAnEquivalentCandidateEventuallyPromotes(t *testing.T) {
	obs := canary(20000, 0.0, 17)
	seq := &analysis.Sequential{}

	v, err := seq.Analyze(context.Background(), stream(obs, 20000, -0.05))
	if err != nil {
		t.Fatal(err)
	}
	if v.Outcome != analysis.OutcomePromote {
		t.Errorf("an equivalent candidate never promoted after 20000 observations: %s — %s",
			v.Outcome, v.Reason)
	}
}

// TestLowerIsBetterFlipsForSequences too: escalation rate and cost are metrics
// where an increase is the regression.
func TestSequentialLowerIsBetter(t *testing.T) {
	// The candidate escalates more often.
	obs := canary(3000, 0.15, 19)
	req := stream(obs, 3000, 0.02)
	req.Primary.Direction = analysis.Lower

	v, err := sequential().Analyze(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if v.Outcome != analysis.OutcomeWaveOff {
		t.Errorf("a rise in a lower-is-better metric was not waved off: %s — %s", v.Outcome, v.Reason)
	}
}

// TestSequentialRefusesDuplicateItems for the same reason the fixed-sample one
// does: repeated observations of the same item are not independent.
func TestSequentialRefusesDuplicateItems(t *testing.T) {
	obs := canary(50, 0, 23)
	req := stream(obs, 50, -0.02)
	req.Observations = append(req.Observations, req.Observations[0])

	if _, err := sequential().Analyze(context.Background(), req); err == nil {
		t.Fatal("duplicate observations were accepted")
	}
}

// unpairedCanary generates observations where each belongs to one arm only,
// which is what a live canary produces: a request is served by the incumbent or
// the candidate, never both.
func unpairedCanary(n int, effect float64, seed int64) []analysis.Observation {
	rng := rand.New(rand.NewSource(seed))
	out := make([]analysis.Observation, 0, n)
	for i := 0; i < n; i++ {
		o := analysis.Observation{Item: fmt.Sprintf("req-%05d", i)}
		if i%2 == 0 {
			o.Incumbent = map[string]float64{"task-completion": clamp(0.5 + rng.NormFloat64()*0.15)}
		} else {
			o.Candidate = map[string]float64{"task-completion": clamp(0.5 + effect + rng.NormFloat64()*0.15)}
		}
		out = append(out, o)
	}
	return out
}

func unpairedRequest(obs []analysis.Observation, margin float64) analysis.Request {
	req := analysis.Request{
		Design: analysis.DesignUnpaired,
		Primary: analysis.Metric{
			Name: "task-completion", Test: analysis.TestSequential,
			Margin: margin, Alpha: 0.05, Direction: analysis.Higher,
		},
		Observations: obs,
	}
	return req
}

// TestAnUnpairedCanaryIsHandled: a live canary has no same-input-both-arms, so
// the paired difference does not exist and treating it as if it did would
// understate the uncertainty badly.
func TestAnUnpairedCanaryIsHandled(t *testing.T) {
	v, err := sequential().Analyze(context.Background(),
		unpairedRequest(unpairedCanary(8000, 0, 41), -0.05))
	if err != nil {
		t.Fatal(err)
	}
	if v.Outcome != analysis.OutcomePromote {
		t.Errorf("an equivalent unpaired canary never concluded: %s — %s", v.Outcome, v.Reason)
	}
	if !strings.Contains(v.Primary.Detail, "unpaired") {
		t.Errorf("the detail should say which design was used: %q", v.Primary.Detail)
	}
}

func TestAnUnpairedRegressionIsCaught(t *testing.T) {
	v, err := sequential().Analyze(context.Background(),
		unpairedRequest(unpairedCanary(8000, -0.25, 43), -0.02))
	if err != nil {
		t.Fatal(err)
	}
	if v.Outcome != analysis.OutcomeWaveOff {
		t.Errorf("an unpaired regression was not caught: %s — %s", v.Outcome, v.Reason)
	}
}

// TestUnpairedNeedsMoreEvidenceThanPaired is the quantitative reason to get as
// far as possible with replay and shadow before routing a single user to a
// candidate: nothing cancels in an unpaired comparison.
//
// Both designs are built from one generator, so the per-arm distribution is
// identical and the only difference is whether the two arms saw the same item.
func TestUnpairedNeedsMoreEvidenceThanPaired(t *testing.T) {
	const n = 600
	rng := rand.New(rand.NewSource(47))

	paired := make([]analysis.Observation, n)
	unpaired := make([]analysis.Observation, 0, 2*n)
	for i := 0; i < n; i++ {
		// Item difficulty, which varies a lot between requests. This is the
		// variance pairing removes and an unpaired comparison has to absorb.
		difficulty := rng.Float64()
		inc := clamp(difficulty + rng.NormFloat64()*0.02)
		cand := clamp(difficulty + rng.NormFloat64()*0.02)

		paired[i] = analysis.Observation{
			Item:      fmt.Sprintf("item-%05d", i),
			Incumbent: map[string]float64{"task-completion": inc},
			Candidate: map[string]float64{"task-completion": cand},
		}
		// The same values, split so no request reaches both arms.
		unpaired = append(unpaired,
			analysis.Observation{Item: fmt.Sprintf("i-%05d", i),
				Incumbent: map[string]float64{"task-completion": inc}},
			analysis.Observation{Item: fmt.Sprintf("c-%05d", i),
				Candidate: map[string]float64{"task-completion": cand}},
		)
	}

	pairedV, err := sequential().Analyze(context.Background(), stream(paired, n, -0.05))
	if err != nil {
		t.Fatal(err)
	}
	unpairedV, err := sequential().Analyze(context.Background(), unpairedRequest(unpaired, -0.05))
	if err != nil {
		t.Fatal(err)
	}

	pairedWidth := pairedV.Primary.CIUpper - pairedV.Primary.CILower
	unpairedWidth := unpairedV.Primary.CIUpper - unpairedV.Primary.CILower
	t.Logf("same data, %d items per arm: paired width %.4f, unpaired width %.4f (%.1fx)",
		n, pairedWidth, unpairedWidth, unpairedWidth/pairedWidth)

	if unpairedWidth <= pairedWidth {
		t.Errorf("the unpaired interval (%.4f) is no wider than the paired one (%.4f); "+
			"pairing is supposed to be removing item-level variance", unpairedWidth, pairedWidth)
	}
}

// TestUnpairedAllowsRepeatedItems: an item id appearing twice is ordinary when
// each observation belongs to one arm.
func TestUnpairedAllowsRepeatedItems(t *testing.T) {
	obs := unpairedCanary(400, 0, 49)
	obs = append(obs, obs[0])
	if _, err := sequential().Analyze(context.Background(), unpairedRequest(obs, -0.05)); err != nil {
		t.Fatalf("an unpaired request with a repeated item was refused: %v", err)
	}
}
