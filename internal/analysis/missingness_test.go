// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package analysis_test

import (
	"context"
	"strings"
	"testing"

	"github.com/waveoffai/waveoff/internal/analysis"
)

func missing(attempted, both, candOnly, incOnly, neither int) analysis.Missingness {
	return analysis.Missingness{
		Attempted: attempted, BothScored: both,
		CandidateOnlyFailed: candOnly, IncumbentOnlyFailed: incOnly, BothFailed: neither,
	}
}

func check(m analysis.Missingness) analysis.MissingnessCheck {
	return analysis.CheckMissingness(m, analysis.MissingnessPolicy{}, 0.05)
}

// TestSymmetricDropsAreAcceptable: scoring fails sometimes, and a few failures
// spread evenly across the arms is a smaller sample rather than a biased one.
func TestSymmetricDropsAreAcceptable(t *testing.T) {
	c := check(missing(100, 90, 4, 4, 2))
	if !c.Pass {
		t.Errorf("evenly spread failures were rejected: %s", c.Detail)
	}
}

// TestAsymmetricDropsAreCaught is the failure this whole check exists for.
//
// A candidate producing malformed or truncated output makes the judge fail more
// often on it. Those items get dropped, and the candidate is then promoted on
// the subset where it behaved — shipping exactly the regression the gate was
// supposed to catch, while looking like a clean pass.
func TestAsymmetricDropsAreCaught(t *testing.T) {
	// Fifteen items the incumbent scored and the candidate did not, against
	// two the other way.
	c := check(missing(100, 83, 15, 2, 0))
	if c.Pass {
		t.Fatalf("a 15-vs-2 asymmetry was accepted: %s", c.Detail)
	}
	if c.PValue >= 0.05 {
		t.Errorf("p = %v, expected significant", c.PValue)
	}
	// The explanation has to name which arm, or nobody can act on it.
	if !strings.Contains(c.Detail, "candidate") {
		t.Errorf("the detail should say which arm failed more: %q", c.Detail)
	}
}

// TestAsymmetryIsJudgedOnDiscordantPairsOnly. In paired data the items both
// arms scored, or neither did, say nothing about which arm is harder to judge.
func TestAsymmetryIsJudgedOnDiscordantPairsOnly(t *testing.T) {
	// The same discordant split, with wildly different concordant counts.
	// Both stay under the drop-rate ceiling so only the asymmetry test runs.
	few := check(missing(60, 51, 8, 1, 0))
	many := check(missing(600, 591, 8, 1, 0))
	if few.PValue != many.PValue {
		t.Errorf("the p-value moved with the concordant counts: %v vs %v", few.PValue, many.PValue)
	}
}

// TestTooManyDropsIsUnusableEvenWhenSymmetric: a fifth of the corpus failing is
// not a smaller sample of the same population, it is whatever the scorer could
// handle.
func TestTooManyDropsIsUnusableEvenWhenSymmetric(t *testing.T) {
	c := check(missing(100, 60, 20, 20, 0))
	if c.Pass {
		t.Fatalf("a 40%% drop rate was accepted because it was symmetric: %s", c.Detail)
	}
	if !strings.Contains(c.Detail, "population") {
		t.Errorf("detail = %q", c.Detail)
	}
}

func TestNoDropsPasses(t *testing.T) {
	c := check(missing(50, 50, 0, 0, 0))
	if !c.Pass {
		t.Errorf("a complete corpus was rejected: %s", c.Detail)
	}
	if c.PValue != 1 {
		t.Errorf("p = %v with no discordant pairs, want 1", c.PValue)
	}
}

// TestMissingnessBlocksPromotion is the end-to-end consequence: a biased subset
// must not produce a verdict, however good the numbers on it look.
func TestMissingnessBlocksPromotion(t *testing.T) {
	// The candidate looks excellent on the items that scored.
	req := request(noisyPair(60, 0.20, 40), -0.02)
	req.Missing = missing(100, 60, 30, 3, 7)

	v, err := (&analysis.PairedBootstrap{Resamples: 2000}).Analyze(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if v.Outcome == analysis.OutcomePromote {
		t.Fatalf("a candidate was promoted on a biased subset: %s", v.Reason)
	}
	if v.Outcome != analysis.OutcomeInconclusive {
		t.Errorf("outcome = %s, want inconclusive", v.Outcome)
	}
	// The verdict must carry the pattern, not just the conclusion.
	if v.Missing.CandidateOnlyFailed != 30 {
		t.Errorf("the verdict lost the drop pattern: %+v", v.Missing)
	}
}

// TestCleanMissingnessDoesNotBlock: the check must not fire on ordinary
// scoring flakiness, or every rollout holds and the gate gets switched off.
func TestCleanMissingnessDoesNotBlock(t *testing.T) {
	req := request(noisyPair(60, 0.0, 41), -0.02)
	req.Missing = missing(64, 60, 2, 2, 0)

	v, err := (&analysis.PairedBootstrap{Resamples: 2000}).Analyze(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if v.Outcome != analysis.OutcomePromote {
		t.Errorf("ordinary flakiness blocked a good release: %s — %s", v.Outcome, v.Reason)
	}
}

// TestDuplicateItemsAreRefused: repeated runs of one corpus item are not
// independent, and resampling them flat understates variance, narrows the
// interval and over-promotes. Until a clustered bootstrap exists, the shape
// that would silently break it is refused.
func TestDuplicateItemsAreRefused(t *testing.T) {
	req := request(noisyPair(60, 0, 42), -0.02)
	req.Observations = append(req.Observations, req.Observations[0])

	_, err := (&analysis.PairedBootstrap{}).Analyze(context.Background(), req)
	if err == nil {
		t.Fatal("repeated measurements of one item were accepted as independent")
	}
	if !strings.Contains(err.Error(), "not") {
		t.Errorf("err = %v", err)
	}
}

// TestVerdictCarriesItsInputs: a fixed seed makes the resampling reproducible
// given the scores, and the scores come from a judge that is not. Without the
// inputs the decision cannot be recomputed, only re-run.
func TestVerdictCarriesItsInputs(t *testing.T) {
	req := request(noisyPair(60, 0, 43), -0.02)
	v, err := (&analysis.PairedBootstrap{Resamples: 2000}).Analyze(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Observations) != len(req.Observations) {
		t.Errorf("the verdict carries %d observations, want %d", len(v.Observations), len(req.Observations))
	}
	if v.Seed == 0 {
		t.Error("the verdict does not record the seed it used")
	}
}
