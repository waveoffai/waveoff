// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package rollout

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/waveoffai/waveoff/api/v1alpha1"
	"github.com/waveoffai/waveoff/internal/analysis"
	"github.com/waveoffai/waveoff/internal/cas"
	"github.com/waveoffai/waveoff/internal/corpus"
	"github.com/waveoffai/waveoff/internal/replay"
	"github.com/waveoffai/waveoff/internal/score"
	"github.com/waveoffai/waveoff/internal/traffic"
)

// Event reasons. The vocabulary is deliberate: a failed gate *waves off* a
// candidate. The wave-off is the decision and the rollback is the
// implementation, and an operator reading an event stream should be able to
// tell which one they are looking at.
const (
	EventWavedOff     = "WavedOff"
	EventPromoted     = "Promoted"
	EventHeld         = "Held"
	EventStageStarted = "StageStarted"
)

// Reconciler runs AgentRollouts.
type Reconciler struct {
	client.Client
	Recorder record.EventRecorder

	// CorpusRoot is where recorded corpora live, and OutputRoot where replay
	// outputs are written.
	CorpusRoot string
	OutputRoot string
	BlobDir    string

	// ModelUpstream is the provider replays run the model against.
	ModelUpstream string

	// Observations supplies the measurements a live or shadow stage gathers.
	//
	// Normally nil: the controller builds a CorpusObservations from the
	// stage's own corpus selector and scorer spec, so a live stage is measured
	// through exactly the path an offline one is. Set it to measure from
	// somewhere else — an eval vendor's store, an existing pipeline — and the
	// gate reads whatever it returns.
	Observations ObservationSource

	// StageTimeout bounds one stage. A gate that never finishes is a rollout
	// that never resolves, which in practice means a candidate that ships
	// because somebody got tired of waiting.
	StageTimeout time.Duration
}

// +kubebuilder:rbac:groups=waveoff.ai,resources=agentrollouts,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=waveoff.ai,resources=agentrollouts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=waveoff.ai,resources=agentmanifests,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// SetupWithManager registers the controller.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("waveoff-rollout")
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.AgentRollout{}).
		Complete(r)
}

// Reconcile advances a rollout by one stage.
//
// One stage per pass, rather than the whole rollout in a loop. A stage replays
// an entire corpus and can take minutes, and doing them all in one reconcile
// would hold a worker for the duration and lose everything already decided if
// the manager restarted in the middle.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var rollout v1alpha1.AgentRollout
	if err := r.Get(ctx, req.NamespacedName, &rollout); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Terminal rollouts are left alone. A promoted or waved-off candidate is a
	// decision that was made, and re-running it would quietly replace evidence
	// somebody may already have acted on.
	if terminal(rollout.Status.Phase) && rollout.Status.ObservedGeneration == rollout.Generation {
		return ctrl.Result{}, nil
	}
	if rollout.Status.ObservedGeneration != rollout.Generation {
		// The spec changed: start again rather than continue against results
		// that describe a different comparison.
		rollout.Status = v1alpha1.AgentRolloutStatus{ObservedGeneration: rollout.Generation}
	}

	incumbent, candidate, err := r.manifests(ctx, &rollout)
	if err != nil {
		return r.hold(ctx, &rollout, fmt.Sprintf("cannot load the manifests under comparison: %v", err))
	}

	next := nextStage(&rollout)
	if next < 0 {
		return r.finish(ctx, &rollout, v1alpha1.PhasePromoted,
			"every stage passed", EventPromoted)
	}
	stage := rollout.Spec.Stages[next]

	// A live or shadow stage is a thing that is happening rather than a batch
	// job, so it advances a step per reconcile and its progress lives in
	// status — which is what makes a manager restart mid-canary survivable.
	if stage.Mode == v1alpha1.StageLive || stage.Mode == v1alpha1.StageShadow {
		return r.advanceLive(ctx, &rollout, next, stage, incumbent, candidate)
	}

	logger.Info("running stage", "stage", stage.Name, "mode", stage.Mode)
	r.Recorder.Eventf(&rollout, corev1.EventTypeNormal, EventStageStarted,
		"stage %q started (%s)", stage.Name, stage.Mode)

	if err := r.setRunning(ctx, &rollout, stage.Name); err != nil {
		return ctrl.Result{}, err
	}

	result, runErr := r.runStage(ctx, &rollout, stage, incumbent, candidate)
	if runErr != nil {
		// A stage that could not run is not a stage that failed. Holding and
		// saying why is the only safe answer: promoting would ship the exact
		// change nobody was able to check.
		return r.hold(ctx, &rollout, fmt.Sprintf("stage %q could not run: %v", stage.Name, runErr))
	}

	status := v1alpha1.StageStatus{
		Name:       stage.Name,
		Sessions:   result.Sessions,
		Scored:     result.Scored,
		Excluded:   result.Excluded,
		Verdict:    string(result.Verdict.Outcome),
		Reason:     result.Verdict.Reason,
		Analyzer:   result.Verdict.Analyzer,
		StartedAt:  &metav1.Time{Time: result.Started},
		FinishedAt: &metav1.Time{Time: result.Ended},
	}

	switch result.Verdict.Outcome {
	case analysis.OutcomePromote:
		status.Phase = v1alpha1.PhasePromoted
		r.recordStage(&rollout, status)
		if next == len(rollout.Spec.Stages)-1 {
			return r.finish(ctx, &rollout, v1alpha1.PhasePromoted, result.Verdict.Reason, EventPromoted)
		}
		// More stages to run.
		if err := r.writeStatus(ctx, &rollout, v1alpha1.PhaseRunning, result.Verdict.Reason); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil

	case analysis.OutcomeWaveOff:
		status.Phase = v1alpha1.PhaseWavedOff
		r.recordStage(&rollout, status)
		return r.finish(ctx, &rollout, v1alpha1.PhaseWavedOff, result.Verdict.Reason, EventWavedOff)

	default:
		status.Phase = v1alpha1.PhaseHeld
		r.recordStage(&rollout, status)
		return r.finish(ctx, &rollout, v1alpha1.PhaseHeld, result.Verdict.Reason, EventHeld)
	}
}

