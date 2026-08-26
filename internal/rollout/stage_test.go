// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package rollout_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/waveoffai/waveoff/api/v1alpha1"
	"github.com/waveoffai/waveoff/internal/analysis"
	"github.com/waveoffai/waveoff/internal/cassette"
	"github.com/waveoffai/waveoff/internal/corpus"
	"github.com/waveoffai/waveoff/internal/replay"
	"github.com/waveoffai/waveoff/internal/rollout"
	"github.com/waveoffai/waveoff/internal/score"
)

func ptr[T any](v T) *T { return &v }

func manifest(name, digest string) *v1alpha1.AgentManifestSpec {
	return &v1alpha1.AgentManifestSpec{Agent: name, BehaviorDigest: digest}
}

// corpusWith creates a store holding n recorded sessions.
func corpusWith(t *testing.T, agent string, n int) (corpus.Store, []string) {
	t.Helper()
	store, err := corpus.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var sessions []string
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("sess-%03d", i)
		w, err := store.Create(context.Background(), cassette.Header{
			SessionID: id, Agent: agent, RecordedAt: time.Now(),
		})
		if err != nil {
			t.Fatal(err)
		}
		w.Close()
		sessions = append(sessions, id)
	}
	return store, sessions
}

// fakeReplayer returns a clean report for every session unless told otherwise.
type fakeReplayer struct {
	unusable map[string]bool
	noOps    int
	err      error
}

func (f *fakeReplayer) Replay(_ context.Context, session string,
	_ *v1alpha1.AgentManifestSpec, arm replay.ArmLabel) (*replay.Report, error) {
	if f.err != nil {
		return nil, f.err
	}
	r := &replay.Report{
		Session: session,
		Steps:   []replay.Step{{Index: 0}},
		NoOps:   f.noOps,
	}
	if f.unusable[session] {
		r.Refused = 1
	}
	return r, nil
}

// fakeScorer scores each arm with a fixed effect plus item-level noise, which
// is the shape real evaluation takes.
type fakeScorer struct {
	effect  float64
	seed    int64
	missing map[string]string // item -> arm that fails to score
	err     error
	seen    []score.Ref
}

func (f *fakeScorer) Score(_ context.Context, refs []score.Ref) ([]score.Result, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.seen = append(f.seen, refs...)

	rng := rand.New(rand.NewSource(f.seed))
	base := map[string]float64{}
	var out []score.Result
	for _, ref := range refs {
		if _, ok := base[ref.Item]; !ok {
			base[ref.Item] = rng.Float64()
		}
		if f.missing[ref.Item] == ref.Arm {
			out = append(out, score.Result{Item: ref.Item, Arm: ref.Arm, Error: "judge timed out"})
			continue
		}
		value := base[ref.Item]
		if ref.Arm == string(replay.ArmCandidate) {
			value += f.effect
		}
		out = append(out, score.Result{
			Item: ref.Item, Arm: ref.Arm,
			Metrics: map[string]float64{"task-completion": value, "policy-violations": 0},
		})
	}
	return out, nil
}

func stage(margin float64) v1alpha1.Stage {
	return v1alpha1.Stage{
		Name: "replay", Mode: v1alpha1.StageOfflineReplay,
		Corpus: v1alpha1.CorpusSelector{Ref: "prod"},
		Gate: v1alpha1.Gate{
			Primary: v1alpha1.GateMetric{
				Metric: "task-completion", Test: v1alpha1.GatePairedBootstrap,
				Margin: ptr(margin), Alpha: ptr(0.05), Direction: v1alpha1.HigherIsBetter,
			},
		},
	}
}

func runner(t *testing.T, store corpus.Store, rep rollout.Replayer, sc score.Scorer) *rollout.Runner {
	t.Helper()
	return &rollout.Runner{
		Corpus: store, Replayer: rep, Scorer: sc,
		Analyzer:     &analysis.PairedBootstrap{Resamples: 2000},
		OutputCorpus: t.TempDir(),
	}
}

