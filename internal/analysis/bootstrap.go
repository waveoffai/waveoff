// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package analysis

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"sort"
)

// DefaultResamples is how many bootstrap resamples are drawn.
//
// Enough that the interval bounds are stable to about three decimal places,
// which is finer than any margin an operator will set. Raising it buys
// precision nobody uses; lowering it makes the same input give different
// answers on different runs.
const DefaultResamples = 10000

// DefaultAlpha is the false-positive rate when none is given.
const DefaultAlpha = 0.05

// PairedBootstrap tests non-inferiority by resampling the per-item differences
// between two arms.
//
// Two choices matter here.
//
// Paired: both arms see the same corpus items, so the difference is taken per
// item before anything is aggregated. Item difficulty varies enormously between
// tasks and cancels out entirely in the difference, which is why this needs
// roughly an order of magnitude fewer items than comparing two independent
// means.
//
// Non-inferiority rather than superiority: the question at a release gate is
// almost never "is the candidate better?" but "is it not worse by more than I
// can accept?". Testing for superiority would block every release that is
// merely equivalent, which is most of them.
type PairedBootstrap struct {
	// Resamples defaults to DefaultResamples.
	Resamples int
	// Rand makes a run reproducible. Nil uses the request's seed.
	Rand *rand.Rand
}

var _ Analyzer = (*PairedBootstrap)(nil)

// Name implements Analyzer.
func (p *PairedBootstrap) Name() string { return "paired-bootstrap" }

// Analyze runs the primary metric and every guardrail.
func (p *PairedBootstrap) Analyze(ctx context.Context, req Request) (Verdict, error) {
	if err := req.Validate(); err != nil {
		return Verdict{}, err
	}

	rng := p.Rand
	if rng == nil {
		// An unseeded request gets a fixed seed rather than a random one, so
		// the resampling is reproducible. That alone is not enough — see
		// Verdict.Observations for the other half.
		rng = rand.New(rand.NewSource(req.seedOrDefault()))
	}

	seed := req.Seed
	if seed == 0 {
		seed = 1
	}
	verdict := Verdict{
		Analyzer: p.Name(),
		Missing:  req.Missing,
		Seed:     seed,
		// The exact inputs, so the decision can be recomputed without the
		// judge that produced them.
		Observations: req.Observations,
	}

	primary, err := p.metric(ctx, req, req.Primary, rng)
	if err != nil {
		return Verdict{}, err
	}
	verdict.Primary = primary
	verdict.N = primary.N

	// Guardrails run at full alpha, with no multiplicity correction.
	//
	// That is not an oversight, it falls out of the test structure. Every
	// metric here is a non-inferiority test: the null is "this is breached"
	// and the candidate has to prove otherwise. Promotion requires rejecting
	// the primary null *and* every guardrail null, which is an
	// intersection-union test, and the size of an intersection-union test is
	// bounded by the largest of its parts. Requiring all k rejections already
	// controls the false-promotion rate at alpha.
	//
	// Correcting on top of that would only shrink alpha, widen every interval,
	// and make guardrails harder to satisfy — costing power to prove
	// non-inferiority and waving off good releases, to buy error control the
	// structure already provides.
	//
	// The correction would be needed if guardrails were framed the other way
	// round, as detection tests with a null of "no harm". That framing also
	// makes a guardrail pass by default when it is underpowered, which is the
	// opposite of what a gate should do.
	guardAlpha := DefaultAlpha
	if req.Primary.Alpha > 0 {
		guardAlpha = req.Primary.Alpha
	}

	for _, g := range req.Guardrails {
		if g.Alpha == 0 {
			g.Alpha = guardAlpha
		}
		result, err := p.metric(ctx, req, g, rng)
		if err != nil {
			return Verdict{}, err
		}
		verdict.Guardrails = append(verdict.Guardrails, result)
	}

	// The drop pattern is checked before the numbers are believed. A result
	// computed over a biased subset is worse than no result, because it looks
	// exactly like a clean one.
	alpha := DefaultAlpha
	if req.Primary.Alpha > 0 {
		alpha = req.Primary.Alpha
	}
	verdict.MissingCheck = CheckMissingness(req.Missing, req.MissingPolicy, alpha)

	return decide(verdict, req), nil
}

