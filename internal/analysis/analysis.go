// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

// Package analysis decides whether a candidate may be promoted.
//
// The decision is separated from the controller behind one interface, and that
// separation is load-bearing rather than tidy. Statistical practice for
// non-deterministic systems is not settled: a team with a statistician will
// want to bring their own test, and an ecosystem of analyzers is worth more
// than a single opinionated one. The interface is public and stable so that
// third parties can implement it.
//
// This package ships two analyzers that are complete and correct within their
// stated assumptions: a threshold test for guardrails, and a paired bootstrap
// for the primary metric.
package analysis

import (
	"context"
	"fmt"
)

// Test names a statistical procedure.
type Test string

const (
	// TestThreshold compares a value against a fixed bound. Right for a
	// guardrail like "zero policy violations", wrong for anything noisy.
	TestThreshold Test = "threshold"

	// TestSequential is an anytime-valid confidence sequence, for a canary
	// that is watched while it runs.
	//
	// A fixed-horizon test is invalid under continuous peeking: look often
	// enough and a good candidate will eventually look bad. This one holds at
	// every time step at once, so a decision can be taken the moment the
	// evidence is sufficient.
	TestSequential Test = "sequential"

	// TestPairedBootstrap resamples the per-item differences between arms.
	//
	// Paired because both arms see the same inputs, which removes item-level
	// variance and cuts the sample size needed by roughly an order of
	// magnitude against an unpaired test. Bootstrap because it assumes nothing
	// about the distribution, which matters for judge scores bounded in [0,1]
	// where a t-test's normality assumption is plainly false.
	TestPairedBootstrap Test = "paired-bootstrap"
)

// Direction says which way a metric is good.
type Direction string

const (
	// Higher means a larger value is better: task completion, for instance.
	Higher Direction = "higher-is-better"
	// Lower means a smaller value is better: cost, latency, escalation rate.
	Lower Direction = "lower-is-better"
)

// Metric describes one thing being tested.
type Metric struct {
	// Name is the metric as the scorer reports it. The vocabulary is the
	// operator's, not ours.
	Name string `json:"name"`
	Test Test   `json:"test"`

	// Margin is the non-inferiority bound.
	//
	// You almost never need "the candidate is better". You need "the candidate
	// is not worse by more than δ". A margin of -0.02 on a
	// higher-is-better metric means a two-point drop is tolerable and anything
	// worse is not. The value is the operator's judgement about their own
	// product and never a default of ours.
	Margin float64 `json:"margin"`

	// Threshold is the bound for TestThreshold.
	Threshold float64 `json:"threshold,omitempty"`

	// Alpha is the false-positive rate for this test.
	Alpha float64 `json:"alpha,omitempty"`

	Direction Direction `json:"direction,omitempty"`
}

// Design says whether the two arms saw the same inputs.
//
// This is not a preference, it is a property of how the stage runs, and it
// changes what a valid comparison looks like. Getting it wrong understates the
// uncertainty and promotes on evidence that is not there.
type Design string

const (
	// DesignPaired: both arms saw the same input, so the comparison can be made
	// per item and item difficulty cancels out. True of offline replay and of
	// shadow, where the same request reaches both.
	DesignPaired Design = "paired"

	// DesignUnpaired: each observation belongs to one arm only.
	//
	// True of a live canary: a request is served by the incumbent or the
	// candidate, never both. Nothing cancels, so the same conclusion needs far
	// more traffic — which is the real reason to get as far as possible with
	// replay and shadow before routing a single user to a candidate.
	DesignUnpaired Design = "unpaired"
)

