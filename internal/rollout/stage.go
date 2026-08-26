// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

// Package rollout runs the stages of an AgentRollout and turns their results
// into a promotion decision.
package rollout

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/waveoffai/waveoff/api/v1alpha1"
	"github.com/waveoffai/waveoff/internal/analysis"
	"github.com/waveoffai/waveoff/internal/corpus"
	"github.com/waveoffai/waveoff/internal/replay"
	"github.com/waveoffai/waveoff/internal/score"
)

// The rollout controller reads manifests and corpora, and writes rollout
// status. It creates nothing and deletes nothing.
// +kubebuilder:rbac:groups=waveoff.ai,resources=agentrollouts,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=waveoff.ai,resources=agentrollouts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
//
// Deliberately no access to Secrets. Scorer credentials are given to the
// manager at deploy time, from its own environment. Granting a controller that
// already mutates every pod the ability to read every Secret in the cluster is
// a large privilege increase to save an operator one mount.

// Replayer runs one recorded session against one manifest and returns both the
// replay report and where its output was written.
//
// Injected so a stage can be tested without standing up a replay server, and so
// the controller does not have to know how a replay is driven.
type Replayer interface {
	Replay(ctx context.Context, session string, spec *v1alpha1.AgentManifestSpec,
		arm replay.ArmLabel) (*replay.Report, error)
}

// StageResult is what running one stage produced.
type StageResult struct {
	Name string

	// Sessions is how many corpus items were attempted, Scored how many
	// produced a usable measurement under both arms, Excluded how many were
	// dropped and why.
	Sessions int
	Scored   int
	Excluded int
	// Exclusions explains each drop, so a thin result is legible rather than
	// merely small.
	Exclusions []string
	// Missing is the drop pattern the analyzer was given.
	Missing analysis.Missingness
	// JudgeCheck records whether the judges were trusted to decide at all.
	JudgeCheck JudgeCheck

	Verdict analysis.Verdict
	Started time.Time
	Ended   time.Time
}

// Runner executes a stage.
type Runner struct {
	Corpus   corpus.Store
	Replayer Replayer
	Scorer   score.Scorer
	Analyzer analysis.Analyzer

	// OutputCorpus is where replay outputs are written, and what the scorer is
	// pointed at.
	OutputCorpus string
	BlobDir      string
}

// RunOfflineReplay replays a corpus against both arms, scores the results and
// asks the analyzer.
//
// Both arms replay the same sessions, in the same order, from the same
// recordings. That is what makes the comparison paired, and pairing is what
// makes the sample size manageable — so it is a property of how the stage runs,
// not an option.
func (r *Runner) RunOfflineReplay(ctx context.Context, stage v1alpha1.Stage,
	incumbent, candidate *v1alpha1.AgentManifestSpec) (*StageResult, error) {

	result := &StageResult{Name: stage.Name, Started: time.Now()}
	defer func() { result.Ended = time.Now() }()

	// Gate the gate, before anything is measured.
	//
	// Every number below comes from a judge. Running the whole comparison and
	// then discovering the instrument was uncalibrated wastes the run and,
	// worse, produces a verdict somebody might act on.
	floor := 0.0
	if stage.Gate.KappaFloor != nil {
		floor = *stage.Gate.KappaFloor
	}
	if jc := CheckJudges(incumbent, candidate, floor); !jc.Pass {
		result.JudgeCheck = jc
		return result, fmt.Errorf("%s", jc.Reason)
	} else {
		result.JudgeCheck = jc
	}

	sessions, err := r.selectSessions(ctx, stage, incumbent)
	if err != nil {
		return nil, err
	}
	if len(sessions) == 0 {
		return nil, fmt.Errorf("corpus %q has no sessions for agent %q; "+
			"a gate with nothing to measure cannot decide anything", stage.Corpus.Ref, incumbent.Agent)
	}
	result.Sessions = len(sessions)

	var refs []score.Ref
	for _, session := range sessions {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		usable := true
		for _, arm := range []struct {
			label replay.ArmLabel
			spec  *v1alpha1.AgentManifestSpec
		}{
			{replay.ArmIncumbent, incumbent},
			{replay.ArmCandidate, candidate},
		} {
			report, err := r.Replayer.Replay(ctx, session, arm.spec, arm.label)
			if err != nil {
				return nil, fmt.Errorf("replaying %s as %s: %w", session, arm.label, err)
			}
			ok, why := report.Usable()
			if !ok {
				// One arm being unusable disqualifies the pair, not just that
				// arm: a paired test needs both sides of the same item, and
				// half a pair is not a measurement.
				usable = false
				result.Exclusions = append(result.Exclusions,
					fmt.Sprintf("%s (%s): %s", session, arm.label, why))
				break
			}
			refs = append(refs, score.Ref{
				Item:     session,
				Arm:      string(arm.label),
				Session:  replay.OutputName(session, arm.label),
				Corpus:   r.OutputCorpus,
				Blobs:    r.BlobDir,
				Manifest: arm.spec.BehaviorDigest,
				Degraded: report.NoOps > 0,
				Reason:   degradation(report),
			})
		}
		if !usable {
			result.Excluded++
			// Drop the arm already queued for this item, so a half-pair never
			// reaches the scorer.
			refs = dropItem(refs, session)
		}
	}

	if len(refs) == 0 {
		return nil, fmt.Errorf("every session was excluded, so nothing was measured:\n  %s",
			joinLimited(result.Exclusions, 5))
	}

	results, err := r.Scorer.Score(ctx, refs)
	if err != nil {
		// A scoring failure is an infrastructure problem, not a verdict. It
		// must stop the rollout rather than produce an absence of scores a
		// gate could read as a pass.
		return nil, fmt.Errorf("scoring: %w", err)
	}
	if err := score.Validate(results); err != nil {
		return nil, err
	}

	req, dropped := buildRequest(stage, incumbent, candidate, results, len(sessions))
	result.Scored = len(req.Observations)
	result.Excluded += len(dropped)
	result.Missing = req.Missing
	for _, item := range dropped {
		result.Exclusions = append(result.Exclusions, item+": scored under only one arm")
	}

	verdict, err := r.Analyzer.Analyze(ctx, req)
	if err != nil {
		// The controller must hold and page rather than promote. Failing open
		// on a promotion decision ships the exact change nobody could check.
		return nil, fmt.Errorf("analyzer: %w", err)
	}
	result.Verdict = verdict
	return result, nil
}

