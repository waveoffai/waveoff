// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package rollout_test

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/waveoffai/waveoff/api/v1alpha1"
	"github.com/waveoffai/waveoff/internal/analysis"
	"github.com/waveoffai/waveoff/internal/rollout"
	"github.com/waveoffai/waveoff/internal/traffic"
)

// fakeRouter records what a stage asked for.
type fakeRouter struct {
	split    traffic.Split
	mirror   int
	splitErr error
	calls    []string
	sticky   traffic.Stickiness
}

func (f *fakeRouter) Name() string { return "fake" }

func (f *fakeRouter) Stickiness(context.Context, traffic.Target) (traffic.Stickiness, error) {
	if f.sticky == "" {
		return traffic.StickyBySession, nil
	}
	return f.sticky, nil
}

func (f *fakeRouter) SetSplit(_ context.Context, _ traffic.Target, s traffic.Split) error {
	if f.splitErr != nil {
		return f.splitErr
	}
	if err := s.Valid(); err != nil {
		return err
	}
	f.split = s
	f.calls = append(f.calls, fmt.Sprintf("split %d/%d", s.Incumbent, s.Candidate))
	return nil
}

func (f *fakeRouter) Split(context.Context, traffic.Target) (traffic.Split, error) {
	return f.split, nil
}

func (f *fakeRouter) Mirror(_ context.Context, _ traffic.Target, percent int) error {
	f.mirror = percent
	f.calls = append(f.calls, fmt.Sprintf("mirror %d", percent))
	return nil
}

// fixedObservations returns a canary's worth of paired measurements.
type fixedObservations struct {
	effect float64
	n      int
	err    error
}

func (f *fixedObservations) Since(context.Context, string, string, string, time.Time) (
	[]analysis.Observation, analysis.Missingness, error) {
	if f.err != nil {
		return nil, analysis.Missingness{}, f.err
	}
	rng := rand.New(rand.NewSource(9))
	out := make([]analysis.Observation, f.n)
	for i := range out {
		base := rng.Float64()
		out[i] = analysis.Observation{
			Item:      fmt.Sprintf("req-%05d", i),
			Incumbent: map[string]float64{"task-completion": base},
			Candidate: map[string]float64{"task-completion": clampUnit(base + f.effect)},
		}
	}
	return out, analysis.Missingness{Attempted: f.n, BothScored: f.n}, nil
}

func clampUnit(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func liveStage(mutate func(*v1alpha1.Stage)) v1alpha1.Stage {
	margin := -0.02
	s := v1alpha1.Stage{
		Name: "canary", Mode: v1alpha1.StageLive, Weight: 5,
		Traffic: &v1alpha1.TrafficSpec{
			Router: "gateway-api", RouteRef: "support-agent",
			IncumbentBackend: "incumbent", CandidateBackend: "candidate",
		},
		Gate: v1alpha1.Gate{Primary: v1alpha1.GateMetric{
			Metric: "task-completion", Test: v1alpha1.GateSequential,
			Margin: &margin, Direction: v1alpha1.HigherIsBetter,
		}},
	}
	if mutate != nil {
		mutate(&s)
	}
	return s
}

func liveRollout(auto bool, triggers ...v1alpha1.RollbackTrigger) *v1alpha1.AgentRollout {
	return &v1alpha1.AgentRollout{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "prod"},
		Spec: v1alpha1.AgentRolloutSpec{
			Rollback: v1alpha1.RollbackSpec{Auto: auto, Triggers: triggers},
		},
	}
}

func liveRunner(router *fakeRouter, obs rollout.ObservationSource, now time.Time) *rollout.LiveRunner {
	return &rollout.LiveRunner{
		Router:       router,
		Observations: obs,
		Analyzer:     &analysis.Sequential{},
		Now:          func() time.Time { return now },
	}
}

// TestShadowGivesTheCandidateNoRealTraffic is the property that makes a shadow
// stage safe: the candidate meets production requests and no user depends on
// anything it produces.
func TestShadowGivesTheCandidateNoRealTraffic(t *testing.T) {
	router := &fakeRouter{}
	r := liveRunner(router, &fixedObservations{}, time.Now())

	stage := liveStage(func(s *v1alpha1.Stage) {
		s.Mode = v1alpha1.StageShadow
		s.MirrorPercent = 100
	})
	if err := r.Enter(context.Background(), liveRollout(true), stage); err != nil {
		t.Fatal(err)
	}
	if router.split.Candidate != 0 {
		t.Errorf("a shadow stage routed %d%% of real traffic to the candidate", router.split.Candidate)
	}
	if router.mirror != 100 {
		t.Errorf("mirror = %d, want 100", router.mirror)
	}
}

