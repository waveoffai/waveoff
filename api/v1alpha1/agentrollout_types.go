// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// StageMode is how a stage exercises the candidate.
// +kubebuilder:validation:Enum=offline-replay;shadow;live
type StageMode string

const (
	// StageOfflineReplay replays recorded sessions against the candidate.
	//
	// No production traffic is involved and no write ever executes, so this is
	// safe to run against a corpus recorded from production.
	StageOfflineReplay StageMode = "offline-replay"

	// StageShadow mirrors live traffic to the candidate with its writes
	// suppressed.
	//
	// The candidate sees real production requests, and nothing it produces
	// reaches a user or mutates anything. It is how a candidate meets the
	// traffic shapes a corpus never captured — without a single request
	// depending on it being right.
	StageShadow StageMode = "shadow"

	// StageLive routes a share of real traffic to the candidate.
	//
	// The only stage where a user's request is actually served by the
	// candidate, so it is the only one whose gate must be anytime-valid: a
	// canary is watched continuously, and a fixed-horizon test is not valid
	// under that.
	StageLive StageMode = "live"
)

// GateTest names a statistical procedure.
// +kubebuilder:validation:Enum=threshold;paired-bootstrap;sequential
type GateTest string

const (
	// GateThreshold compares against a fixed bound. Right for a guardrail,
	// wrong for anything noisy.
	GateThreshold GateTest = "threshold"
	// GatePairedBootstrap resamples per-item differences between the arms.
	// Fixed-horizon: valid when the result is read once, at the end.
	GatePairedBootstrap GateTest = "paired-bootstrap"
	// GateSequential is an anytime-valid confidence sequence.
	//
	// Required for a live canary, which is looked at continuously. A
	// fixed-horizon test peeked at repeatedly will eventually call a good
	// candidate a regression, and the controller does exactly that peeking.
	GateSequential GateTest = "sequential"
)

// MetricDirection says which way a metric is good.
// +kubebuilder:validation:Enum=higher-is-better;lower-is-better
type MetricDirection string

const (
	HigherIsBetter MetricDirection = "higher-is-better"
	LowerIsBetter  MetricDirection = "lower-is-better"
)

// GateMetric is one thing a gate tests.
type GateMetric struct {
	// Metric is the name the scorer reports. The vocabulary is yours.
	// +kubebuilder:validation:MinLength=1
	Metric string `json:"metric"`

	Test GateTest `json:"test"`

	// Margin is the non-inferiority bound: how much worse the candidate may be
	// before this fails.
	//
	// You almost never need "the candidate is better", you need "not worse by
	// more than I can accept". On a higher-is-better metric a margin of -0.02
	// tolerates a two-point drop. There is deliberately no default: the value
	// is a judgement about your product that nobody else can make.
	// +optional
	Margin *float64 `json:"margin,omitempty"`

	// Threshold is the bound for a threshold test.
	// +optional
	Threshold *float64 `json:"threshold,omitempty"`

	// Alpha is the false-positive rate. Defaults to 0.05 for the primary
	// metric; guardrails are corrected for multiplicity.
	// +optional
	Alpha *float64 `json:"alpha,omitempty"`

	// +optional
	Direction MetricDirection `json:"direction,omitempty"`
}

// Gate decides whether a stage passes.
type Gate struct {
	// Primary is the metric the promotion decision turns on. Exactly one.
	//
	// Gating eight metrics at alpha 0.05 each gives roughly a 34% chance of a
	// spurious rollback. One primary metric carries the full alpha; everything
	// else belongs in guardrails, under a stricter correction.
	Primary GateMetric `json:"primary"`

	// Guardrails must all hold. A guardrail failure is decisive: no amount of
	// improvement in the primary metric buys a policy violation.
	// +optional
	Guardrails []GateMetric `json:"guardrails,omitempty"`

	// KappaFloor is the agreement with the human gold set below which a judge
	// is not trusted to decide this release.
	//
	// Every number this gate compares came from a judge, and a judge is a
	// measuring instrument with its own error. If its agreement has drifted, or
	// was measured against a different judge model than the one about to run,
	// the comparison is precise about a quantity nobody validated — which is
	// worse than not gating, because it produces something that looks like
	// evidence. Defaults to 0.6.
	// +optional
	KappaFloor *float64 `json:"kappaFloor,omitempty"`
}

// CorpusSelector chooses which recorded sessions to replay.
type CorpusSelector struct {
	// Ref names a corpus store.
	//
	// Required by offline replay, where it is the recorded traffic to replay.
	// Optional for a live or shadow stage, where it names the store the
	// recorder is writing production sessions into — the same selector and the
	// same scorer, so that a candidate is measured on one instrument rather
	// than compared across two.
	// +optional
	Ref string `json:"ref,omitempty"`

	// Limit caps how many sessions are replayed. Zero means all of them.
	// +optional
	Limit int `json:"limit,omitempty"`

	// Sessions replays exactly these, for reproducing one result.
	// +optional
	Sessions []string `json:"sessions,omitempty"`
}