// advanceLive moves a live or shadow stage on by one step.
func (r *Reconciler) advanceLive(ctx context.Context, rollout *v1alpha1.AgentRollout,
	index int, stage v1alpha1.Stage, incumbent, candidate *v1alpha1.AgentManifestSpec) (ctrl.Result, error) {

	logger := log.FromContext(ctx)

	// Resolve where the measurements come from before touching the traffic
	// configuration. A stage with nothing to measure is held whatever the
	// router says, and finding that out first gives the operator the
	// informative failure rather than a networking one.
	scorer, err := buildScorer(stage.Scorer)
	if err != nil {
		return r.hold(ctx, rollout, err.Error())
	}
	observations, activity, err := r.liveSources(stage, scorer)
	if err != nil {
		return r.hold(ctx, rollout, err.Error())
	}

	router, err := r.routerFor(stage)
	if err != nil {
		return r.hold(ctx, rollout, err.Error())
	}
	// A live stage must be gated by an anytime-valid test. The CRD enforces
	// this too; checking again here means a rollout created before that rule
	// existed cannot slip through.
	if stage.Mode == v1alpha1.StageLive && stage.Gate.Primary.Test != v1alpha1.GateSequential {
		return r.hold(ctx, rollout, fmt.Sprintf(
			"stage %q is live and gated by %q; a canary is watched continuously and a fixed-horizon "+
				"test is not valid under that", stage.Name, stage.Gate.Primary.Test))
	}

	// And it must keep a session on one arm. Weights are per request, so
	// without stickiness a multi-turn session lands on both arms across its
	// turns: the measurement attributes to one arm work the other partly did,
	// and an agent with any session state simply breaks.
	if stage.Mode == v1alpha1.StageLive {
		sticky, err := router.Stickiness(ctx, targetFor(rollout, stage))
		if err != nil {
			return r.hold(ctx, rollout, fmt.Sprintf("stage %q: cannot determine session affinity: %v",
				stage.Name, err))
		}
		if sticky != traffic.StickyBySession {
			return r.hold(ctx, rollout, fmt.Sprintf(
				"stage %q routes live traffic without session affinity. Weights are evaluated per "+
					"request, so a multi-turn session would be served by both arms — the comparison "+
					"would attribute to one arm what the other partly produced. Configure the route "+
					"to hash on %s and try again", stage.Name, traffic.SessionHeader))
		}
	}

	live := &LiveRunner{
		Router:       router,
		Scorer:       scorer,
		Analyzer:     buildLiveAnalyzer(rollout.Spec.Analyzer),
		Observations: observations,
		Activity:     activity,
	}

	status, started := r.stageStatus(rollout, stage.Name)
	if !started {
		// First pass: put the traffic configuration in place before anything
		// is measured.
		if err := live.Enter(ctx, rollout, stage); err != nil {
			return r.hold(ctx, rollout, fmt.Sprintf("stage %q could not be started: %v", stage.Name, err))
		}
		now := metav1.Now()
		status = v1alpha1.StageStatus{Name: stage.Name, Phase: v1alpha1.PhaseRunning, StartedAt: &now}
		r.recordStage(rollout, status)
		r.Recorder.Eventf(rollout, corev1.EventTypeNormal, EventStageStarted,
			"stage %q started (%s)", stage.Name, stage.Mode)
		if err := r.writeStatus(ctx, rollout, v1alpha1.PhaseRunning, "running stage "+stage.Name); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: DefaultPollInterval}, nil
	}

	step, err := live.Advance(ctx, rollout, stage, incumbent, candidate, status.StartedAt.Time)
	if err != nil {
		return r.hold(ctx, rollout, fmt.Sprintf("stage %q could not be advanced: %v", stage.Name, err))
	}
	logger.Info("canary step", "stage", stage.Name, "outcome", step.Outcome,
		"weight", step.Weight, "reason", step.Reason)

	status.Weight = step.Weight
	status.Reason = step.Reason
	status.Verdict = string(step.Verdict.Outcome)
	status.Analyzer = step.Verdict.Analyzer
	status.Scored = step.Verdict.N
	status.WriteActivity = writeActivityStatus(step.Activity)

	switch step.Outcome {
	case StepRollBack:
		// Withdraw first, then record. If the process dies between the two, a
		// candidate that is no longer serving and a status that says it is
		// still running is a far better failure than the reverse.
		if err := live.Withdraw(ctx, rollout, stage); err != nil {
			return r.hold(ctx, rollout, fmt.Sprintf(
				"stage %q failed its gate and could not be withdrawn — the candidate may still be "+
					"serving traffic: %v", stage.Name, err))
		}
		status.Phase = v1alpha1.PhaseWavedOff
		status.RolledBack = true
		status.Trigger = step.Trigger
		finished := metav1.Now()
		status.FinishedAt = &finished
		r.recordStage(rollout, status)
		r.Recorder.Eventf(rollout, corev1.EventTypeWarning, EventWavedOff,
			"stage %q: %s (trigger %s); all traffic returned to the incumbent",
			stage.Name, step.Reason, step.Trigger)
		return ctrl.Result{}, r.writeStatus(ctx, rollout, v1alpha1.PhaseWavedOff, step.Reason)

	case StepPromote:
		status.Phase = v1alpha1.PhasePromoted
		finished := metav1.Now()
		status.FinishedAt = &finished
		r.recordStage(rollout, status)
		if index == len(rollout.Spec.Stages)-1 {
			return r.finish(ctx, rollout, v1alpha1.PhasePromoted, step.Reason, EventPromoted)
		}
		if err := r.writeStatus(ctx, rollout, v1alpha1.PhaseRunning, step.Reason); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil

	default:
		r.recordStage(rollout, status)
		if err := r.writeStatus(ctx, rollout, v1alpha1.PhaseRunning, step.Reason); err != nil {
			return ctrl.Result{}, err
		}
		if step.RequeueAfter <= 0 {
			// The stage is stalled deliberately — waved off with automatic
			// rollback disabled — and needs a human rather than a timer.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{RequeueAfter: step.RequeueAfter}, nil
	}
}