// TestLiveStopsMirroringFirst: a candidate serving traffic and also receiving
// mirrored copies of it sees every request twice, doubling its cost and
// corrupting every per-request metric.
func TestLiveStopsMirroringFirst(t *testing.T) {
	router := &fakeRouter{mirror: 100}
	r := liveRunner(router, &fixedObservations{}, time.Now())

	if err := r.Enter(context.Background(), liveRollout(true), liveStage(nil)); err != nil {
		t.Fatal(err)
	}
	if router.mirror != 0 {
		t.Errorf("mirroring was left on during a live stage: %d", router.mirror)
	}
	if router.split.Candidate != 5 {
		t.Errorf("candidate weight = %d, want 5", router.split.Candidate)
	}
	if len(router.calls) < 2 || !strings.HasPrefix(router.calls[0], "mirror") {
		t.Errorf("mirroring should be disabled before traffic is shifted: %v", router.calls)
	}
}

// TestARegressionWithdrawsTheCandidate is auto-rollback: a weight flip, not a
// rebuild.
func TestARegressionWithdrawsTheCandidate(t *testing.T) {
	router := &fakeRouter{split: traffic.Split{Incumbent: 95, Candidate: 5}}
	r := liveRunner(router, &fixedObservations{effect: -0.3, n: 800}, time.Now())

	step, err := r.Advance(context.Background(), liveRollout(true), liveStage(nil),
		manifest("support-agent", "sha256:a"), manifest("support-agent", "sha256:b"),
		time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if step.Outcome != rollout.StepRollBack {
		t.Fatalf("a 30-point regression did not withdraw the candidate: %s — %s", step.Outcome, step.Reason)
	}
	if step.Trigger != v1alpha1.TriggerGateFail {
		t.Errorf("trigger = %q", step.Trigger)
	}

	if err := r.Withdraw(context.Background(), liveRollout(true), liveStage(nil)); err != nil {
		t.Fatal(err)
	}
	if router.split.Candidate != 0 {
		t.Errorf("withdrawal left %d%% on the candidate", router.split.Candidate)
	}
	if router.mirror != 0 {
		t.Error("withdrawal left mirroring on")
	}
}

// TestAutomaticRollbackCanBeRefused: with auto off, a bad candidate stops the
// rollout advancing but a human decides whether to withdraw.
func TestAutomaticRollbackCanBeRefused(t *testing.T) {
	router := &fakeRouter{split: traffic.Split{Incumbent: 95, Candidate: 5}}
	r := liveRunner(router, &fixedObservations{effect: -0.3, n: 800}, time.Now())

	step, err := r.Advance(context.Background(), liveRollout(false), liveStage(nil),
		manifest("support-agent", "sha256:a"), manifest("support-agent", "sha256:b"),
		time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if step.Outcome == rollout.StepRollBack {
		t.Error("the candidate was withdrawn although automatic rollback is disabled")
	}
	if step.Outcome == rollout.StepPromote {
		t.Error("a failing candidate was promoted")
	}
	if !strings.Contains(step.Reason, "disabled") {
		t.Errorf("the reason should say why nothing happened: %q", step.Reason)
	}
}

// TestOnlySelectedTriggersFire.
func TestOnlySelectedTriggersFire(t *testing.T) {
	router := &fakeRouter{split: traffic.Split{Incumbent: 95, Candidate: 5}}
	// Automatic rollback on budget breach only.
	ro := liveRollout(true, v1alpha1.TriggerBudgetBreach)
	r := liveRunner(router, &fixedObservations{effect: -0.3, n: 800}, time.Now())

	step, err := r.Advance(context.Background(), ro, liveStage(nil),
		manifest("support-agent", "sha256:a"), manifest("support-agent", "sha256:b"),
		time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if step.Outcome == rollout.StepRollBack {
		t.Error("a gate failure withdrew the candidate although only budget-breach was enabled")
	}
}

// TestTheMinimumWindowIsHonouredEvenWhenTheNumbersAreConclusive.
//
// Statistical sufficiency is not the same as having seen the traffic that
// matters: a canary promoted in four minutes has not met the nightly batch or
// the Monday peak.
func TestTheMinimumWindowIsHonoured(t *testing.T) {
	router := &fakeRouter{split: traffic.Split{Incumbent: 95, Candidate: 5}}
	now := time.Now()
	r := liveRunner(router, &fixedObservations{effect: 0, n: 20000}, now)

	stage := liveStage(func(s *v1alpha1.Stage) {
		s.MinObservationWindow = &metav1.Duration{Duration: 24 * time.Hour}
	})
	step, err := r.Advance(context.Background(), liveRollout(true), stage,
		manifest("support-agent", "sha256:a"), manifest("support-agent", "sha256:b"),
		now.Add(-10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if step.Outcome != rollout.StepContinue {
		t.Fatalf("a ten-minute canary promoted against a 24-hour window: %s", step.Reason)
	}
	if !strings.Contains(step.Reason, "non-inferior") {
		t.Errorf("the reason should say the numbers are already fine: %q", step.Reason)
	}

	// Past the window, the same evidence promotes.
	later := liveRunner(router, &fixedObservations{effect: 0, n: 20000}, now)
	step, err = later.Advance(context.Background(), liveRollout(true), stage,
		manifest("support-agent", "sha256:a"), manifest("support-agent", "sha256:b"),
		now.Add(-25*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if step.Outcome != rollout.StepPromote {
		t.Errorf("outcome = %s after the window elapsed: %s", step.Outcome, step.Reason)
	}
}

// TestABudgetBreachWithdrawsTheCandidate: a canary with no end is a candidate
// that ships by exhaustion.
func TestABudgetBreachWithdrawsTheCandidate(t *testing.T) {
	router := &fakeRouter{split: traffic.Split{Incumbent: 95, Candidate: 5}}
	now := time.Now()
	// Never conclusive: too few observations to establish anything.
	r := liveRunner(router, &fixedObservations{effect: 0, n: 5}, now)

	stage := liveStage(func(s *v1alpha1.Stage) {
		s.MaxDuration = &metav1.Duration{Duration: time.Hour}
	})
	step, err := r.Advance(context.Background(), liveRollout(true), stage,
		manifest("support-agent", "sha256:a"), manifest("support-agent", "sha256:b"),
		now.Add(-2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if step.Outcome != rollout.StepRollBack {
		t.Fatalf("a canary past its budget kept running: %s", step.Reason)
	}
	if step.Trigger != v1alpha1.TriggerBudgetBreach {
		t.Errorf("trigger = %q", step.Trigger)
	}
}

// TestAnUnreachableAnalyzerWithdraws: a candidate serving real traffic that
// nobody is checking is worse than no candidate at all.
func TestAnUnreachableAnalyzerWithdraws(t *testing.T) {
	router := &fakeRouter{split: traffic.Split{Incumbent: 95, Candidate: 5}}
	r := liveRunner(router, &fixedObservations{effect: 0, n: 500}, time.Now())
	r.Analyzer = &failingAnalyzer{}

	step, err := r.Advance(context.Background(), liveRollout(true), liveStage(nil),
		manifest("support-agent", "sha256:a"), manifest("support-agent", "sha256:b"),
		time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if step.Outcome != rollout.StepRollBack {
		t.Fatalf("an unreachable analyzer left the candidate serving: %s", step.Reason)
	}
	if step.Trigger != v1alpha1.TriggerAnalyzerUnavailable {
		t.Errorf("trigger = %q", step.Trigger)
	}
	if !strings.Contains(step.Reason, "nobody is checking") {
		t.Errorf("the reason should say why this is unsafe: %q", step.Reason)
	}
}

// TestTheWeightIsReadBackFromTheRouter, not assumed from the last write. A
// controller that trusts its own writes reports a candidate as withdrawn while
// it is still serving.
func TestTheWeightIsReadBackFromTheRouter(t *testing.T) {
	// The router says 20% even though nothing here set that.
	router := &fakeRouter{split: traffic.Split{Incumbent: 80, Candidate: 20}}
	r := liveRunner(router, &fixedObservations{effect: 0, n: 100}, time.Now())

	step, err := r.Advance(context.Background(), liveRollout(true), liveStage(nil),
		manifest("support-agent", "sha256:a"), manifest("support-agent", "sha256:b"),
		time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if step.Weight != 20 {
		t.Errorf("weight = %d; it must come from the router, not from what we last asked for", step.Weight)
	}
}

// TestAnEarlyCanaryKeepsWatching: undecided is the normal state of a canary
// that has not seen enough traffic, not a fault.
func TestAnEarlyCanaryKeepsWatching(t *testing.T) {
	router := &fakeRouter{split: traffic.Split{Incumbent: 95, Candidate: 5}}
	r := liveRunner(router, &fixedObservations{effect: 0, n: 12}, time.Now())

	step, err := r.Advance(context.Background(), liveRollout(true), liveStage(nil),
		manifest("support-agent", "sha256:a"), manifest("support-agent", "sha256:b"),
		time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if step.Outcome != rollout.StepContinue {
		t.Errorf("a canary with twelve observations decided %s: %s", step.Outcome, step.Reason)
	}
	if step.RequeueAfter <= 0 {
		t.Error("a continuing canary must ask to be looked at again")
	}
}

// fakeActivity reports fixed write attempts for each arm.
type fakeActivity struct {
	incumbent, candidate map[string]int
	sessions             int
}

func (f *fakeActivity) Activity(context.Context, string, string, string, time.Time) (
	analysis.Activity, analysis.Activity, error) {

	n := f.sessions
	if n == 0 {
		n = 50
	}
	return analysis.Activity{Arm: "incumbent", Sessions: n, Attempts: f.incumbent},
		analysis.Activity{Arm: "candidate", Sessions: n, Attempts: f.candidate}, nil
}

// TestAWriteTheIncumbentNeverMakesWithdrawsTheCandidate.
//
// The whole point of the check running before the analyzer: the observations
// here say the candidate is fine, and it is being withdrawn anyway because it
// reached for a tool the incumbent does not use. A statistical gate would need
// hundreds of sessions to notice; this needs one.
func TestAWriteTheIncumbentNeverMakesWithdrawsTheCandidate(t *testing.T) {
	router := &fakeRouter{split: traffic.Split{Incumbent: 95, Candidate: 5}}
	r := liveRunner(router, &fixedObservations{effect: 0.05, n: 800}, time.Now())
	r.Activity = &fakeActivity{
		incumbent: map[string]int{"jira.create_issue": 40},
		candidate: map[string]int{"jira.create_issue": 41, "jira.delete_issue": 1},
	}

	step, err := r.Advance(context.Background(), liveRollout(true), liveStage(nil),
		manifest("support-agent", "sha256:a"), manifest("support-agent", "sha256:b"),
		time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if step.Outcome != rollout.StepRollBack {
		t.Fatalf("outcome = %q, reason %q", step.Outcome, step.Reason)
	}
	if step.Trigger != v1alpha1.TriggerWriteDivergence {
		t.Errorf("trigger = %q", step.Trigger)
	}
	if step.Activity == nil || !step.Activity.Violated {
		t.Error("the finding was not reported on the step")
	}
}

// TestWriteActivityIsReportedEvenWhenItPasses.
//
// A shadow stage where the arms wrote through the same tools is a result, not
// an absence of one, and the operator has to be able to see that the check
// actually ran. "Safe and useless looks identical to working" applies here too.
func TestWriteActivityIsReportedEvenWhenItPasses(t *testing.T) {
	router := &fakeRouter{split: traffic.Split{Incumbent: 95, Candidate: 5}}
	r := liveRunner(router, &fixedObservations{effect: 0.05, n: 800}, time.Now())
	r.Activity = &fakeActivity{
		incumbent: map[string]int{"jira.create_issue": 40},
		candidate: map[string]int{"jira.create_issue": 44},
	}

	step, err := r.Advance(context.Background(), liveRollout(true), liveStage(nil),
		manifest("support-agent", "sha256:a"), manifest("support-agent", "sha256:b"),
		time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if step.Outcome == rollout.StepRollBack {
		t.Fatalf("withdrawn on a shared write set: %s", step.Reason)
	}
	if step.Activity == nil || step.Activity.Violated {
		t.Fatal("no activity finding was reported")
	}
	if step.Activity.Candidate.Rate() <= step.Activity.Incumbent.Rate() {
		t.Error("the rates were not carried through")
	}
}

// TestWriteDivergenceRespectsTheTriggerList.
//
// A team that has switched this trigger off has said, explicitly, that they
// want to look at a divergence themselves rather than have a candidate
// withdrawn for it. The finding still has to be reported.
func TestWriteDivergenceRespectsTheTriggerList(t *testing.T) {
	router := &fakeRouter{split: traffic.Split{Incumbent: 95, Candidate: 5}}
	r := liveRunner(router, &fixedObservations{effect: 0.05, n: 800}, time.Now())
	r.Activity = &fakeActivity{
		incumbent: map[string]int{"jira.create_issue": 40},
		candidate: map[string]int{"jira.create_issue": 40, "jira.delete_issue": 2},
	}

	ro := liveRollout(true, v1alpha1.TriggerGateFail)
	step, err := r.Advance(context.Background(), ro, liveStage(nil),
		manifest("support-agent", "sha256:a"), manifest("support-agent", "sha256:b"),
		time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if step.Trigger == v1alpha1.TriggerWriteDivergence {
		t.Error("withdrew on a trigger the rollout excluded")
	}
	if step.Activity == nil || !step.Activity.Violated {
		t.Error("the divergence was not reported")
	}
}