// metric runs one test.
func (p *PairedBootstrap) metric(ctx context.Context, req Request, m Metric, rng *rand.Rand) (MetricResult, error) {
	incumbent, candidate, _ := req.Values(m.Name)
	result := MetricResult{Name: m.Name, Test: m.Test, N: len(incumbent), Alpha: m.Alpha}
	if result.Alpha == 0 {
		result.Alpha = DefaultAlpha
	}

	if len(incumbent) == 0 {
		// No measurement is not a pass. A gate that reads an unmeasured metric
		// as satisfied promotes on the strength of a test that never ran.
		result.Pass = false
		result.Detail = fmt.Sprintf("no item was scored for %q under both arms", m.Name)
		return result, nil
	}

	result.IncumbentMean = mean(incumbent)
	result.CandidateMean = mean(candidate)
	result.Delta = result.CandidateMean - result.IncumbentMean

	if m.Test == TestThreshold {
		return threshold(result, m, candidate), nil
	}

	// The per-item difference. This is where the pairing happens, and it is the
	// whole reason the sample size is manageable.
	diffs := make([]float64, len(incumbent))
	for i := range incumbent {
		diffs[i] = candidate[i] - incumbent[i]
	}

	lower, upper, err := bootstrapCI(ctx, diffs, result.Alpha, p.resamples(), rng)
	if err != nil {
		return result, err
	}
	result.CILower, result.CIUpper = lower, upper

	// Where the interval sits relative to the margin gives three answers, not
	// two, and the third one is the honest handling of an underpowered test.
	//
	//   entirely on the acceptable side  -> non-inferior
	//   entirely on the other side       -> genuinely worse
	//   straddling the margin            -> the data cannot tell
	//
	// This is why there is no minimum-sample constant to argue about. An
	// interval too wide to separate "acceptable" from "worse" produces
	// Inconclusive on its own, and it self-adjusts per metric and per
	// variance — which is exactly what the honest minimum depends on.
	if m.Direction == Lower {
		result.Pass = upper < m.Margin
		result.Inferior = lower > m.Margin
	} else {
		result.Pass = lower > m.Margin
		result.Inferior = upper < m.Margin
	}
	result.Indeterminate = !result.Pass && !result.Inferior

	bound, side := lower, "at worst"
	if m.Direction == Lower {
		bound, side = upper, "at worst"
	}
	result.Detail = fmt.Sprintf(
		"candidate is %+.4f against the incumbent; the %.0f%% interval reaches %s %+.4f, and the margin allows %+.4f",
		result.Delta, (1-result.Alpha)*100, side, bound, m.Margin)
	if result.Indeterminate {
		result.Detail += fmt.Sprintf(
			" — the interval [%+.4f, %+.4f] straddles the margin, so this cannot separate acceptable from worse",
			lower, upper)
	}
	return result, nil
}

func (p *PairedBootstrap) resamples() int {
	if p.Resamples > 0 {
		return p.Resamples
	}
	return DefaultResamples
}

// bootstrapCI returns a percentile confidence interval for the mean difference.
func bootstrapCI(ctx context.Context, diffs []float64, alpha float64, resamples int, rng *rand.Rand) (lower, upper float64, err error) {
	n := len(diffs)
	if n == 0 {
		return 0, 0, fmt.Errorf("no paired observations")
	}
	if n == 1 {
		// One item cannot support an interval. Returning a degenerate one
		// would look like enormous confidence in a single measurement.
		return math.Inf(-1), math.Inf(1), nil
	}

	means := make([]float64, resamples)
	for r := 0; r < resamples; r++ {
		if r%1024 == 0 {
			if err := ctx.Err(); err != nil {
				return 0, 0, err
			}
		}
		sum := 0.0
		for i := 0; i < n; i++ {
			sum += diffs[rng.Intn(n)]
		}
		means[r] = sum / float64(n)
	}
	sort.Float64s(means)

	// A two-sided percentile interval. The non-inferiority test reads only the
	// relevant end, but reporting both tells an operator whether the candidate
	// is merely non-inferior or actually better.
	return percentile(means, alpha/2), percentile(means, 1-alpha/2), nil
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return math.NaN()
	}
	idx := int(p * float64(len(sorted)))
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// threshold compares the candidate against a fixed bound.
func threshold(result MetricResult, m Metric, candidate []float64) MetricResult {
	worst := candidate[0]
	for _, v := range candidate {
		if m.Direction == Lower && v > worst {
			worst = v
		}
		if m.Direction != Lower && v < worst {
			worst = v
		}
	}
	if m.Direction == Lower {
		result.Pass = worst <= m.Threshold
		result.Detail = fmt.Sprintf("worst observed %.4f against a limit of %.4f", worst, m.Threshold)
	} else {
		result.Pass = worst >= m.Threshold
		result.Detail = fmt.Sprintf("worst observed %.4f against a floor of %.4f", worst, m.Threshold)
	}
	// A threshold test is a direct observation, not an inference: there is no
	// interval to straddle, so a failure is always a demonstrated breach.
	result.Inferior = !result.Pass
	return result
}