// ScorerSpec says where scores come from.
//
// Waveoff does not evaluate agents; it consumes scores from whatever you
// already run. Exactly one of Exec or HTTP is set.
type ScorerSpec struct {
	// +optional
	Exec *ExecScorerSpec `json:"exec,omitempty"`
	// +optional
	HTTP *HTTPScorerSpec `json:"http,omitempty"`

	// TimeoutSeconds bounds a scoring run. A scorer that hangs must not hang a
	// rollout.
	// +optional
	TimeoutSeconds int `json:"timeoutSeconds,omitempty"`
}

// ExecScorerSpec runs a scorer as a subprocess: refs on stdin, results on
// stdout.
type ExecScorerSpec struct {
	// +kubebuilder:validation:MinLength=1
	Command string `json:"command"`
	// +optional
	Args []string `json:"args,omitempty"`
}

// HTTPScorerSpec posts to a scoring service.
//
// Credentials are not configured here yet. They are given to the manager at
// deploy time from its own environment, because letting a rollout name any
// Secret would require a controller that already mutates every pod to be able
// to read every Secret in the cluster.
//
// The middle path, when per-rollout credentials are needed: a namespace-scoped
// Role granting Secret read only in the rollout's own namespace, bound
// per-namespace by the operator. That is a far smaller grant than cluster-wide
// and preserves a tenant boundary — which matters, because shared manager
// credentials mean every tenant's rollouts score through the same identity.
type HTTPScorerSpec struct {
	// +kubebuilder:validation:MinLength=1
	Endpoint string `json:"endpoint"`
}

// TrafficSpec says how a stage moves live traffic.
//
// Required by the shadow and live stages, meaningless to offline replay.
type TrafficSpec struct {
	// Router selects the implementation. The controller talks to an interface
	// and never to a mesh, so a cluster can change one without changing this.
	// +kubebuilder:validation:Enum=gateway-api;istio
	Router string `json:"router"`

	// RouteRef is the HTTPRoute or VirtualService to control.
	// +kubebuilder:validation:MinLength=1
	RouteRef string `json:"routeRef"`

	// IncumbentBackend and CandidateBackend are the backends already present on
	// that route. A rollout reweights what is there; it does not add backends
	// nobody configured.
	// +kubebuilder:validation:MinLength=1
	IncumbentBackend string `json:"incumbentBackend"`
	// +kubebuilder:validation:MinLength=1
	CandidateBackend string `json:"candidateBackend"`
}

// Stage is one step of a rollout.
//
// +kubebuilder:validation:XValidation:rule="self.mode != 'offline-replay' || (has(self.corpus) && size(self.corpus.ref) > 0)",message="an offline-replay stage needs a corpus to replay"
// +kubebuilder:validation:XValidation:rule="self.mode != 'live' || has(self.traffic)",message="a live stage routes real traffic and needs a traffic block"
// +kubebuilder:validation:XValidation:rule="self.mode != 'shadow' || has(self.traffic)",message="a shadow stage mirrors real traffic and needs a traffic block"
// +kubebuilder:validation:XValidation:rule="self.mode != 'live' || self.gate.primary.test == 'sequential'",message="a live canary is watched continuously, so its gate must use the sequential test; a fixed-horizon test is not valid under repeated peeking"
type Stage struct {
	// +kubebuilder:validation:MinLength=1
	Name string    `json:"name"`
	Mode StageMode `json:"mode"`

	// Corpus selects the recorded sessions an offline replay runs against.
	// +optional
	Corpus CorpusSelector `json:"corpus,omitempty"`
	Scorer ScorerSpec     `json:"scorer"`
	Gate   Gate           `json:"gate"`

	// Traffic is required by the shadow and live stages.
	// +optional
	Traffic *TrafficSpec `json:"traffic,omitempty"`

	// Weight is the share of live traffic the candidate receives, in percent.
	// Only meaningful for a live stage.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +optional
	Weight int `json:"weight,omitempty"`

	// MirrorPercent is the share of traffic copied to the candidate during a
	// shadow stage. Gateway API mirrors a whole rule or none of it, so only 0
	// and 100 are honoured there; Istio accepts any percentage.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +optional
	MirrorPercent int `json:"mirrorPercent,omitempty"`

	// MinObservationWindow is how long a live or shadow stage must run before
	// its verdict is acted on, however quickly the numbers look conclusive.
	//
	// Statistical sufficiency is not the same as having seen the traffic that
	// matters. A canary promoted in four minutes has not met the nightly batch,
	// the Monday morning peak, or the customer whose sessions are unlike
	// everyone else's.
	// +optional
	MinObservationWindow *metav1.Duration `json:"minObservationWindow,omitempty"`

	// MaxDuration bounds the stage. A canary with no end is a candidate that
	// ships by exhaustion.
	// +optional
	MaxDuration *metav1.Duration `json:"maxDuration,omitempty"`
}

