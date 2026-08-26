// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package rollout

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/waveoffai/waveoff/api/v1alpha1"
	"github.com/waveoffai/waveoff/internal/analysis"
	"github.com/waveoffai/waveoff/internal/score"
	"github.com/waveoffai/waveoff/internal/traffic"
)

// LiveRunner drives the shadow and live stages.
//
// The offline stages run to completion inside one call. These do not: a canary
// is a thing that is happening, watched repeatedly, and the controller advances
// it a step at a time. So this exposes a single Step, and everything about
// where the stage has got to lives in status rather than in memory — which is
// what makes a manager restart mid-canary survivable.
type LiveRunner struct {
	Router traffic.Router
	Scorer score.Scorer
	// Analyzer must be anytime-valid for a live stage. The controller checks
	// that before it starts one.
	Analyzer analysis.Analyzer

	// Observations supplies the paired measurements accumulated so far.
	Observations ObservationSource

	// Activity supplies what each arm tried to write.
	//
	// Optional: a stage with no write tools has nothing to report and a
	// deployment that has not wired a source still gets a working gate. When it
	// is present it is checked before the analyzer, because it is deterministic
	// and needs no accumulation — it can withdraw a candidate on the first
	// session, before any interval has begun to close.
	Activity ActivitySource

	Now func() time.Time
}

// ObservationSource returns the paired measurements a live stage has gathered.
//
// Behind an interface because where they come from is a deployment decision:
// the recorder's corpus, an eval vendor's store, an operator's own pipeline.
// The gate only needs the numbers.
type ObservationSource interface {
	// Since returns the measurements recorded for either arm since a stage
	// began. The two behaviour digests are what attribute a session to an arm:
	// a recording that cannot name the manifest it ran against is not
	// attributable, and guessing would put one arm's work on the other's side
	// of the comparison.
	Since(ctx context.Context, agent, incumbent, candidate string, since time.Time) (
		[]analysis.Observation, analysis.Missingness, error)
}

// ActivitySource returns what each arm attempted to write.
//
// In shadow the candidate's writes never happen, so the attempts are the only
// evidence of them — recorded by the suppressor at the moment it declined to
// forward the call.
type ActivitySource interface {
	Activity(ctx context.Context, agent, incumbent, candidate string, since time.Time) (
		analysis.Activity, analysis.Activity, error)
}

// StepOutcome is what the controller should do next.
type StepOutcome string

const (
	// StepContinue: not enough evidence, or not enough time. Keep watching.
	StepContinue StepOutcome = "continue"
	// StepPromote: the stage is satisfied.
	StepPromote StepOutcome = "promote"
	// StepRollBack: withdraw the candidate.
	StepRollBack StepOutcome = "roll-back"
)

// Step is one pass of a live or shadow stage.
type Step struct {
	Outcome StepOutcome
	Reason  string
	Trigger v1alpha1.RollbackTrigger
	Verdict analysis.Verdict
	// Activity is the deterministic comparison of what each arm tried to
	// write. Reported on every step, whether or not it fired.
	Activity *analysis.ActivityFinding
	// Weight is what the router reports the candidate actually holds.
	Weight int
	// RequeueAfter is how long to wait before looking again.
	RequeueAfter time.Duration
}

// DefaultPollInterval is how often a live stage is re-examined.
//
// Frequent enough to withdraw a bad candidate quickly, and affordable precisely
// because the gate is anytime-valid: with a fixed-horizon test, looking this
// often would be the thing that breaks it.
const DefaultPollInterval = 30 * time.Second