// decide turns metric results into an outcome.
func decide(v Verdict, req Request) Verdict {
	// Before anything else: if scoring failed too often, or too unevenly
	// between the arms, the surviving items are not a fair comparison and no
	// amount of arithmetic over them fixes that.
	if v.Missing.Attempted > 0 && !v.MissingCheck.Pass {
		v.Outcome = OutcomeInconclusive
		v.Reason = v.MissingCheck.Detail
		return v
	}

	// A guardrail breach is decisive regardless of the primary metric. That is
	// what a guardrail is for: no amount of improvement in task completion
	// buys a policy violation.
	//
	// Order matters between the two failure kinds. A demonstrated breach is
	// more informative than an undecidable one, so it is looked for first; an
	// undecidable guardrail only holds the rollout once nothing is definitely
	// broken.
	for _, g := range v.Guardrails {
		if g.Inferior {
			v.Outcome = OutcomeWaveOff
			v.Reason = fmt.Sprintf("guardrail %s was breached: %s", g.Name, g.Detail)
			return v
		}
	}

	// A guardrail that could not decide is not a guardrail that passed. An
	// underpowered guardrail is an unmeasured one wearing a p-value, and this
	// gate already refuses to read an unmeasured metric as a pass.
	for _, g := range v.Guardrails {
		if g.Indeterminate || !g.Pass {
			v.Outcome = OutcomeInconclusive
			v.Reason = fmt.Sprintf("guardrail %s could not be decided: %s", g.Name, g.Detail)
			return v
		}
	}

	if v.Primary.N == 0 {
		v.Outcome = OutcomeInconclusive
		v.Reason = fmt.Sprintf("nothing was measured for %s, so there is no evidence either way", v.Primary.Name)
		return v
	}
	if v.Primary.N < BootstrapFloor {
		// Not a power argument — the straddle test above handles power. This
		// is about the resampling itself: below a few dozen paired items a
		// percentile bootstrap is drawing from too few distinct values to give
		// an interval worth reading, however narrow it looks.
		v.Outcome = OutcomeInconclusive
		v.Reason = fmt.Sprintf(
			"only %d item(s) were scored under both arms; a percentile bootstrap below %d is resampling "+
				"too few distinct values for its interval to mean anything", v.Primary.N, BootstrapFloor)
		return v
	}

	if v.Primary.Indeterminate {
		v.Outcome = OutcomeInconclusive
		v.Reason = fmt.Sprintf("%s could not be decided: %s", v.Primary.Name, v.Primary.Detail)
		return v
	}
	if v.Primary.Pass {
		v.Outcome = OutcomePromote
		v.Reason = fmt.Sprintf("%s is non-inferior: %s", v.Primary.Name, v.Primary.Detail)
		return v
	}
	v.Outcome = OutcomeWaveOff
	v.Reason = fmt.Sprintf("%s is worse than the margin allows: %s", v.Primary.Name, v.Primary.Detail)
	return v
}

// BootstrapFloor is the fewest paired items the resampling itself is trusted
// on.
//
// This is not a power threshold. Power is handled by whether the interval
// straddles the margin, which adapts to the metric and its variance. This is
// about the percentile bootstrap's own validity: with a handful of items the
// resamples are drawn from a handful of distinct values, and the interval can
// look reassuringly narrow while having no resolution behind it.
const BootstrapFloor = 30

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sum := 0.0
	for _, x := range xs {
		sum += x
	}
	return sum / float64(len(xs))
}
