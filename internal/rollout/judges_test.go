// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package rollout_test

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/waveoffai/waveoff/api/v1alpha1"
	"github.com/waveoffai/waveoff/internal/rollout"
)

func at(day int) *metav1.Time {
	t := metav1.NewTime(time.Date(2026, 8, day, 0, 0, 0, 0, time.UTC))
	return &t
}

func judge(model, rubric string, kappa float64, measured *metav1.Time) v1alpha1.JudgeSpec {
	return v1alpha1.JudgeSpec{
		Name: "task-completion", Model: model,
		RubricDigest: "sha256:" + strings.Repeat(rubric, 64),
		Calibration: &v1alpha1.JudgeCalibration{
			Kappa: &kappa, MeasuredAt: measured,
			GoldSetDigest: "sha256:" + strings.Repeat("9", 64),
		},
	}
}

func withJudges(js ...v1alpha1.JudgeSpec) *v1alpha1.AgentManifestSpec {
	return &v1alpha1.AgentManifestSpec{Agent: "support-agent", Judges: js}
}

func TestCalibratedJudgePasses(t *testing.T) {
	j := judge("claude-opus-5", "a", 0.78, at(1))
	c := rollout.CheckJudges(withJudges(j), withJudges(j), 0)
	if !c.Pass {
		t.Errorf("a well-calibrated judge was refused: %s", c.Reason)
	}
}

// TestUncalibratedJudgeIsRefused: a judge with no recorded agreement is an
// unvalidated instrument, and a number from one looks exactly like evidence.
func TestUncalibratedJudgeIsRefused(t *testing.T) {
	j := v1alpha1.JudgeSpec{Name: "task-completion", Model: "claude-opus-5"}
	c := rollout.CheckJudges(withJudges(j), withJudges(j), 0)
	if c.Pass {
		t.Fatal("a judge with no calibration was trusted to decide a release")
	}
	if !strings.Contains(c.Reason, "gold set") {
		t.Errorf("reason = %q", c.Reason)
	}
}

func TestLowAgreementIsRefused(t *testing.T) {
	j := judge("claude-opus-5", "a", 0.35, at(1))
	c := rollout.CheckJudges(withJudges(j), withJudges(j), 0)
	if c.Pass {
		t.Fatalf("a judge at κ=0.35 was trusted: %s", c.Reason)
	}
	if !strings.Contains(c.Reason, "0.35") {
		t.Errorf("the reason should quote the measured agreement: %q", c.Reason)
	}
}

func TestFloorIsConfigurable(t *testing.T) {
	j := judge("claude-opus-5", "a", 0.65, at(1))
	if c := rollout.CheckJudges(withJudges(j), withJudges(j), 0.6); !c.Pass {
		t.Errorf("κ=0.65 failed a floor of 0.6: %s", c.Reason)
	}
	if c := rollout.CheckJudges(withJudges(j), withJudges(j), 0.8); c.Pass {
		t.Error("κ=0.65 passed a floor of 0.8")
	}
}

// TestASwappedJudgeNeedsRecalibrating is the case waveoff diff already flags
// and the gate previously ignored: κ measured against the old judge says
// nothing about the new one.
func TestASwappedJudgeNeedsRecalibrating(t *testing.T) {
	old := judge("claude-opus-4-1", "a", 0.78, at(1))
	// Same calibration, different judge model.
	swapped := judge("claude-opus-5", "a", 0.78, at(1))

	c := rollout.CheckJudges(withJudges(old), withJudges(swapped), 0)
	if c.Pass {
		t.Fatal("a judge was swapped without recalibrating and the gate accepted it")
	}
	if !strings.Contains(c.Reason, "no longer exists") {
		t.Errorf("reason = %q", c.Reason)
	}
}

func TestARewrittenRubricNeedsRecalibrating(t *testing.T) {
	old := judge("claude-opus-5", "a", 0.78, at(1))
	rewritten := judge("claude-opus-5", "b", 0.78, at(1))

	if c := rollout.CheckJudges(withJudges(old), withJudges(rewritten), 0); c.Pass {
		t.Fatal("a rubric was rewritten without recalibrating and the gate accepted it")
	}
}

// TestRecalibrationClearsIt: doing the right thing must actually unblock the
// release, or nobody will do it.
func TestRecalibrationClearsIt(t *testing.T) {
	old := judge("claude-opus-4-1", "a", 0.78, at(1))
	swapped := judge("claude-opus-5", "a", 0.81, at(20)) // re-measured later

	c := rollout.CheckJudges(withJudges(old), withJudges(swapped), 0)
	if !c.Pass {
		t.Errorf("a recalibrated judge was still refused: %s", c.Reason)
	}
}

// TestNoJudgesIsNotThisChecksBusiness: scores may come from somewhere the
// manifest does not describe, and refusing on that basis would block every
// team that scores outside Waveoff.
func TestNoJudgesIsNotThisChecksBusiness(t *testing.T) {
	c := rollout.CheckJudges(withJudges(), withJudges(), 0)
	if !c.Pass {
		t.Errorf("a manifest declaring no judges was refused: %s", c.Reason)
	}
}

// TestAnAddedJudgeNeedsCalibrationButNotComparison: a judge the incumbent never
// had has nothing to be compared against, but still has to be calibrated.
func TestAnAddedJudgeNeedsCalibrationButNotComparison(t *testing.T) {
	added := judge("claude-opus-5", "a", 0.78, at(5))
	if c := rollout.CheckJudges(withJudges(), withJudges(added), 0); !c.Pass {
		t.Errorf("a newly added, calibrated judge was refused: %s", c.Reason)
	}

	uncalibrated := v1alpha1.JudgeSpec{Name: "new", Model: "m"}
	if c := rollout.CheckJudges(withJudges(), withJudges(uncalibrated), 0); c.Pass {
		t.Error("a newly added judge with no calibration was accepted")
	}
}
