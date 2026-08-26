// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package analysis

import (
	"fmt"
	"math"
)

// Missingness records which items failed to score, and under which arm.
//
// Dropping unscored items is correct against imputation and incomplete on its
// own. Dropping is only unbiased when the missingness is random, and here it
// very likely is not: scorers fail more on long, complex, edge-case sessions,
// and they may fail at different rates between the arms.
//
// The path that matters: a candidate that produces more malformed or truncated
// output makes the judge choke more often, those items are dropped, and the
// candidate is then promoted on the subset where it behaved. That is a silent
// route to shipping exactly the regression this gate exists to catch, and it
// looks like a clean pass from every angle unless somebody measures it.
type Missingness struct {
	// Attempted is how many items were put forward for scoring.
	Attempted int `json:"attempted"`

	// BothScored is the usable sample.
	BothScored int `json:"bothScored"`

	// CandidateOnlyFailed and IncumbentOnlyFailed are the discordant pairs:
	// items one arm scored and the other did not. These, not the totals, are
	// what carry evidence of asymmetry in paired data.
	CandidateOnlyFailed int `json:"candidateOnlyFailed"`
	IncumbentOnlyFailed int `json:"incumbentOnlyFailed"`

	// BothFailed is uninformative about asymmetry but counts against the
	// overall drop rate.
	BothFailed int `json:"bothFailed"`
}

// Dropped is how many items produced no paired measurement.
func (m Missingness) Dropped() int {
	return m.CandidateOnlyFailed + m.IncumbentOnlyFailed + m.BothFailed
}

// Rate is the proportion of attempted items that could not be paired.
func (m Missingness) Rate() float64 {
	if m.Attempted == 0 {
		return 0
	}
	return float64(m.Dropped()) / float64(m.Attempted)
}

// MissingnessPolicy bounds how much and how unevenly scoring may fail.
type MissingnessPolicy struct {
	// MaxRate is the largest share of items that may go unscored before the
	// result is treated as unusable. Zero uses DefaultMaxDropRate.
	MaxRate float64 `json:"maxRate,omitempty"`

	// Alpha is the significance level for the asymmetry test. Zero uses the
	// primary metric's alpha.
	Alpha float64 `json:"alpha,omitempty"`
}

// DefaultMaxDropRate is the share of unscored items above which a result is
// treated as unusable rather than thin.
//
// A fifth of the corpus failing to score is not a smaller sample of the same
// population; it is a different population, selected by whatever made the
// scorer fail.
const DefaultMaxDropRate = 0.20

// MissingnessCheck is the outcome of examining the drop pattern.
type MissingnessCheck struct {
	Pass   bool    `json:"pass"`
	Rate   float64 `json:"rate"`
	PValue float64 `json:"pValue,omitempty"`
	Detail string  `json:"detail"`
}

// CheckMissingness tests whether scoring failed too often, or too unevenly
// between the arms.
//
// The asymmetry test is McNemar's, exact. Paired data makes the concordant
// items — scored by both arms or by neither — uninformative about which arm is
// harder to score; all the evidence is in the discordant pairs. Under the null
// that both arms fail equally often, the split between them is a fair coin, so
// the test is an exact binomial on the discordant counts.
func CheckMissingness(m Missingness, policy MissingnessPolicy, fallbackAlpha float64) MissingnessCheck {
	maxRate := policy.MaxRate
	if maxRate <= 0 {
		maxRate = DefaultMaxDropRate
	}
	alpha := policy.Alpha
	if alpha <= 0 {
		alpha = fallbackAlpha
	}
	if alpha <= 0 {
		alpha = DefaultAlpha
	}

	check := MissingnessCheck{Rate: m.Rate()}

	if m.Attempted == 0 {
		check.Detail = "nothing was attempted"
		return check
	}

	// Computed before either verdict, so the field always means what it says.
	// Leaving it at zero when an earlier check fires would read as "extremely
	// significant asymmetry" rather than "not computed".
	check.PValue = mcNemarExact(m.CandidateOnlyFailed, m.IncumbentOnlyFailed)

	if check.Rate > maxRate {
		check.Detail = fmt.Sprintf(
			"%d of %d item(s) could not be scored (%.0f%%, over the %.0f%% limit); "+
				"what remains is not a smaller sample of the same population, it is whatever the scorer could handle",
			m.Dropped(), m.Attempted, check.Rate*100, maxRate*100)
		return check
	}

	b, c := m.CandidateOnlyFailed, m.IncumbentOnlyFailed
	if check.PValue < alpha {
		worse, other, label := b, c, "candidate"
		if c > b {
			worse, other, label = c, b, "incumbent"
		}
		check.Detail = fmt.Sprintf(
			"scoring failed on %d item(s) for the %s and %d for the other arm (p=%.4f). "+
				"That asymmetry means the surviving items are not a fair comparison: "+
				"the %s is being judged on the subset it handled",
			worse, label, other, check.PValue, label)
		return check
	}

	check.Pass = true
	check.Detail = fmt.Sprintf("%d of %d item(s) scored under both arms; drop rate %.1f%%, no material asymmetry (p=%.3f)",
		m.BothScored, m.Attempted, check.Rate*100, check.PValue)
	return check
}

// mcNemarExact returns the two-sided exact p-value for the discordant counts.
//
// Under the null of equal failure rates the discordant pairs split like a fair
// coin, so this is a two-sided exact binomial test on b out of b+c at p=0.5.
func mcNemarExact(b, c int) float64 {
	n := b + c
	if n == 0 {
		// No discordant pairs: no evidence of asymmetry either way.
		return 1
	}
	k := b
	if c < b {
		k = c
	}

	// Sum the smaller tail, then double it. Computed in log space so a large
	// number of discordant pairs cannot overflow the binomial coefficient.
	logHalfN := float64(n) * math.Log(0.5)
	tail := 0.0
	for i := 0; i <= k; i++ {
		tail += math.Exp(logChoose(n, i) + logHalfN)
	}
	p := 2 * tail
	if p > 1 {
		return 1
	}
	return p
}

func logChoose(n, k int) float64 {
	if k < 0 || k > n {
		return math.Inf(-1)
	}
	lgN, _ := math.Lgamma(float64(n) + 1)
	lgK, _ := math.Lgamma(float64(k) + 1)
	lgNK, _ := math.Lgamma(float64(n-k) + 1)
	return lgN - lgK - lgNK
}
