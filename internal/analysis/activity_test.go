// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package analysis_test

import (
	"strings"
	"testing"

	"github.com/waveoffai/waveoff/internal/analysis"
)

func activity(arm string, sessions int, attempts map[string]int) analysis.Activity {
	return analysis.Activity{Arm: arm, Sessions: sessions, Attempts: attempts}
}

// TestANewWriteClassIsAViolationOnFirstSight.
//
// The incumbent has been serving this traffic in production, so the set of
// tools it writes through is the demonstrated normal. A candidate reaching
// outside that set has changed what the agent does, not how often — and waiting
// for a rate to reach significance means waiting for a destructive call to
// happen repeatedly first.
func TestANewWriteClassIsAViolationOnFirstSight(t *testing.T) {
	f := analysis.CompareActivity(
		activity("incumbent", 200, map[string]int{"jira.create_issue": 180}),
		activity("candidate", 200, map[string]int{"jira.create_issue": 175, "jira.delete_issue": 1}),
	)

	if !f.Violated {
		t.Fatal("a single attempt at an unseen write class did not fire the guardrail")
	}
	if len(f.NewClasses) != 1 || f.NewClasses[0] != "jira.delete_issue" {
		t.Errorf("NewClasses = %v", f.NewClasses)
	}
	if !strings.Contains(f.Reason, "jira.delete_issue") {
		t.Errorf("the reason should name the tool: %q", f.Reason)
	}
}

// TestMoreOfTheSameWritesIsNotAViolation.
//
// Three times as many tickets might be a regression or a busier hour. That is
// a rate question, and separating those two is what the primary metric and its
// interval exist for — this check must not pre-empt it.
func TestMoreOfTheSameWritesIsNotAViolation(t *testing.T) {
	f := analysis.CompareActivity(
		activity("incumbent", 100, map[string]int{"jira.create_issue": 20}),
		activity("candidate", 100, map[string]int{"jira.create_issue": 60}),
	)

	if f.Violated {
		t.Errorf("a rate difference fired a set-membership guardrail: %s", f.Reason)
	}
	if f.Candidate.Rate() != 0.6 || f.Incumbent.Rate() != 0.2 {
		t.Errorf("rates = %v / %v", f.Incumbent.Rate(), f.Candidate.Rate())
	}
}

// TestAWriteClassTheCandidateStoppedUsingIsReportedNotJudged.
//
// A candidate that has stopped writing may have got better or may have got
// lazier, and only a scorer can tell those apart. Reporting it is useful;
// withdrawing on it would wave off half the improvements this system exists to
// ship.
func TestAWriteClassTheCandidateStoppedUsingIsReportedNotJudged(t *testing.T) {
	f := analysis.CompareActivity(
		activity("incumbent", 50, map[string]int{"jira.create_issue": 10, "slack.post": 5}),
		activity("candidate", 50, map[string]int{"jira.create_issue": 10}),
	)

	if f.Violated {
		t.Error("dropping a write class waved off the candidate")
	}
	if len(f.DroppedClasses) != 1 || f.DroppedClasses[0] != "slack.post" {
		t.Errorf("DroppedClasses = %v", f.DroppedClasses)
	}
}

// TestOneArmWithNoSessionsCannotBeCompared.
//
// With no incumbent traffic there is no demonstrated normal, so every class the
// candidate touches would look new. Firing there would wave off every candidate
// in the first seconds of a stage.
func TestOneArmWithNoSessionsCannotBeCompared(t *testing.T) {
	f := analysis.CompareActivity(
		activity("incumbent", 0, nil),
		activity("candidate", 3, map[string]int{"jira.create_issue": 3}),
	)

	if f.Violated {
		t.Errorf("compared against an arm that has not run: %s", f.Reason)
	}
	if !strings.Contains(f.Reason, "not enough sessions") {
		t.Errorf("reason = %q", f.Reason)
	}
}