func TestEquivalentCandidateIsPromoted(t *testing.T) {
	store, _ := corpusWith(t, "support-agent", 60)
	r := runner(t, store, &fakeReplayer{}, &fakeScorer{effect: 0, seed: 1})

	got, err := r.RunOfflineReplay(context.Background(), stage(-0.02),
		manifest("support-agent", "sha256:a"), manifest("support-agent", "sha256:b"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Verdict.Outcome != analysis.OutcomePromote {
		t.Errorf("outcome = %s: %s", got.Verdict.Outcome, got.Verdict.Reason)
	}
	if got.Scored != 60 {
		t.Errorf("scored = %d, want 60", got.Scored)
	}
}

func TestRegressionIsWavedOff(t *testing.T) {
	store, _ := corpusWith(t, "support-agent", 60)
	r := runner(t, store, &fakeReplayer{}, &fakeScorer{effect: -0.2, seed: 2})

	got, err := r.RunOfflineReplay(context.Background(), stage(-0.02),
		manifest("support-agent", "sha256:a"), manifest("support-agent", "sha256:b"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Verdict.Outcome != analysis.OutcomeWaveOff {
		t.Errorf("a 20-point regression was not waved off: %s", got.Verdict.Reason)
	}
}

// TestBothArmsReplayTheSameSessions is what makes the comparison paired, and
// pairing is what makes the sample size manageable. It is a property of how the
// stage runs, not an option.
func TestBothArmsReplayTheSameSessions(t *testing.T) {
	store, _ := corpusWith(t, "support-agent", 10)
	sc := &fakeScorer{effect: 0, seed: 3}
	r := runner(t, store, &fakeReplayer{}, sc)

	if _, err := r.RunOfflineReplay(context.Background(), stage(-0.02),
		manifest("support-agent", "sha256:a"), manifest("support-agent", "sha256:b")); err != nil {
		t.Fatal(err)
	}

	byItem := map[string]map[string]bool{}
	for _, ref := range sc.seen {
		if byItem[ref.Item] == nil {
			byItem[ref.Item] = map[string]bool{}
		}
		byItem[ref.Item][ref.Arm] = true
	}
	if len(byItem) != 10 {
		t.Fatalf("items = %d, want 10", len(byItem))
	}
	for item, arms := range byItem {
		if !arms["incumbent"] || !arms["candidate"] {
			t.Errorf("%s was not replayed under both arms: %v", item, arms)
		}
	}
}

// TestAnUnusableArmDisqualifiesThePair: a paired test needs both sides of the
// same item, so half a pair is not a measurement.
func TestAnUnusableArmDisqualifiesThePair(t *testing.T) {
	store, sessions := corpusWith(t, "support-agent", 60)
	rep := &fakeReplayer{unusable: map[string]bool{sessions[0]: true, sessions[1]: true}}
	sc := &fakeScorer{effect: 0, seed: 4}
	r := runner(t, store, rep, sc)

	got, err := r.RunOfflineReplay(context.Background(), stage(-0.02),
		manifest("support-agent", "sha256:a"), manifest("support-agent", "sha256:b"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Excluded < 2 {
		t.Errorf("excluded = %d, want at least 2", got.Excluded)
	}
	if got.Scored != 58 {
		t.Errorf("scored = %d, want 58", got.Scored)
	}
	// A half-pair must never reach the scorer.
	for _, ref := range sc.seen {
		if ref.Item == sessions[0] || ref.Item == sessions[1] {
			t.Errorf("an excluded item was sent for scoring: %s/%s", ref.Item, ref.Arm)
		}
	}
	// And the drop must be explained, or a thin result looks merely small.
	if len(got.Exclusions) == 0 {
		t.Error("exclusions were counted but not explained")
	}
}

// TestAHalfScoredItemIsDroppedNotImputed.
func TestAHalfScoredItemIsDroppedNotImputed(t *testing.T) {
	store, sessions := corpusWith(t, "support-agent", 60)
	sc := &fakeScorer{
		effect: 0, seed: 5,
		missing: map[string]string{sessions[0]: "candidate"},
	}
	r := runner(t, store, &fakeReplayer{}, sc)

	got, err := r.RunOfflineReplay(context.Background(), stage(-0.02),
		manifest("support-agent", "sha256:a"), manifest("support-agent", "sha256:b"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Scored != 59 {
		t.Errorf("scored = %d, want 59", got.Scored)
	}
	if got.Verdict.Missing.CandidateOnlyFailed != 1 {
		t.Errorf("the verdict does not record which arm failed to score: %+v", got.Verdict.Missing)
	}
}

// TestAsymmetricScoringFailureBlocksPromotion is the end-to-end version of the
// bias path: a candidate whose output the judge chokes on more often would
// otherwise be promoted on the subset where it behaved.
func TestAsymmetricScoringFailureBlocksPromotion(t *testing.T) {
	store, sessions := corpusWith(t, "support-agent", 60)

	// Eight of sixty is a 13% drop rate, comfortably under the ceiling, so
	// this exercises the asymmetry test rather than the volume one: every
	// failure is on the candidate's side, and the candidate looks excellent on
	// what survives.
	failing := map[string]string{}
	for i := 0; i < 8; i++ {
		failing[sessions[i]] = string(replay.ArmCandidate)
	}
	sc := &fakeScorer{effect: 0.20, seed: 30, missing: failing}
	r := runner(t, store, &fakeReplayer{}, sc)

	got, err := r.RunOfflineReplay(context.Background(), stage(-0.02),
		manifest("support-agent", "sha256:a"), manifest("support-agent", "sha256:b"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Verdict.Outcome == analysis.OutcomePromote {
		t.Fatalf("a candidate was promoted on the subset the judge could score: %s", got.Verdict.Reason)
	}
	if got.Missing.CandidateOnlyFailed != 8 {
		t.Errorf("the drop pattern was not attributed to the right arm: %+v", got.Missing)
	}
	if !strings.Contains(got.Verdict.Reason, "asymmetry") {
		t.Errorf("the block should be attributed to the asymmetry, not the volume: %q", got.Verdict.Reason)
	}
	if !strings.Contains(got.Verdict.Reason, "candidate") {
		t.Errorf("the reason should name the arm that failed to score: %q", got.Verdict.Reason)
	}
}

// TestAnUncalibratedJudgeStopsTheStageBeforeAnythingRuns.
//
// Discovering the instrument was uncalibrated after the whole comparison has
// run wastes the run and, worse, produces a verdict somebody might act on.
func TestAnUncalibratedJudgeStopsTheStageBeforeAnythingRuns(t *testing.T) {
	store, _ := corpusWith(t, "support-agent", 60)
	sc := &fakeScorer{effect: 0, seed: 50}
	r := runner(t, store, &fakeReplayer{}, sc)

	low := 0.2
	candidate := manifest("support-agent", "sha256:b")
	candidate.Judges = []v1alpha1.JudgeSpec{{
		Name: "task-completion", Model: "claude-opus-5",
		RubricDigest: "sha256:" + strings.Repeat("a", 64),
		Calibration:  &v1alpha1.JudgeCalibration{Kappa: &low},
	}}

	_, err := r.RunOfflineReplay(context.Background(), stage(-0.02),
		manifest("support-agent", "sha256:a"), candidate)
	if err == nil {
		t.Fatal("a stage ran on a judge below the agreement floor")
	}
	if !strings.Contains(err.Error(), "κ") {
		t.Errorf("err = %v", err)
	}
	// Nothing should have been scored: the check runs first.
	if len(sc.seen) != 0 {
		t.Errorf("%d item(s) were scored before the judge check", len(sc.seen))
	}
}

// TestScorerFailureStopsTheRollout: an infrastructure failure is not a verdict,
// and must never produce an absence of scores a gate reads as a pass.
func TestScorerFailureStopsTheRollout(t *testing.T) {
	store, _ := corpusWith(t, "support-agent", 10)
	r := runner(t, store, &fakeReplayer{}, &fakeScorer{err: errors.New("judge unreachable")})

	_, err := r.RunOfflineReplay(context.Background(), stage(-0.02),
		manifest("support-agent", "sha256:a"), manifest("support-agent", "sha256:b"))
	if err == nil {
		t.Fatal("a failing scorer produced a result")
	}
	if !strings.Contains(err.Error(), "judge unreachable") {
		t.Errorf("err = %v", err)
	}
}

// TestAnalyzerFailureStopsTheRollout: the controller holds and pages rather
// than promoting. Failing open ships the exact change nobody could check.
func TestAnalyzerFailureStopsTheRollout(t *testing.T) {
	store, _ := corpusWith(t, "support-agent", 10)
	r := runner(t, store, &fakeReplayer{}, &fakeScorer{effect: 0, seed: 6})
	r.Analyzer = &failingAnalyzer{}

	_, err := r.RunOfflineReplay(context.Background(), stage(-0.02),
		manifest("support-agent", "sha256:a"), manifest("support-agent", "sha256:b"))
	if err == nil {
		t.Fatal("an unreachable analyzer produced a result")
	}
}

// TestAnEmptyCorpusIsAnError: a gate with nothing to measure cannot decide
// anything, and must say so rather than pass by default.
func TestAnEmptyCorpusIsAnError(t *testing.T) {
	store, _ := corpusWith(t, "another-agent", 5)
	r := runner(t, store, &fakeReplayer{}, &fakeScorer{})

	_, err := r.RunOfflineReplay(context.Background(), stage(-0.02),
		manifest("support-agent", "sha256:a"), manifest("support-agent", "sha256:b"))
	if err == nil {
		t.Fatal("an empty corpus produced a verdict")
	}
	if !strings.Contains(err.Error(), "nothing to measure") {
		t.Errorf("err = %v", err)
	}
}

// TestDegradationReachesTheScorer: a score over a session where writes were
// synthesised means something different, and a scorer that cannot see the
// difference reports both the same.
func TestDegradationReachesTheScorer(t *testing.T) {
	store, _ := corpusWith(t, "support-agent", 10)
	sc := &fakeScorer{effect: 0, seed: 7}
	r := runner(t, store, &fakeReplayer{noOps: 3}, sc)

	if _, err := r.RunOfflineReplay(context.Background(), stage(-0.02),
		manifest("support-agent", "sha256:a"), manifest("support-agent", "sha256:b")); err != nil {
		t.Fatal(err)
	}
	for _, ref := range sc.seen {
		if !ref.Degraded || ref.Reason == "" {
			t.Fatalf("degradation was not passed to the scorer: %+v", ref)
			return
		}
	}
}

// TestGuardrailIsDecisive at the stage level.
func TestGuardrailIsDecisive(t *testing.T) {
	store, _ := corpusWith(t, "support-agent", 60)
	sc := &violatingScorer{}
	r := runner(t, store, &fakeReplayer{}, sc)

	s := stage(-0.02)
	s.Gate.Guardrails = []v1alpha1.GateMetric{{
		Metric: "policy-violations", Test: v1alpha1.GateThreshold,
		Threshold: ptr(0.0), Direction: v1alpha1.LowerIsBetter,
	}}

	got, err := r.RunOfflineReplay(context.Background(), s,
		manifest("support-agent", "sha256:a"), manifest("support-agent", "sha256:b"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Verdict.Outcome != analysis.OutcomeWaveOff {
		t.Errorf("a policy violation did not wave the candidate off: %s", got.Verdict.Reason)
	}
}

type failingAnalyzer struct{}

func (f *failingAnalyzer) Name() string { return "failing" }
func (f *failingAnalyzer) Analyze(context.Context, analysis.Request) (analysis.Verdict, error) {
	return analysis.Verdict{}, errors.New("analyzer unreachable")
}

// violatingScorer makes the candidate better on the primary metric but leaves
// one policy violation behind.
type violatingScorer struct{ n int }

func (v *violatingScorer) Score(_ context.Context, refs []score.Ref) ([]score.Result, error) {
	var out []score.Result
	for _, ref := range refs {
		violations := 0.0
		value := 0.5
		if ref.Arm == string(replay.ArmCandidate) {
			value = 0.9
			if ref.Item == "sess-003" {
				violations = 1
			}
		}
		out = append(out, score.Result{
			Item: ref.Item, Arm: ref.Arm,
			Metrics: map[string]float64{"task-completion": value, "policy-violations": violations},
		})
	}
	return out, nil
}