// routerFor builds the traffic router a stage asked for.
// writeActivityStatus renders a write-activity finding for the object.
//
// Reported whether or not it fired: a stage where nothing was attempted either
// had no write tools or was not actually watching, and without this those look
// identical from outside.
func writeActivityStatus(f *analysis.ActivityFinding) *v1alpha1.WriteActivityStatus {
	if f == nil {
		return nil
	}
	return &v1alpha1.WriteActivityStatus{
		IncumbentAttempts: fmt.Sprintf("%.2f/session", f.Incumbent.Rate()),
		CandidateAttempts: fmt.Sprintf("%.2f/session", f.Candidate.Rate()),
		NewClasses:        f.NewClasses,
		DroppedClasses:    f.DroppedClasses,
		IncumbentSessions: f.Incumbent.Sessions,
		CandidateSessions: f.Candidate.Sessions,
	}
}

// liveSources decides where a live or shadow stage's measurements come from.
//
// The default is the recorder's own corpus, read through the stage's scorer:
// the same sessions, the same selector and the same scorer spec an offline
// stage uses. Measuring a canary any other way compares two agents on two
// instruments, and the difference that turns up then belongs to the
// instruments.
func (r *Reconciler) liveSources(stage v1alpha1.Stage, scorer score.Scorer) (
	ObservationSource, ActivitySource, error) {

	if r.Observations != nil {
		activity, _ := r.Observations.(ActivitySource)
		return r.Observations, activity, nil
	}
	if stage.Corpus.Ref == "" {
		return nil, nil, fmt.Errorf(
			"stage %q has nothing to measure: set corpus.ref to the store the recorder writes "+
				"production sessions into, or configure an observation source. A canary nobody is "+
				"measuring is a candidate serving traffic on nothing but hope", stage.Name)
	}
	dir := r.corpusPath(stage.Corpus.Ref)
	store, err := corpus.NewFS(dir)
	if err != nil {
		return nil, nil, err
	}
	src := &CorpusObservations{
		Store:  store,
		Dir:    dir,
		Blobs:  r.BlobDir,
		Scorer: scorer,
		Limit:  stage.Corpus.Limit,
	}
	return src, src, nil
}