// Request is what an analyzer is given.
//
// Deliberately plain: an implementation in another language behind the gRPC
// seam receives exactly this, so it carries no Go-specific shapes and nothing
// an implementer would have to guess at.
type Request struct {
	// Agent and the two manifest identities under comparison.
	Agent     string `json:"agent"`
	Incumbent string `json:"incumbent"`
	Candidate string `json:"candidate"`

	// Primary is the metric the promotion decision turns on. Exactly one.
	//
	// Gating eight metrics at alpha 0.05 each gives roughly a 34% chance of a
	// spurious rollback. One primary metric carries the full alpha; everything
	// else is a guardrail under a stricter correction.
	Primary Metric `json:"primary"`

	// Guardrails must all hold. They are one-sided and corrected for
	// multiplicity.
	Guardrails []Metric `json:"guardrails,omitempty"`

	// Observations are the paired measurements, one entry per corpus item.
	Observations []Observation `json:"observations"`

	// Missing describes which items failed to score and under which arm.
	//
	// Carried into the analyzer rather than filtered out beforehand, because
	// the pattern of what went missing is itself evidence. Dropping unscored
	// items is unbiased only when the missingness is random, and a candidate
	// that makes the judge fail more often would otherwise be promoted on the
	// subset where it behaved.
	Missing Missingness `json:"missing"`

	// MissingPolicy bounds how much and how unevenly scoring may fail.
	MissingPolicy MissingnessPolicy `json:"missingPolicy,omitempty"`

	Reasons []string `json:"reasons,omitempty"`

	// Design says whether the arms saw the same inputs. Empty means paired.
	Design Design `json:"design,omitempty"`

	// Seed makes a bootstrap reproducible. A promotion decision that cannot be
	// recomputed from its inputs is not evidence.
	Seed int64 `json:"seed,omitempty"`
}

// Observation is one corpus item measured under both arms.
type Observation struct {
	Item      string             `json:"item"`
	Incumbent map[string]float64 `json:"incumbent"`
	Candidate map[string]float64 `json:"candidate"`
}

// Outcome is the decision.
type Outcome string

const (
	// OutcomePromote: the evidence supports promotion.
	OutcomePromote Outcome = "promote"
	// OutcomeWaveOff: the evidence does not. The candidate goes around again.
	//
	// A failed gate waves off a candidate. It does not "fail" or "abort" it:
	// the wave-off is the decision, the rollback is the implementation, and
	// keeping the two words apart keeps the API honest about which is which.
	OutcomeWaveOff Outcome = "wave-off"
	// OutcomeInconclusive: not enough evidence either way.
	//
	// Distinct from a wave-off on purpose. "We cannot tell" is not "it is
	// worse", and collapsing them either blocks good releases or, worse,
	// promotes on the strength of a test that never ran.
	OutcomeInconclusive Outcome = "inconclusive"
)

// MetricResult is the finding for one metric.
type MetricResult struct {
	Name string `json:"name"`
	Test Test   `json:"test"`
	Pass bool   `json:"pass"`

	// IncumbentMean and CandidateMean are the observed values.
	IncumbentMean float64 `json:"incumbentMean"`
	CandidateMean float64 `json:"candidateMean"`
	Delta         float64 `json:"delta"`

	// CILower and CIUpper bound the difference. A non-inferiority test passes
	// when the whole interval sits on the acceptable side of the margin.
	CILower float64 `json:"ciLower,omitempty"`
	CIUpper float64 `json:"ciUpper,omitempty"`

	// Inferior means the interval sits entirely on the wrong side of the
	// margin: the candidate is genuinely worse, not merely unproven.
	Inferior bool `json:"inferior,omitempty"`

	// Indeterminate means the interval straddles the margin, so this metric
	// cannot separate acceptable from worse whatever the point estimate says.
	//
	// Kept distinct from a failure on purpose. "We cannot tell" is not "it is
	// worse", and — more importantly for a guardrail — it is not "it is fine"
	// either.
	Indeterminate bool `json:"indeterminate,omitempty"`

	// Alpha and N are what the result was computed at and over.
	//
	// Alpha is the two-sided level of the interval. The non-inferiority test
	// reads one end of it, so an Alpha of 0.05 is a one-sided test at 0.025 —
	// the usual convention, and the more conservative reading of the two.
	Alpha float64 `json:"alpha,omitempty"`
	N     int     `json:"n"`

	Detail string `json:"detail,omitempty"`
}

