// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package analysis

import (
	"context"
	"fmt"
	"math"
)

// Sequential decides a live canary while it is still running.
//
// A canary is watched continuously: an operator, or a controller, looks at the
// numbers every few seconds and stops the moment they look bad. A fixed-horizon
// interval — the paired bootstrap, a t-test, anything with a fixed n — is not
// valid under that. Its guarantee is "if you look once, at the end, you are
// wrong at most alpha of the time". Look a thousand times and take the worst
// one, and the real error rate is far higher: with enough peeking you can
// always find a moment where a perfectly good candidate looks like a
// regression.
//
// This is a confidence sequence instead: an interval that holds *simultaneously
// at every time step*, so stopping whenever the evidence is sufficient costs
// nothing. That is what makes it possible to end a canary in forty minutes
// rather than six hours without inflating the false-rollback rate.
//
// # What this implements
//
// A predictable-plug-in empirical-Bernstein confidence sequence for the mean of
// a bounded paired difference. The width follows the variance actually
// observed rather than the worst case the bounds permit, which matters more
// than it sounds: pairing makes the two arms track each other closely, so the
// real variance of a difference is small and a worst-case bound throws that
// entire advantage away.
// There is deliberately no tuning parameter. A mixture-boundary sequence needs
// one — a guess at the eventual sample size, fixed in advance, costing width
// wherever the guess is wrong. This one adapts to the variance it observes
// instead, so there is nothing for an operator to get wrong and nothing that
// could be quietly re-tuned after seeing the data.
type Sequential struct {
	// Lower and Upper are the range a single paired difference can take.
	// Scores in [0,1] give a difference in [-1,1], which is the default.
	Lower, Upper float64
}

var _ Analyzer = (*Sequential)(nil)

// Name implements Analyzer.
func (s *Sequential) Name() string { return "sequential-confidence-sequence" }

// Analyze runs the primary metric and every guardrail as anytime-valid
// non-inferiority tests.
func (s *Sequential) Analyze(ctx context.Context, req Request) (Verdict, error) {
	if err := req.Validate(); err != nil {
		return Verdict{}, err
	}

	verdict := Verdict{
		Analyzer:     s.Name(),
		Missing:      req.Missing,
		Seed:         req.seedOrDefault(),
		Observations: req.Observations,
	}

	primary, err := s.metric(ctx, req, req.Primary)
	if err != nil {
		return Verdict{}, err
	}
	verdict.Primary = primary
	verdict.N = primary.N

	// Full alpha on every guardrail, for the same reason as the fixed-sample
	// analyzer: these are non-inferiority tests, promotion requires rejecting
	// all of them, and the size of an intersection-union test is bounded by
	// the largest of its parts.
	for _, g := range req.Guardrails {
		if g.Alpha == 0 {
			g.Alpha = req.Primary.Alpha
			if g.Alpha == 0 {
				g.Alpha = DefaultAlpha
			}
		}
		result, err := s.metric(ctx, req, g)
		if err != nil {
			return Verdict{}, err
		}
		verdict.Guardrails = append(verdict.Guardrails, result)
	}

	alpha := DefaultAlpha
	if req.Primary.Alpha > 0 {
		alpha = req.Primary.Alpha
	}
	verdict.MissingCheck = CheckMissingness(req.Missing, req.MissingPolicy, alpha)

	return decideSequential(verdict), nil
}

func (s *Sequential) metric(ctx context.Context, req Request, m Metric) (MetricResult, error) {
	unpaired := req.Design == DesignUnpaired

	var incumbent, candidate []float64
	if unpaired {
		incumbent, candidate = req.Arms(m.Name)
	} else {
		incumbent, candidate, _ = req.Values(m.Name)
	}

	n := len(incumbent)
	if unpaired && len(candidate) < n {
		n = len(candidate)
	}
	result := MetricResult{Name: m.Name, Test: m.Test, N: n, Alpha: m.Alpha}
	if result.Alpha == 0 {
		result.Alpha = DefaultAlpha
	}

	if len(incumbent) == 0 || len(candidate) == 0 {
		result.Detail = fmt.Sprintf("no observation for %q under both arms", m.Name)
		return result, nil
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}

	result.IncumbentMean = mean(incumbent)
	result.CandidateMean = mean(candidate)
	result.Delta = result.CandidateMean - result.IncumbentMean

	if m.Test == TestThreshold {
		return threshold(result, m, candidate), nil
	}

	if unpaired {
		// Two independent sequences, each at half the level, combined by their
		// worst corners. Conservative, and correct without assuming anything
		// about how the two samples relate — which is the whole point, since in
		// a live canary they relate only through whoever happened to be routed
		// where.
		arm := &Sequential{Lower: 0, Upper: 1}
		if s.Upper > s.Lower {
			arm = &Sequential{Lower: s.Lower, Upper: s.Upper}
		}
		incLo, incHi := arm.interval(incumbent, result.Alpha/2)
		candLo, candHi := arm.interval(candidate, result.Alpha/2)
		result.CILower = candLo - incHi
		result.CIUpper = candHi - incLo
	} else {
		diffs := make([]float64, len(incumbent))
		for i := range incumbent {
			diffs[i] = candidate[i] - incumbent[i]
		}
		result.CILower, result.CIUpper = s.interval(diffs, result.Alpha)
	}

	// The same three-way reading as the fixed-sample test: entirely acceptable,
	// entirely worse, or straddling the margin and therefore undecided. Here
	// "undecided" is the normal state early in a canary rather than a problem,
	// and it is what tells the controller to keep watching instead of deciding.
	if m.Direction == Lower {
		result.Pass = result.CIUpper < m.Margin
		result.Inferior = result.CILower > m.Margin
	} else {
		result.Pass = result.CILower > m.Margin
		result.Inferior = result.CIUpper < m.Margin
	}
	result.Indeterminate = !result.Pass && !result.Inferior

	design := "paired"
	if unpaired {
		design = "unpaired"
	}
	result.Detail = fmt.Sprintf(
		"candidate is %+.4f against the incumbent after %d %s observation(s); "+
			"the anytime-valid interval is [%+.4f, %+.4f] and the margin allows %+.4f",
		result.Delta, result.N, design, result.CILower, result.CIUpper, m.Margin)
	return result, nil
}