// Advance moves a live or shadow stage on by one step.
func (l *LiveRunner) Advance(ctx context.Context, rollout *v1alpha1.AgentRollout,
	stage v1alpha1.Stage, incumbent, candidate *v1alpha1.AgentManifestSpec,
	startedAt time.Time) (*Step, error) {

	now := l.now()
	target := targetFor(rollout, stage)

	// Read back what the router actually has, rather than what we last asked
	// for. A controller that assumes its own writes took effect will report a
	// candidate as withdrawn while it is still serving.
	split, err := l.Router.Split(ctx, target)
	if err != nil {
		return nil, fmt.Errorf("reading the traffic split: %w", err)
	}
	step := &Step{Weight: split.Candidate, RequeueAfter: DefaultPollInterval}

	// A stage that has run past its budget is withdrawn rather than left
	// running. A canary with no end is a candidate that ships by exhaustion.
	if stage.MaxDuration != nil && now.Sub(startedAt) > stage.MaxDuration.Duration {
		step.Outcome = StepRollBack
		step.Trigger = v1alpha1.TriggerBudgetBreach
		step.Reason = fmt.Sprintf("stage %q ran for %s without concluding, past its budget of %s",
			stage.Name, now.Sub(startedAt).Round(time.Second), stage.MaxDuration.Duration)
		return step, nil
	}

	// What each arm reached for is checked first. It is deterministic, it needs
	// no accumulation, and in shadow it is the only direct evidence of the
	// writes the candidate would have made.
	if l.Activity != nil {
		inc, cand, err := l.Activity.Activity(ctx, incumbent.Agent,
			incumbent.BehaviorDigest, candidate.BehaviorDigest, startedAt)
		if err != nil {
			return nil, fmt.Errorf("gathering write attempts: %w", err)
		}
		finding := analysis.CompareActivity(inc, cand)
		step.Activity = &finding
		if finding.Violated && rollout.Spec.Rollback.Fires(v1alpha1.TriggerWriteDivergence) {
			step.Outcome = StepRollBack
			step.Trigger = v1alpha1.TriggerWriteDivergence
			step.Reason = finding.Reason
			return step, nil
		}
	}

	observations, missing, err := l.Observations.Since(ctx, incumbent.Agent,
		incumbent.BehaviorDigest, candidate.BehaviorDigest, startedAt)
	if err != nil {
		return nil, fmt.Errorf("gathering observations: %w", err)
	}
	if len(observations) == 0 {
		step.Outcome = StepContinue
		step.Reason = "no paired observations yet"
		return step, nil
	}

	req := analysis.Request{
		Agent:     incumbent.Agent,
		Incumbent: incumbent.BehaviorDigest,
		Candidate: candidate.BehaviorDigest,
		Primary:   toMetric(stage.Gate.Primary),
		Missing:   missing,
		Design:    designFor(stage.Mode),
		Seed:      1,
	}
	for _, g := range stage.Gate.Guardrails {
		req.Guardrails = append(req.Guardrails, toMetric(g))
	}
	req.Observations = observations

	verdict, err := l.Analyzer.Analyze(ctx, req)
	if err != nil {
		// An analyzer that cannot answer must not leave a candidate serving
		// traffic on the assumption it is fine. This is the one failure that
		// justifies withdrawing without evidence: the absence of a verdict is
		// itself the problem.
		if rollout.Spec.Rollback.Fires(v1alpha1.TriggerAnalyzerUnavailable) {
			step.Outcome = StepRollBack
			step.Trigger = v1alpha1.TriggerAnalyzerUnavailable
			step.Reason = fmt.Sprintf("the analyzer could not be reached, and a candidate is serving "+
				"traffic nobody is checking: %v", err)
			return step, nil
		}
		return nil, fmt.Errorf("analyzer: %w", err)
	}
	step.Verdict = verdict

	switch verdict.Outcome {
	case analysis.OutcomeWaveOff:
		trigger := v1alpha1.TriggerGateFail
		for _, g := range verdict.Guardrails {
			if g.Inferior {
				trigger = v1alpha1.TriggerGuardrailViolation
			}
		}
		if rollout.Spec.Rollback.Fires(trigger) {
			step.Outcome = StepRollBack
			step.Trigger = trigger
			step.Reason = verdict.Reason
			return step, nil
		}
		// Automatic rollback is off: hold rather than withdraw, and let a
		// human decide. Holding still stops the rollout advancing.
		step.Outcome = StepContinue
		step.Reason = "waved off, but automatic rollback is disabled: " + verdict.Reason
		step.RequeueAfter = 0
		return step, nil

	case analysis.OutcomePromote:
		// Statistical sufficiency is not the same as having seen the traffic
		// that matters. A canary promoted in four minutes has not met the
		// nightly batch or the Monday peak.
		if stage.MinObservationWindow != nil {
			elapsed := now.Sub(startedAt)
			if elapsed < stage.MinObservationWindow.Duration {
				step.Outcome = StepContinue
				step.Reason = fmt.Sprintf(
					"%s is already non-inferior, but the stage has run for %s of a required %s",
					stage.Gate.Primary.Metric, elapsed.Round(time.Second),
					stage.MinObservationWindow.Duration)
				return step, nil
			}
		}
		step.Outcome = StepPromote
		step.Reason = verdict.Reason
		return step, nil

	default:
		step.Outcome = StepContinue
		step.Reason = verdict.Reason
		return step, nil
	}
}