// Verdict is what an analyzer returns.
type Verdict struct {
	Outcome Outcome `json:"outcome"`
	// Reason is written for whoever is woken up by it.
	Reason string `json:"reason"`

	Primary    MetricResult   `json:"primary"`
	Guardrails []MetricResult `json:"guardrails,omitempty"`

	// N is how many paired items the decision rests on.
	N int `json:"n"`

	// Missing is the drop pattern, and MissingCheck the verdict on it.
	Missing      Missingness      `json:"missing"`
	MissingCheck MissingnessCheck `json:"missingCheck"`

	// Seed and Observations are what makes this recomputable.
	//
	// A fixed seed only makes the resampling deterministic given the scores,
	// and the scores come from a judge that is not. Carrying the inputs means
	// the analysis can be rerun without invoking the judge again, which is the
	// difference between a decision that can be audited and one that has to be
	// taken on trust. Recording the seed here rather than only fixing it in
	// code makes a future change to seeding visible in the evidence.
	Seed         int64         `json:"seed"`
	Observations []Observation `json:"observations,omitempty"`

	// Analyzer identifies what produced this, so a decision can be traced back
	// to the thing that made it.
	Analyzer string `json:"analyzer"`
}

// Analyzer turns paired measurements into a promotion decision.
//
// An implementation must never return OutcomePromote when it could not
// complete: an error or OutcomeInconclusive is always the safer answer, because
// a controller that reads silence as approval promotes on no evidence.
type Analyzer interface {
	Analyze(ctx context.Context, req Request) (Verdict, error)
	// Name identifies the analyzer in a verdict and in logs.
	Name() string
}

// Validate checks a request before any test runs.
func (r Request) Validate() error {
	if r.Primary.Name == "" {
		return fmt.Errorf("no primary metric: a gate needs exactly one metric to decide on")
	}
	if len(r.Observations) == 0 {
		return fmt.Errorf("no observations: there is nothing to compare")
	}
	if r.Design == DesignUnpaired {
		// Each observation belongs to one arm, so an item appearing twice is
		// ordinary rather than a mistake, and the paired checks below do not
		// apply.
		return nil
	}

	// Duplicate items would be resampled as if they were independent.
	//
	// They are not: repeated runs of the same corpus item share everything
	// about that item, and treating them as independent understates variance,
	// narrows the interval and over-promotes. Repeated measurement needs a
	// clustered bootstrap — resample items, then repeats within item — which
	// this analyzer does not implement, so the shape that would silently break
	// it is refused rather than accepted.
	seen := make(map[string]struct{}, len(r.Observations))
	for _, o := range r.Observations {
		if _, dup := seen[o.Item]; dup {
			return fmt.Errorf("item %q appears more than once: repeated measurements of one item are not "+
				"independent, and this analyzer resamples at the item level only", o.Item)
		}
		seen[o.Item] = struct{}{}
	}
	if r.Primary.Alpha < 0 || r.Primary.Alpha >= 1 {
		return fmt.Errorf("primary alpha is %v, which is not a probability", r.Primary.Alpha)
	}
	for _, g := range r.Guardrails {
		if g.Name == "" {
			return fmt.Errorf("a guardrail has no metric name")
		}
	}
	return nil
}

// Arms extracts each arm's measurements independently, for an unpaired design.
func (r Request) Arms(metric string) (incumbent, candidate []float64) {
	for _, o := range r.Observations {
		if v, ok := o.Incumbent[metric]; ok {
			incumbent = append(incumbent, v)
		}
		if v, ok := o.Candidate[metric]; ok {
			candidate = append(candidate, v)
		}
	}
	return incumbent, candidate
}

// Values extracts one metric's paired measurements, dropping items where either
// arm is missing it.
//
// Dropped rather than imputed. Substituting a mean or a zero for a missing arm
// invents data at exactly the point a paired test is most sensitive to it.
func (r Request) Values(metric string) (incumbent, candidate []float64, items []string) {
	for _, o := range r.Observations {
		a, okA := o.Incumbent[metric]
		b, okB := o.Candidate[metric]
		if !okA || !okB {
			continue
		}
		incumbent = append(incumbent, a)
		candidate = append(candidate, b)
		items = append(items, o.Item)
	}
	return incumbent, candidate, items
}

// ParseTest validates a test name.
func ParseTest(s string) (Test, error) {
	switch Test(s) {
	case TestThreshold:
		return TestThreshold, nil
	case TestPairedBootstrap:
		return TestPairedBootstrap, nil
	case TestSequential:
		return TestSequential, nil
	}
	return "", fmt.Errorf("unknown test %q: use %s, %s or %s",
		s, TestThreshold, TestPairedBootstrap, TestSequential)
}

// seedOrDefault returns the request's seed, or a fixed one when unset.
func (r Request) seedOrDefault() int64 {
	if r.Seed == 0 {
		return 1
	}
	return r.Seed
}