// interval computes the empirical-Bernstein confidence sequence.
//
// A sub-Gaussian bound would be simpler, and it is what an earlier version of
// this did. It was correct and useless: it assumes the worst-case variance the
// bounds allow, so on a metric whose two arms track each other closely — which
// is exactly what pairing produces — the interval stayed far wider than the
// data warranted. Resolving a two-point margin needed tens of thousands of
// requests, which is not a canary, it is a rollout.
//
// This is the predictable-plug-in empirical-Bernstein sequence (Waudby-Smith
// and Ramdas): it bets on each observation with a stake chosen from the
// variance seen *so far*, never from the observation it is about to weigh. That
// predictability is what keeps it anytime-valid while letting the width follow
// the real variance instead of the worst imaginable one.
func (s *Sequential) interval(diffs []float64, alpha float64) (lower, upper float64) {
	t := len(diffs)
	if t == 0 {
		return math.Inf(-1), math.Inf(1)
	}
	lo, hi := s.bounds()
	width := hi - lo
	if width <= 0 {
		return math.Inf(-1), math.Inf(1)
	}

	// A two-sided interval at alpha, so reading one end is a one-sided test at
	// alpha/2 — the same convention as the fixed-sample analyzer.
	logInv := math.Log(2 / alpha)

	// Running estimates, each used only for the *next* observation.
	muHat, varHat := 0.5, 0.25
	sumZ, sumSq := 0.0, 0.0

	var sumLambdaZ, sumLambda, penalty float64
	for i, d := range diffs {
		z := (d - lo) / width
		n := float64(i + 1)

		// The stake. Larger where the data has been consistent, smaller where
		// it has been noisy, and capped strictly below 1 so the log below
		// stays finite.
		lambda := math.Sqrt(2 * logInv / (varHat * n * math.Log(1+n)))
		if capped := 0.5 / width; lambda > capped {
			lambda = capped
		}
		if lambda > 0.9 {
			lambda = 0.9
		}
		if lambda <= 0 || math.IsNaN(lambda) {
			lambda = 1e-6
		}

		// The variance term is measured against the estimate from before this
		// observation, which is what makes the stake predictable.
		v := 4 * (z - muHat) * (z - muHat)
		penalty += v * psiE(lambda)

		sumLambdaZ += lambda * z
		sumLambda += lambda

		// Update the running estimates for the next step only.
		sumZ += z
		muNext := (0.5 + sumZ) / (n + 1)
		sumSq += (z - muHat) * (z - muHat)
		varHat = (0.25 + sumSq) / (n + 1)
		muHat = muNext
	}

	if sumLambda <= 0 {
		return math.Inf(-1), math.Inf(1)
	}
	center := sumLambdaZ / sumLambda
	radius := (logInv + penalty) / sumLambda

	// Back out of the [0,1] rescaling.
	return lo + (center-radius)*width, lo + (center+radius)*width
}

// psiE is the empirical-Bernstein cumulant bound.
func psiE(lambda float64) float64 {
	if lambda <= 0 || lambda >= 1 {
		return math.Inf(1)
	}
	return (-math.Log(1-lambda) - lambda) / 4
}

// bounds are the range a single paired difference can take.
func (s *Sequential) bounds() (lower, upper float64) {
	if s.Lower == 0 && s.Upper == 0 {
		// Two scores in [0,1] give a difference in [-1,1].
		return -1, 1
	}
	return s.Lower, s.Upper
}

// decideSequential turns anytime-valid results into an outcome.
//
// The difference from the fixed-sample gate is what "undecided" means. There it
// is a problem — the corpus was too small or too noisy to answer. Here it is
// the normal state of a canary that has not seen enough traffic yet, and the
// right response is to keep watching, not to stop.
func decideSequential(v Verdict) Verdict {
	if v.Missing.Attempted > 0 && !v.MissingCheck.Pass {
		v.Outcome = OutcomeInconclusive
		v.Reason = v.MissingCheck.Detail
		return v
	}

	for _, g := range v.Guardrails {
		if g.Inferior {
			v.Outcome = OutcomeWaveOff
			v.Reason = fmt.Sprintf("guardrail %s was breached: %s", g.Name, g.Detail)
			return v
		}
	}

	if v.Primary.Inferior {
		// Enough evidence to stop now. This is the case a confidence sequence
		// exists for: a real regression can be acted on the moment it is
		// established, without waiting out a planned horizon.
		v.Outcome = OutcomeWaveOff
		v.Reason = fmt.Sprintf("%s is worse than the margin allows: %s", v.Primary.Name, v.Primary.Detail)
		return v
	}

	if v.Primary.Indeterminate {
		v.Outcome = OutcomeInconclusive
		v.Reason = fmt.Sprintf("%s has not accumulated enough evidence yet: %s",
			v.Primary.Name, v.Primary.Detail)
		return v
	}

	for _, g := range v.Guardrails {
		if !g.Pass {
			v.Outcome = OutcomeInconclusive
			v.Reason = fmt.Sprintf("guardrail %s has not been established yet: %s", g.Name, g.Detail)
			return v
		}
	}

	v.Outcome = OutcomePromote
	v.Reason = fmt.Sprintf("%s is non-inferior: %s", v.Primary.Name, v.Primary.Detail)
	return v
}
