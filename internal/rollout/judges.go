// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package rollout

import (
	"fmt"
	"sort"

	"github.com/waveoffai/waveoff/api/v1alpha1"
)

// DefaultKappaFloor is the agreement with the human gold set below which a
// judge is not trusted to decide a release.
//
// 0.6 is the bottom of "substantial" on the usual reading of Cohen's kappa, and
// the common practice is not to gate on a judge below moderate agreement. It is
// a convention rather than a law, which is why it is configurable.
const DefaultKappaFloor = 0.6

// JudgeCheck is the outcome of examining the judges a gate would rely on.
type JudgeCheck struct {
	Pass   bool
	Reason string
}

// CheckJudges refuses to gate on a judge that cannot be trusted.
//
// This is the gate on the gate. Every number the analyzer compares came from a
// judge, and a judge is a measuring instrument with its own error: if its
// agreement with the human gold set has drifted, or was never measured, or was
// measured against a different judge model than the one about to run, then the
// comparison is precise about a quantity nobody has validated.
//
// Refusing is the only safe answer. Promoting on an uncalibrated judge is worse
// than not gating at all, because it produces a number that looks like evidence.
func CheckJudges(incumbent, candidate *v1alpha1.AgentManifestSpec, floor float64) JudgeCheck {
	if floor <= 0 {
		floor = DefaultKappaFloor
	}
	if len(candidate.Judges) == 0 {
		// Nothing declared: the scores come from somewhere this manifest does
		// not describe, and there is nothing here to validate. Not this
		// check's business to refuse.
		return JudgeCheck{Pass: true, Reason: "the manifest declares no judges"}
	}

	prior := map[string]v1alpha1.JudgeSpec{}
	for _, j := range incumbent.Judges {
		prior[j.Name] = j
	}

	names := make([]string, 0, len(candidate.Judges))
	byName := map[string]v1alpha1.JudgeSpec{}
	for _, j := range candidate.Judges {
		names = append(names, j.Name)
		byName[j.Name] = j
	}
	sort.Strings(names)

	for _, name := range names {
		j := byName[name]

		if j.Calibration == nil || j.Calibration.Kappa == nil {
			return JudgeCheck{Reason: fmt.Sprintf(
				"judge %q has no recorded agreement with a gold set, so nothing it scores can be "+
					"trusted to decide a release", name)}
		}
		kappa := *j.Calibration.Kappa
		if kappa < floor {
			return JudgeCheck{Reason: fmt.Sprintf(
				"judge %q agrees with the gold set at κ=%.2f, below the floor of %.2f; "+
					"recalibrate it before gating on what it scores", name, kappa, floor)}
		}

		// A judge that changed since its calibration was taken is an
		// uncalibrated judge. The number was measured against a different
		// instrument.
		old, existed := prior[name]
		if !existed {
			continue
		}
		changed := old.Model != j.Model || old.RubricDigest != j.RubricDigest
		if !changed {
			continue
		}
		if !remeasured(old, j) {
			return JudgeCheck{Reason: fmt.Sprintf(
				"judge %q changed (%s → %s) and its calibration was not re-measured; "+
					"κ=%.2f describes a judge that no longer exists",
				name, describeJudge(old), describeJudge(j), kappa)}
		}
	}
	return JudgeCheck{Pass: true, Reason: fmt.Sprintf("%d judge(s) calibrated at or above κ=%.2f", len(names), floor)}
}

// remeasured reports whether the candidate's calibration is newer than the
// incumbent's, which is the only evidence available that it was taken against
// the changed judge.
func remeasured(old, next v1alpha1.JudgeSpec) bool {
	if next.Calibration == nil || next.Calibration.MeasuredAt == nil {
		return false
	}
	if old.Calibration == nil || old.Calibration.MeasuredAt == nil {
		// Nothing to compare against, but the candidate has a timestamp where
		// the incumbent had none. Treat that as a fresh measurement.
		return true
	}
	return next.Calibration.MeasuredAt.After(old.Calibration.MeasuredAt.Time)
}

func describeJudge(j v1alpha1.JudgeSpec) string {
	rubric := j.RubricDigest
	if len(rubric) > 7+8 {
		rubric = rubric[7:15] + "…"
	}
	return fmt.Sprintf("%s/%s", j.Model, rubric)
}
