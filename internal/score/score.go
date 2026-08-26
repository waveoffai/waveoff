// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

// Package score is the boundary between Waveoff and whatever evaluates an
// agent's output.
//
// Waveoff does not build evals. Eval-in-CI is a solved, crowded category, and
// a customer already runs one; what nobody owns is the layer after the score.
// So this package defines the smallest interface that lets a score arrive from
// outside, and nothing more.
//
// # Why there are no vendor SDK adapters here
//
// The obvious reading of "adapters for two vendors" is two packages importing
// two vendors' Go SDKs. That would put third-party dependencies in the OSS
// module for vendors we do not control, on release cycles we do not follow,
// and would quietly pick winners in a market that has not settled.
//
// Instead there are two transports — a subprocess and an HTTP endpoint — that
// any vendor plugs into without this repository knowing they exist. A
// Braintrust adapter is a twenty-line script; so is a Langfuse one. The
// examples live in docs/scoring.md rather than in the dependency graph.
package score

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// SchemaVersion identifies the wire shape a scorer sees. It is a supported
// interface: third parties implement against it, so fields may be added but
// never removed or repurposed without a bump.
const SchemaVersion = "waveoff.ai/score/v1alpha1"

// Ref points a scorer at one thing to score.
//
// The scorer is given a reference rather than a payload. A session transcript
// can be megabytes, a scoring run covers hundreds of them, and a vendor that
// wants the bytes can read them from the corpus with the tooling it already
// has. Passing references also keeps the contract stable while the cassette
// format evolves underneath it.
type Ref struct {
	// Item is the pairing key: the recorded session both arms were driven
	// from.
	//
	// This is the field that makes a paired test possible. A paired comparison
	// needs the same input scored under both arms, and "the same input" is a
	// corpus item, not a replay — the two replays have different session ids
	// by construction.
	Item string `json:"item"`

	// Arm says which side of the comparison this is.
	Arm string `json:"arm"`

	// Session is the replay output's own session id, and Corpus is where it
	// lives. Together they locate the cassette holding what the agent produced.
	Session string `json:"session"`
	Corpus  string `json:"corpus,omitempty"`
	Blobs   string `json:"blobs,omitempty"`

	// Manifest identifies the version that produced this output, so a scorer
	// that cares which judge configuration applies can find it.
	Manifest string `json:"manifest,omitempty"`

	// Degraded reports that the replay did not run cleanly — calls refused,
	// writes synthesised, contracts drifted.
	//
	// A scorer is told rather than left to infer it. A session where half the
	// tools were no-op'd can still be scored, but the number means something
	// different, and a gate that cannot tell the two apart is measuring noise.
	Degraded bool   `json:"degraded,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// Result is what a scorer returns for one Ref.
type Result struct {
	Item string `json:"item"`
	Arm  string `json:"arm"`

	// Metrics are named values. The gate names one of these as its primary
	// metric, so the names are the customer's vocabulary and not ours.
	Metrics map[string]float64 `json:"metrics"`

	// Metadata carries whatever the scorer wants to hand back — a judge model
	// id, a rubric version, a link to a trace in the vendor's own UI.
	Metadata map[string]string `json:"metadata,omitempty"`

	// Error explains why this item could not be scored.
	//
	// Distinguished from a zero score on purpose, and it is the single most
	// important field here. A judge that timed out has not decided the agent
	// failed; counting that as 0.0 is how a gate rolls back a good release
	// because a scoring service was briefly slow.
	Error string `json:"error,omitempty"`
}

// Scored reports whether this result carries a usable measurement.
func (r Result) Scored() bool { return r.Error == "" && len(r.Metrics) > 0 }

// Scorer turns replay outputs into named metrics.
//
// Implementations must return one Result per Ref, in any order, matched by
// (Item, Arm). A missing Result is treated as an unscored item rather than a
// failure of the whole run: one flaky judge call must not discard a batch.
type Scorer interface {
	Score(ctx context.Context, refs []Ref) ([]Result, error)
}

// Batch is a scoring run over both arms of a comparison.
type Batch struct {
	Refs    []Ref
	Results []Result
}

// Pair is one item scored under both arms — the unit a paired test consumes.
type Pair struct {
	Item      string
	Incumbent Result
	Candidate Result
}

// Complete reports whether both arms produced a usable score for a metric.
//
// Incomplete pairs are dropped rather than filled in. Substituting a mean or a
// zero for a missing arm invents data at exactly the point where the test is
// most sensitive to it.
func (p Pair) Complete(metric string) bool {
	if !p.Incumbent.Scored() || !p.Candidate.Scored() {
		return false
	}
	_, a := p.Incumbent.Metrics[metric]
	_, b := p.Candidate.Metrics[metric]
	return a && b
}

// Pairs assembles results into the paired form a comparison needs.
//
// Returns the complete pairs and the items that could not be paired, so a
// caller can report how much of the corpus was actually measured. A gate that
// silently scores 40 of 400 items looks identical to one that scored all 400.
func Pairs(results []Result, metric string) (paired []Pair, dropped []string) {
	byItem := map[string]*Pair{}
	for _, r := range results {
		p, ok := byItem[r.Item]
		if !ok {
			p = &Pair{Item: r.Item}
			byItem[r.Item] = p
		}
		switch strings.ToLower(r.Arm) {
		case "incumbent":
			p.Incumbent = r
		case "candidate":
			p.Candidate = r
		}
	}

	items := make([]string, 0, len(byItem))
	for item := range byItem {
		items = append(items, item)
	}
	sort.Strings(items)

	for _, item := range items {
		p := byItem[item]
		if p.Complete(metric) {
			paired = append(paired, *p)
			continue
		}
		dropped = append(dropped, item)
	}
	return paired, dropped
}

// Values extracts the two arms' measurements for a metric, aligned by item.
func Values(pairs []Pair, metric string) (incumbent, candidate []float64) {
	incumbent = make([]float64, 0, len(pairs))
	candidate = make([]float64, 0, len(pairs))
	for _, p := range pairs {
		incumbent = append(incumbent, p.Incumbent.Metrics[metric])
		candidate = append(candidate, p.Candidate.Metrics[metric])
	}
	return incumbent, candidate
}

// Validate checks a scorer's output before it reaches a gate.
//
// Metrics arrive from a process this repository does not control, and a NaN
// reaching a bootstrap produces a confidence interval of NaN that compares
// false against every threshold — a gate that silently never fires.
func Validate(results []Result) error {
	for i, r := range results {
		if r.Item == "" {
			return fmt.Errorf("result %d has no item; there is nothing to pair it with", i)
		}
		if arm := strings.ToLower(r.Arm); arm != "incumbent" && arm != "candidate" {
			return fmt.Errorf("result %d (%s): arm is %q, want incumbent or candidate", i, r.Item, r.Arm)
		}
		for name, v := range r.Metrics {
			if isNaN(v) || isInf(v) {
				return fmt.Errorf("result %d (%s/%s): metric %q is %v, which no test can compare",
					i, r.Item, r.Arm, name, v)
			}
		}
	}
	return nil
}

func isNaN(f float64) bool { return f != f }
func isInf(f float64) bool { return f > 1e308 || f < -1e308 }
