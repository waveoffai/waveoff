// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ToolEffect classifies what a tool call does to external state.
//
// It is mandatory on every tool. An unclassified tool is refused during replay
// rather than passed through, so a tool whose effect nobody has asserted can
// never write during an evaluation.
// +kubebuilder:validation:Enum=read;idempotent-write;write
type ToolEffect string

const (
	// EffectRead does not mutate external state.
	EffectRead ToolEffect = "read"
	// EffectIdempotentWrite mutates external state, but repeating the call with
	// identical arguments converges on the same result.
	EffectIdempotentWrite ToolEffect = "idempotent-write"
	// EffectWrite mutates external state, and repetition is not safe.
	EffectWrite ToolEffect = "write"
)

// ReplayPolicy says how a tool call is served when the manifest is replayed.
// +kubebuilder:validation:Enum=snapshot;no-op;live
type ReplayPolicy string

const (
	// ReplaySnapshot serves the call from the recorded cassette.
	ReplaySnapshot ReplayPolicy = "snapshot"
	// ReplayNoOp refuses the call and returns a synthesised success.
	ReplayNoOp ReplayPolicy = "no-op"
	// ReplayLive forwards the call to the real server. Only ever safe for reads.
	ReplayLive ReplayPolicy = "live"
)

// CodeSpec pins the agent's executable.
type CodeSpec struct {
	// Image is the container image running the agent. It must be digest-pinned
	// (name@sha256:...); a mutable tag is rejected, because a manifest that
	// cannot say which bytes ran is not a release artifact.
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`
}

// ModelParams are the decoding parameters. Every numeric is a pointer because
// absent and zero are different behaviours: an unset temperature takes the
// provider default, while temperature 0 is greedy decoding.
type ModelParams struct {
	// +optional
	Temperature *float64 `json:"temperature,omitempty"`
	// +optional
	TopP *float64 `json:"topP,omitempty"`
	// +optional
	TopK *int64 `json:"topK,omitempty"`
	// +optional
	MaxTokens *int64 `json:"maxTokens,omitempty"`
	// +optional
	StopSequences []string `json:"stopSequences,omitempty"`
}

// ModelSpec pins the model and how it is sampled.
type ModelSpec struct {
	// +kubebuilder:validation:MinLength=1
	Provider string `json:"provider"`
	// +kubebuilder:validation:MinLength=1
	ID string `json:"id"`
	// Pin is the provider-side version pin, where the provider offers one.
	// +optional
	Pin string `json:"pin,omitempty"`
	// +optional
	Params ModelParams `json:"params,omitempty"`
}

// PromptRef pins one prompt by content.
type PromptRef struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// Source is where the prompt came from. Provenance only: it is recorded and
	// diffed, but excluded from behaviorDigest, so moving a prompt between
	// repositories does not mint a new agent identity.
	// +optional
	Source string `json:"source,omitempty"`
	// Digest is the hash of the prompt body.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	Digest string `json:"digest"`
}

// ToolRef pins one tool contract as the agent saw it.
type ToolRef struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// Server is the MCP endpoint serving this tool. Included in behaviorDigest:
	// repointing an agent from a production to a staging server is a behaviour
	// change even when the contract is byte-identical.
	// +optional
	Server string `json:"server,omitempty"`
	// ContractDigest hashes {name, description, inputSchema} as advertised at
	// call time. The description is hashed because it is prompt input, which
	// makes a silent description rewrite both a behavioural change and a
	// detectable tool-poisoning attempt.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	ContractDigest string `json:"contractDigest"`
	// Effect is mandatory and must be asserted by an operator. It is never
	// inferred from server-advertised hints, because the server is the
	// untrusted party in the tool-poisoning threat model.
	Effect ToolEffect `json:"effect"`
	// +optional
	ReplayPolicy ReplayPolicy `json:"replayPolicy,omitempty"`
}

// RetrievalSpec pins the retrieval side of the agent.
type RetrievalSpec struct {
	// IndexSnapshot names the index contents, not the index. Two snapshots are
	// two different sets of retrievable documents.
	// +optional
	IndexSnapshot string `json:"indexSnapshot,omitempty"`
	// +optional
	EmbeddingModel string `json:"embeddingModel,omitempty"`
}

// PolicySpec pins the authorization bundle.
type PolicySpec struct {
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	BundleDigest string `json:"bundleDigest"`
}

// JudgeCalibration records how well a judge agreed with the human gold set.
//
// This is a measurement, not a configuration input, so it is excluded from
// behaviorDigest: re-measuring kappa on an unchanged judge must not mint a new
// agent identity. It is still covered by contentDigest and still surfaces in
// waveoff diff, because a stale calibration is a release-blocking fact.
type JudgeCalibration struct {
	// +optional
	Kappa *float64 `json:"kappa,omitempty"`
	// +optional
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	GoldSetDigest string `json:"goldSetDigest,omitempty"`
	// +optional
	MeasuredAt *metav1.Time `json:"measuredAt,omitempty"`
}

// JudgeSpec pins a judge. Judge configuration is part of the release artifact:
// if you gate on a judge, a silent judge model update is a release-blocking
// change disguised as a no-op.
type JudgeSpec struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// +kubebuilder:validation:MinLength=1
	Model string `json:"model"`
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	RubricDigest string `json:"rubricDigest"`
	// +optional
	Calibration *JudgeCalibration `json:"calibration,omitempty"`
}

// AgentManifestSpec is the complete set of inputs that determine an agent's
// behaviour, in one object, so that "roll back the agent" is unambiguous.
//
// The spec is immutable once created. A change to any field is a new manifest
// with a new name, never an edit to an existing one.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="AgentManifest spec is immutable; create a new manifest instead of editing this one"
type AgentManifestSpec struct {
	// Agent is the stable logical name of the agent this manifest is a version
	// of. It is part of the identity hash: without it, two unrelated agents
	// that happen to share a configuration would collide in the corpus.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=63
	Agent string `json:"agent"`

	// BehaviorDigest is the identity hash: everything that determines
	// behaviour, and nothing that does not. Two manifests with the same
	// behaviorDigest are the same agent and a rollout between them needs no
	// canary.
	//
	// It is supplied by the author, not computed at admission. `waveoff pin`
	// emits it and `waveoff verify --write` repairs it; the webhook only
	// checks it. Nothing rewrites the stored object, so GitOps sees no drift.
	//
	// Schema validation runs before validating webhooks, so a malformed digest
	// is rejected here rather than by the webhook. That is the right order —
	// it holds even if the webhook was never deployed — but it means this
	// message, not the webhook's, is the one an operator reads. Hence CEL with
	// a written-out message rather than a bare pattern.
	// +kubebuilder:validation:XValidation:rule="self.matches('^sha256:[0-9a-f]{64}$')",message="behaviorDigest must be a sha256 digest. Digests are authored, not computed at admission, so that nothing rewrites what you applied. Run: waveoff verify --write <file>"
	BehaviorDigest string `json:"behaviorDigest"`

	// ContentDigest covers the whole spec with no exclusions. It is the
	// tamper-evidence and compliance hash. Because only behaviorDigest has a
	// classification map, a field misclassified there is still caught here.
	// +kubebuilder:validation:XValidation:rule="self.matches('^sha256:[0-9a-f]{64}$')",message="contentDigest must be a sha256 digest. Digests are authored, not computed at admission, so that nothing rewrites what you applied. Run: waveoff verify --write <file>"
	ContentDigest string `json:"contentDigest"`

	Code  CodeSpec  `json:"code"`
	Model ModelSpec `json:"model"`

	// +optional
	// +listType=map
	// +listMapKey=name
	Prompts []PromptRef `json:"prompts,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=name
	Tools []ToolRef `json:"tools,omitempty"`
	// +optional
	Retrieval *RetrievalSpec `json:"retrieval,omitempty"`
	// +optional
	Policy *PolicySpec `json:"policy,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=name
	Judges []JudgeSpec `json:"judges,omitempty"`
}

// AgentManifest is an immutable, content-addressed pin of everything that
// determines one agent's behaviour.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=am
// +kubebuilder:printcolumn:name="Agent",type=string,JSONPath=`.spec.agent`
// +kubebuilder:printcolumn:name="Model",type=string,JSONPath=`.spec.model.id`
// +kubebuilder:printcolumn:name="Behavior",type=string,JSONPath=`.spec.behaviorDigest`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type AgentManifest struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec AgentManifestSpec `json:"spec"`
}

// AgentManifestList contains a list of AgentManifest.
// +kubebuilder:object:root=true
type AgentManifestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentManifest `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AgentManifest{}, &AgentManifestList{})
}