// Enter puts a stage's traffic configuration in place.
func (l *LiveRunner) Enter(ctx context.Context, rollout *v1alpha1.AgentRollout, stage v1alpha1.Stage) error {
	target := targetFor(rollout, stage)

	switch stage.Mode {
	case v1alpha1.StageShadow:
		// The candidate takes no real traffic; it only receives copies.
		if err := l.Router.SetSplit(ctx, target, traffic.FullyIncumbent); err != nil {
			return err
		}
		percent := stage.MirrorPercent
		if percent == 0 {
			percent = 100
		}
		return l.Router.Mirror(ctx, target, percent)

	case v1alpha1.StageLive:
		// Mirroring is turned off first. A candidate serving traffic *and*
		// receiving mirrored copies of it would see every request twice, which
		// doubles its cost and corrupts any per-request metric.
		if err := l.Router.Mirror(ctx, target, 0); err != nil && !isUnsupported(err) {
			return err
		}
		return l.Router.SetSplit(ctx, target, traffic.Split{
			Incumbent: 100 - stage.Weight, Candidate: stage.Weight,
		})
	}
	return fmt.Errorf("stage %q has mode %q, which is not a live stage", stage.Name, stage.Mode)
}

// Withdraw returns all traffic to the incumbent and stops mirroring.
//
// This is the rollback. It is a weight flip rather than a rebuild, which is why
// the incumbent is kept available — the difference between a rollback that
// takes a second and one that takes a deploy.
func (l *LiveRunner) Withdraw(ctx context.Context, rollout *v1alpha1.AgentRollout, stage v1alpha1.Stage) error {
	target := targetFor(rollout, stage)

	// Split first, then mirror. If only one of the two succeeds, the candidate
	// having no live traffic matters more than it still receiving copies.
	if err := l.Router.SetSplit(ctx, target, traffic.FullyIncumbent); err != nil {
		return err
	}
	if err := l.Router.Mirror(ctx, target, 0); err != nil && !isUnsupported(err) {
		return err
	}
	return nil
}

// designFor says whether a stage's two arms saw the same inputs.
//
// Shadow mirrors each request to both, so the comparison is paired and item
// difficulty cancels. A live canary routes each request to one arm or the
// other, so nothing cancels and the same conclusion needs far more traffic.
// This is a property of how the stage runs, not a setting.
func designFor(mode v1alpha1.StageMode) analysis.Design {
	if mode == v1alpha1.StageLive {
		return analysis.DesignUnpaired
	}
	return analysis.DesignPaired
}

func targetFor(rollout *v1alpha1.AgentRollout, stage v1alpha1.Stage) traffic.Target {
	t := traffic.Target{Namespace: rollout.Namespace}
	if stage.Traffic != nil {
		t.Name = stage.Traffic.RouteRef
		t.Incumbent = stage.Traffic.IncumbentBackend
		t.Candidate = stage.Traffic.CandidateBackend
	}
	return t
}

func isUnsupported(err error) bool {
	return errors.Is(err, traffic.ErrUnsupported)
}

func (l *LiveRunner) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}