func (r *Reconciler) routerFor(stage v1alpha1.Stage) (traffic.Router, error) {
	if stage.Traffic == nil {
		return nil, fmt.Errorf("stage %q moves live traffic but has no traffic block", stage.Name)
	}
	switch stage.Traffic.Router {
	case "gateway-api":
		return &traffic.GatewayAPI{Client: r.Client}, nil
	case "istio":
		return &traffic.Istio{Client: r.Client}, nil
	}
	return nil, fmt.Errorf("stage %q asks for traffic router %q, which is not implemented",
		stage.Name, stage.Traffic.Router)
}

// buildLiveAnalyzer returns an anytime-valid analyzer, or a remote one.
func buildLiveAnalyzer(spec v1alpha1.AnalyzerSpec) analysis.Analyzer {
	if spec.Endpoint == "" {
		return &analysis.Sequential{}
	}
	return &analysis.Remote{
		Endpoint: spec.Endpoint,
		Timeout:  time.Duration(spec.TimeoutSeconds) * time.Second,
	}
}

// stageStatus returns the recorded status for a stage and whether it has begun.
func (r *Reconciler) stageStatus(rollout *v1alpha1.AgentRollout, name string) (v1alpha1.StageStatus, bool) {
	for _, s := range rollout.Status.Stages {
		if s.Name == name {
			return s, s.StartedAt != nil
		}
	}
	return v1alpha1.StageStatus{Name: name}, false
}

// runStage builds a runner for one stage and executes it.
func (r *Reconciler) runStage(ctx context.Context, rollout *v1alpha1.AgentRollout,
	stage v1alpha1.Stage, incumbent, candidate *v1alpha1.AgentManifestSpec) (*StageResult, error) {

	if stage.Mode != v1alpha1.StageOfflineReplay {
		return nil, fmt.Errorf("stage mode %q is not implemented", stage.Mode)
	}

	timeout := r.StageTimeout
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	recordings, err := corpus.NewFS(r.corpusPath(stage.Corpus.Ref))
	if err != nil {
		return nil, err
	}
	blobs, err := cas.NewFS(r.BlobDir)
	if err != nil {
		return nil, err
	}

	// Outputs are namespaced per rollout and generation, so a retry after a
	// manager restart does not collide with the half-written outputs of the
	// attempt it is replacing.
	outDir := filepath.Join(r.OutputRoot,
		fmt.Sprintf("%s-%s-%d", rollout.Namespace, rollout.Name, rollout.Generation))
	outputs, err := corpus.NewFS(outDir)
	if err != nil {
		return nil, err
	}

	scorer, err := buildScorer(stage.Scorer)
	if err != nil {
		return nil, err
	}
	analyzer := buildAnalyzer(rollout.Spec.Analyzer)

	runner := &Runner{
		Corpus: recordings,
		Replayer: &replay.Driver{
			Corpus: recordings, Blobs: blobs, Outputs: outputs,
			Mode:          replay.ModeModelLiveToolsReplayed,
			ModelUpstream: r.ModelUpstream,
		},
		Scorer:       scorer,
		Analyzer:     analyzer,
		OutputCorpus: outDir,
		BlobDir:      r.BlobDir,
	}
	return runner.RunOfflineReplay(ctx, stage, incumbent, candidate)
}