// AnalyzerSpec selects what makes the promotion decision.
//
// The controller does not know or care which implementation answers. Leaving
// this empty uses the in-process analyzer; pointing it at an endpoint lets a
// team bring their own test without forking.
type AnalyzerSpec struct {
	// Endpoint receives the analysis request. Empty means in-process.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`
	// +optional
	TimeoutSeconds int `json:"timeoutSeconds,omitempty"`
}

// RollbackTrigger is a condition that withdraws a candidate.
// +kubebuilder:validation:Enum=gate-fail;guardrail-violation;budget-breach;analyzer-unavailable;write-divergence
type RollbackTrigger string

const (
	// TriggerGateFail is the primary metric failing its margin.
	TriggerGateFail RollbackTrigger = "gate-fail"
	// TriggerGuardrailViolation is any guardrail being breached.
	TriggerGuardrailViolation RollbackTrigger = "guardrail-violation"
	// TriggerBudgetBreach is the stage exceeding its MaxDuration.
	TriggerBudgetBreach RollbackTrigger = "budget-breach"
	// TriggerWriteDivergence is the candidate attempting a class of write the
	// incumbent never makes.
	//
	// Deterministic and available immediately, unlike everything else here. It
	// fires on set membership rather than on a rate, because a candidate
	// reaching for a tool the incumbent does not use has changed what the agent
	// does; waiting for that to reach significance means waiting for it to
	// happen repeatedly, which for a destructive tool is the wrong way round.
	TriggerWriteDivergence RollbackTrigger = "write-divergence"
	// TriggerAnalyzerUnavailable is the analyzer failing to answer.
	//
	// Included deliberately. A controller that cannot get a verdict must not
	// leave a candidate serving traffic on the assumption it is fine.
	TriggerAnalyzerUnavailable RollbackTrigger = "analyzer-unavailable"
)

// RollbackSpec configures automatic wave-off.
type RollbackSpec struct {
	// Auto withdraws the candidate without a human when a trigger fires.
	//
	// Withdrawing means returning all traffic to the incumbent. It is a weight
	// flip, not a rebuild, which is the whole reason the incumbent is retained.
	// +optional
	Auto bool `json:"auto,omitempty"`

	// Triggers are the conditions that withdraw a candidate. Empty means all
	// of them.
	// +optional
	Triggers []RollbackTrigger `json:"triggers,omitempty"`

	// RetainIncumbentFor keeps the incumbent available after promotion, so
	// going back is a weight flip rather than a rebuild.
	// +optional
	RetainIncumbentFor *metav1.Duration `json:"retainIncumbentFor,omitempty"`
}

// Fires reports whether a trigger is enabled.
func (r RollbackSpec) Fires(trigger RollbackTrigger) bool {
	if !r.Auto {
		return false
	}
	if len(r.Triggers) == 0 {
		return true
	}
	for _, t := range r.Triggers {
		if t == trigger {
			return true
		}
	}
	return false
}

// AgentRolloutSpec is a comparison between two manifests.
//
// +kubebuilder:validation:XValidation:rule="self.incumbentRef != self.candidateRef",message="incumbentRef and candidateRef are the same manifest; there is nothing to compare"
type AgentRolloutSpec struct {
	// IncumbentRef and CandidateRef name AgentManifests in this namespace.
	// +kubebuilder:validation:MinLength=1
	IncumbentRef string `json:"incumbentRef"`
	// +kubebuilder:validation:MinLength=1
	CandidateRef string `json:"candidateRef"`

	// Stages run in order. A stage that fails stops the rollout.
	// +kubebuilder:validation:MinItems=1
	Stages []Stage `json:"stages"`

	// +optional
	Analyzer AnalyzerSpec `json:"analyzer,omitempty"`
	// +optional
	Rollback RollbackSpec `json:"rollback,omitempty"`
}

// RolloutPhase is where a rollout has got to.
// +kubebuilder:validation:Enum=Pending;Running;Promoted;WavedOff;Held;Failed
type RolloutPhase string