// buildRequest turns scorer output into the paired form an analyzer consumes.
func buildRequest(stage v1alpha1.Stage, incumbent, candidate *v1alpha1.AgentManifestSpec,
	results []score.Result, attempted int) (analysis.Request, []string) {

	primary := toMetric(stage.Gate.Primary)
	pairs, dropped := score.Pairs(results, primary.Name)

	req := analysis.Request{
		Agent:     incumbent.Agent,
		Incumbent: incumbent.BehaviorDigest,
		Candidate: candidate.BehaviorDigest,
		Primary:   primary,
		Missing:   missingnessOf(results, pairs, attempted),
		// A fixed seed makes the resampling reproducible. The verdict carries
		// the observations as well, because the scores themselves come from a
		// judge that is not reproducible.
		Seed: 1,
	}
	for _, g := range stage.Gate.Guardrails {
		req.Guardrails = append(req.Guardrails, toMetric(g))
	}
	for _, p := range pairs {
		req.Observations = append(req.Observations, analysis.Observation{
			Item:      p.Item,
			Incumbent: p.Incumbent.Metrics,
			Candidate: p.Candidate.Metrics,
		})
	}
	return req, dropped
}

// missingnessOf works out which arm failed to score each item.
//
// The discordant counts are what carry evidence of asymmetry in paired data:
// items both arms scored, or neither did, say nothing about which arm is harder
// to judge. Items one arm scored and the other did not say everything.
func missingnessOf(results []score.Result, pairs []score.Pair, attempted int) analysis.Missingness {
	type arms struct{ incumbent, candidate, sawIncumbent, sawCandidate bool }
	byItem := map[string]*arms{}
	for _, r := range results {
		a, ok := byItem[r.Item]
		if !ok {
			a = &arms{}
			byItem[r.Item] = a
		}
		switch r.Arm {
		case string(replay.ArmIncumbent):
			a.sawIncumbent = true
			a.incumbent = r.Scored()
		case string(replay.ArmCandidate):
			a.sawCandidate = true
			a.candidate = r.Scored()
		}
	}

	m := analysis.Missingness{Attempted: attempted, BothScored: len(pairs)}
	for _, a := range byItem {
		switch {
		case a.incumbent && a.candidate:
			// Counted via pairs.
		case a.incumbent && !a.candidate:
			m.CandidateOnlyFailed++
		case !a.incumbent && a.candidate:
			m.IncumbentOnlyFailed++
		default:
			m.BothFailed++
		}
	}
	// Items that never reached the scorer at all — excluded during replay —
	// are unscored under both arms.
	if unreached := attempted - len(byItem); unreached > 0 {
		m.BothFailed += unreached
	}
	return m
}

func toMetric(g v1alpha1.GateMetric) analysis.Metric {
	m := analysis.Metric{
		Name:      g.Metric,
		Test:      analysis.Test(g.Test),
		Direction: analysis.Direction(g.Direction),
	}
	if m.Direction == "" {
		m.Direction = analysis.Higher
	}
	if g.Margin != nil {
		m.Margin = *g.Margin
	}
	if g.Threshold != nil {
		m.Threshold = *g.Threshold
	}
	if g.Alpha != nil {
		m.Alpha = *g.Alpha
	}
	return m
}

func (r *Runner) selectSessions(ctx context.Context, stage v1alpha1.Stage,
	spec *v1alpha1.AgentManifestSpec) ([]string, error) {

	if len(stage.Corpus.Sessions) > 0 {
		return stage.Corpus.Sessions, nil
	}
	headers, err := r.Corpus.List(ctx, corpus.Filter{
		Agent: spec.Agent,
		Limit: stage.Corpus.Limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(headers))
	for _, h := range headers {
		out = append(out, h.SessionID)
	}
	// Stable order, so re-running a stage replays the same items in the same
	// sequence and a disagreement is a real one.
	sort.Strings(out)
	return out, nil
}

func degradation(report *replay.Report) string {
	if report.NoOps == 0 {
		return ""
	}
	return fmt.Sprintf("%d write(s) refused and synthesised", report.NoOps)
}

func dropItem(refs []score.Ref, item string) []score.Ref {
	out := refs[:0]
	for _, r := range refs {
		if r.Item != item {
			out = append(out, r)
		}
	}
	return out
}

func joinLimited(items []string, max int) string {
	if len(items) > max {
		return fmt.Sprintf("%s\n  … and %d more", joinAll(items[:max]), len(items)-max)
	}
	return joinAll(items)
}

func joinAll(items []string) string {
	out := ""
	for i, s := range items {
		if i > 0 {
			out += "\n  "
		}
		out += s
	}
	return out
}