func buildScorer(spec v1alpha1.ScorerSpec) (score.Scorer, error) {
	timeout := time.Duration(spec.TimeoutSeconds) * time.Second
	switch {
	case spec.Exec != nil:
		return &score.ExecScorer{
			Command: spec.Exec.Command, Args: spec.Exec.Args, Timeout: timeout,
		}, nil
	case spec.HTTP != nil:
		return &score.HTTPScorer{Endpoint: spec.HTTP.Endpoint, Timeout: timeout}, nil
	}
	return nil, fmt.Errorf("no scorer configured: this gate has no source of measurements")
}

// buildAnalyzer returns the in-process analyzer, or a remote one when the
// rollout names an endpoint. The controller does not know which answered.
func buildAnalyzer(spec v1alpha1.AnalyzerSpec) analysis.Analyzer {
	if spec.Endpoint == "" {
		return &analysis.PairedBootstrap{}
	}
	return &analysis.Remote{
		Endpoint: spec.Endpoint,
		Timeout:  time.Duration(spec.TimeoutSeconds) * time.Second,
	}
}

func (r *Reconciler) manifests(ctx context.Context, rollout *v1alpha1.AgentRollout) (
	incumbent, candidate *v1alpha1.AgentManifestSpec, err error) {

	load := func(name string) (*v1alpha1.AgentManifestSpec, error) {
		var m v1alpha1.AgentManifest
		key := types.NamespacedName{Namespace: rollout.Namespace, Name: name}
		if err := r.Get(ctx, key, &m); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, fmt.Errorf("no AgentManifest %q in %s", name, rollout.Namespace)
			}
			return nil, err
		}
		return &m.Spec, nil
	}

	if incumbent, err = load(rollout.Spec.IncumbentRef); err != nil {
		return nil, nil, err
	}
	candidate, err = load(rollout.Spec.CandidateRef)
	return incumbent, candidate, err
}

// nextStage returns the index of the first stage that has not finished, or -1
// when every stage has passed.
func nextStage(rollout *v1alpha1.AgentRollout) int {
	done := map[string]bool{}
	for _, s := range rollout.Status.Stages {
		if s.Phase == v1alpha1.PhasePromoted {
			done[s.Name] = true
		}
	}
	for i, s := range rollout.Spec.Stages {
		if !done[s.Name] {
			return i
		}
	}
	return -1
}

func terminal(p v1alpha1.RolloutPhase) bool {
	switch p {
	case v1alpha1.PhasePromoted, v1alpha1.PhaseWavedOff, v1alpha1.PhaseHeld, v1alpha1.PhaseFailed:
		return true
	}
	return false
}

func (r *Reconciler) recordStage(rollout *v1alpha1.AgentRollout, status v1alpha1.StageStatus) {
	for i, s := range rollout.Status.Stages {
		if s.Name == status.Name {
			rollout.Status.Stages[i] = status
			return
		}
	}
	rollout.Status.Stages = append(rollout.Status.Stages, status)
}

func (r *Reconciler) setRunning(ctx context.Context, rollout *v1alpha1.AgentRollout, stage string) error {
	return r.writeStatus(ctx, rollout, v1alpha1.PhaseRunning, "running stage "+stage)
}

func (r *Reconciler) hold(ctx context.Context, rollout *v1alpha1.AgentRollout, reason string) (ctrl.Result, error) {
	r.Recorder.Event(rollout, corev1.EventTypeWarning, EventHeld, reason)
	if err := r.writeStatus(ctx, rollout, v1alpha1.PhaseHeld, reason); err != nil {
		return ctrl.Result{}, err
	}
	// No requeue. A held rollout needs a human, and retrying on a timer would
	// turn a page into a loop.
	return ctrl.Result{}, nil
}

func (r *Reconciler) finish(ctx context.Context, rollout *v1alpha1.AgentRollout,
	phase v1alpha1.RolloutPhase, reason, event string) (ctrl.Result, error) {

	kind := corev1.EventTypeNormal
	if phase != v1alpha1.PhasePromoted {
		kind = corev1.EventTypeWarning
	}
	r.Recorder.Event(rollout, kind, event, reason)
	return ctrl.Result{}, r.writeStatus(ctx, rollout, phase, reason)
}

func (r *Reconciler) writeStatus(ctx context.Context, rollout *v1alpha1.AgentRollout,
	phase v1alpha1.RolloutPhase, reason string) error {

	rollout.Status.Phase = phase
	rollout.Status.Reason = reason
	rollout.Status.ObservedGeneration = rollout.Generation
	return r.Status().Update(ctx, rollout)
}

func (r *Reconciler) corpusPath(ref string) string {
	if filepath.IsAbs(ref) {
		return ref
	}
	return filepath.Join(r.CorpusRoot, ref)
}