const (
	PhasePending RolloutPhase = "Pending"
	PhaseRunning RolloutPhase = "Running"
	// PhasePromoted: every stage passed.
	PhasePromoted RolloutPhase = "Promoted"
	// PhaseWavedOff: a gate decided against the candidate.
	//
	// The wave-off is the decision; the rollback is the implementation. The API
	// keeps the two words apart.
	PhaseWavedOff RolloutPhase = "WavedOff"
	// PhaseHeld: the rollout cannot proceed and will not guess.
	//
	// An unreachable analyzer, a scorer that failed, a corpus whose contracts
	// have drifted. Holding is the only safe answer: promoting would ship the
	// exact change nobody was able to check.
	PhaseHeld RolloutPhase = "Held"
	// PhaseFailed: the rollout could not run at all.
	PhaseFailed RolloutPhase = "Failed"
)

// StageStatus records what happened in one stage.
type StageStatus struct {
	Name  string       `json:"name"`
	Phase RolloutPhase `json:"phase"`

	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// +optional
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`

	// Sessions is how many corpus items were replayed, and Scored how many
	// produced a usable measurement under both arms.
	//
	// Both are recorded because they are usually different, and the difference
	// is the difference between a result and an illusion: a gate that scored 40
	// of 400 items looks identical to one that scored all 400.
	// +optional
	Sessions int `json:"sessions,omitempty"`
	// +optional
	Scored int `json:"scored,omitempty"`
	// +optional
	Excluded int `json:"excluded,omitempty"`

	// Verdict is the analyzer's decision, rendered for a human.
	// +optional
	Verdict string `json:"verdict,omitempty"`
	// +optional
	Reason string `json:"reason,omitempty"`
	// Analyzer identifies what decided, so a promotion can be traced back to
	// the thing that made it.
	// +optional
	Analyzer string `json:"analyzer,omitempty"`

	// Weight is the share of live traffic the candidate held when this stage
	// finished, read back from the router rather than assumed.
	// +optional
	Weight int `json:"weight,omitempty"`

	// WriteActivity is what each arm tried to do to the world.
	//
	// Reported on every step of a shadow or live stage, whether or not it
	// fired. A stage where nothing was ever attempted either had no write tools
	// or was not actually watching, and those look identical without this.
	// +optional
	WriteActivity *WriteActivityStatus `json:"writeActivity,omitempty"`

	// RolledBack records that the candidate was withdrawn, and why.
	// +optional
	RolledBack bool `json:"rolledBack,omitempty"`
	// +optional
	Trigger RollbackTrigger `json:"trigger,omitempty"`
}

// WriteActivityStatus is the deterministic comparison of what each arm
// attempted to write.
//
// In shadow the candidate's writes never happened, so nothing downstream can
// compare their effects. The attempts, though, are directly comparable: both
// arms saw the same traffic and each decided what to reach for. That makes this
// the one finding a shadow stage produces that needs no judge and no
// statistics, and the earliest one available.
type WriteActivityStatus struct {
	// IncumbentAttempts and CandidateAttempts are write attempts per session,
	// so two arms that saw different amounts of traffic are not compared as
	// though they had not.
	// +optional
	IncumbentAttempts string `json:"incumbentAttempts,omitempty"`
	// +optional
	CandidateAttempts string `json:"candidateAttempts,omitempty"`

	// NewClasses are write tools the candidate reached for and the incumbent
	// never did. Non-empty is a guardrail violation on its own.
	// +optional
	NewClasses []string `json:"newClasses,omitempty"`

	// DroppedClasses are write tools the incumbent used and the candidate did
	// not. Reported, never a violation: a candidate that writes less may have
	// got better or may have got lazier, and only a scorer tells those apart.
	// +optional
	DroppedClasses []string `json:"droppedClasses,omitempty"`

	// Sessions counted on each arm.
	// +optional
	IncumbentSessions int `json:"incumbentSessions,omitempty"`
	// +optional
	CandidateSessions int `json:"candidateSessions,omitempty"`
}

// AgentRolloutStatus is the observed state.
type AgentRolloutStatus struct {
	// +optional
	Phase RolloutPhase `json:"phase,omitempty"`
	// +optional
	Reason string `json:"reason,omitempty"`

	// +optional
	Stages []StageStatus `json:"stages,omitempty"`

	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// AgentRollout compares a candidate manifest against the incumbent and decides
// whether it may be promoted.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=aro
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Incumbent",type=string,JSONPath=`.spec.incumbentRef`
// +kubebuilder:printcolumn:name="Candidate",type=string,JSONPath=`.spec.candidateRef`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.reason`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type AgentRollout struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec AgentRolloutSpec `json:"spec"`
	// +optional
	Status AgentRolloutStatus `json:"status,omitempty"`
}

// AgentRolloutList contains a list of AgentRollout.
// +kubebuilder:object:root=true
type AgentRolloutList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentRollout `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AgentRollout{}, &AgentRolloutList{})
}
