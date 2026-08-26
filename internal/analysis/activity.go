// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package analysis

import (
	"fmt"
	"sort"
)

// Activity is what one arm tried to do to the world.
//
// In a shadow stage the candidate's writes are suppressed, so nothing
// downstream can compare their effects: the ticket was filed by the incumbent
// and not by the candidate, and no scorer can see a difference that did not
// happen. The attempts, however, are directly comparable — both arms saw the
// same traffic and each decided, independently, what to reach for.
//
// That makes this the one signal a shadow stage produces that needs no judge,
// no rubric and no statistics. It is also the earliest: a candidate that has
// started reaching for a destructive tool is visible on the first session,
// long before enough observations exist for any interval to close.
type Activity struct {
	Arm string
	// Sessions is how many sessions the counts are drawn from, so two arms that
	// saw different amounts of traffic are not compared as though they had not.
	Sessions int
	// Attempts counts write attempts by tool name.
	Attempts map[string]int
}

// Tools returns the write classes this arm reached for, sorted.
func (a Activity) Tools() []string {
	out := make([]string, 0, len(a.Attempts))
	for name, n := range a.Attempts {
		if n > 0 {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Total is every write attempt, across tools.
func (a Activity) Total() int {
	var n int
	for _, v := range a.Attempts {
		n += v
	}
	return n
}

// Rate is write attempts per session, or zero when no sessions ran.
func (a Activity) Rate() float64 {
	if a.Sessions == 0 {
		return 0
	}
	return float64(a.Total()) / float64(a.Sessions)
}

// ActivityFinding compares two arms' write attempts.
type ActivityFinding struct {
	Incumbent Activity
	Candidate Activity

	// NewClasses are write tools the candidate reached for and the incumbent
	// never did. This is the guardrail: it is a category change, not a rate
	// change, so it does not need a threshold and does not need a test.
	NewClasses []string
	// DroppedClasses are write tools the incumbent used and the candidate did
	// not. Reported but never a violation on its own — a candidate that has
	// stopped writing may have got better, may have got lazier, and only a
	// scorer can tell those apart.
	DroppedClasses []string

	// Violated is true when the candidate attempted a write class the
	// incumbent did not.
	Violated bool
	Reason   string
}

// CompareActivity is a deterministic guardrail on what each arm tried to do.
//
// It fires on set membership, not on counts. A single attempt at a write class
// the incumbent never touches is enough: the incumbent has been serving this
// traffic in production, so its write classes are the demonstrated normal, and
// a candidate reaching outside that set has changed what the agent does rather
// than how often it does it. Waiting for a rate to become significant would
// mean waiting for it to happen repeatedly, which for a destructive tool is the
// wrong way round.
//
// Rate differences within the shared set are reported and deliberately not
// judged here. "Three times as many tickets" might be a regression or a busier
// hour, and separating those is what the primary metric and its interval are
// for.
func CompareActivity(incumbent, candidate Activity) ActivityFinding {
	f := ActivityFinding{Incumbent: incumbent, Candidate: candidate}

	seen := map[string]bool{}
	for _, t := range incumbent.Tools() {
		seen[t] = true
	}
	for _, t := range candidate.Tools() {
		if !seen[t] {
			f.NewClasses = append(f.NewClasses, t)
		}
	}
	candidateSet := map[string]bool{}
	for _, t := range candidate.Tools() {
		candidateSet[t] = true
	}
	for _, t := range incumbent.Tools() {
		if !candidateSet[t] {
			f.DroppedClasses = append(f.DroppedClasses, t)
		}
	}

	// Both arms have to have run for a set difference to mean anything. If the
	// incumbent has no sessions there is no demonstrated normal to compare
	// against, and every class the candidate uses would look new.
	if incumbent.Sessions == 0 || candidate.Sessions == 0 {
		f.Reason = "not enough sessions on both arms to compare write behaviour"
		return f
	}

	if len(f.NewClasses) > 0 {
		f.Violated = true
		f.Reason = fmt.Sprintf(
			"the candidate attempted %v, which the incumbent never does across %d sessions. "+
				"This is a change in what the agent reaches for, not in how often — "+
				"no amount of further traffic makes it expected",
			f.NewClasses, incumbent.Sessions)
		return f
	}

	f.Reason = fmt.Sprintf("both arms wrote through the same %d tool classes "+
		"(incumbent %.2f attempts/session, candidate %.2f)",
		len(incumbent.Tools()), incumbent.Rate(), candidate.Rate())
	return f
}
