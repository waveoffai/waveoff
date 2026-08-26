// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

// Package digest computes the two hashes that give an AgentManifest its
// identity.
//
//   - behaviorDigest covers everything that determines behaviour and nothing
//     that does not. Two manifests sharing one are the same agent.
//   - contentDigest covers the whole spec with no exclusions. It is the
//     tamper-evidence hash, and it is what catches a field this package
//     misclassified.
//
// Both are driven by the single Registry below, so a field cannot be added to
// the API without a decision being recorded here. TestRegistryIsExhaustive
// walks the Go type and fails if any path is missing.
package digest

// Scope selects which of the two digests a projection is being built for.
type Scope int

const (
	// ScopeBehavior projects only the fields that determine behaviour.
	ScopeBehavior Scope = iota
	// ScopeContent projects the entire spec.
	ScopeContent
)

func (s Scope) String() string {
	if s == ScopeBehavior {
		return "behavior"
	}
	return "content"
}

// Inclusion says whether a field participates in behaviorDigest.
type Inclusion int

const (
	// InBoth means the field determines behaviour. It is hashed into both
	// digests.
	InBoth Inclusion = iota
	// ContentOnly means the field is provenance or measurement. It is hashed
	// into contentDigest, diffed, and displayed, but a change to it alone
	// leaves the agent's identity untouched.
	ContentOnly
)

// NullSemantics resolves the absent-versus-zero question, which JSON
// canonicalisation does not answer for us. Getting this wrong is silent: it
// changes every existing digest without changing any visible behaviour.
type NullSemantics int

const (
	// Required fields are always emitted. API validation guarantees they are
	// non-empty, so there is no absent case to disambiguate.
	Required NullSemantics = iota
	// DistinctZero fields are emitted whenever set, including when set to a
	// zero value. Absent and zero are different behaviours and must hash
	// differently: an unset temperature takes the provider default, while
	// temperature 0 is greedy decoding.
	DistinctZero
	// EmptyIsAbsent fields normalise an empty value away. `tools: []` and an
	// absent `tools` describe the same agent and must hash identically.
	EmptyIsAbsent
)

// Field is one entry in the classification map.
type Field struct {
	// Path is the dotted path into the spec, with "[]" marking a list element,
	// e.g. "tools[].contractDigest".
	Path string
	// Inclusion decides whether this field reaches behaviorDigest.
	Inclusion Inclusion
	// Null decides how absent and zero values are projected.
	Null NullSemantics
	// Why is the rationale. It is rendered into docs/digest.md, so it is the
	// documentation as well as a comment.
	Why string
}

// Registry is the classification map. It is normative: docs/digest.md is
// generated from it, and both projections are driven by it.
//
// The guiding principle is: when in doubt, include. Including a behaviourally
// irrelevant field costs one unnecessary canary. Excluding a behaviourally
// relevant one ships an unvalidated change to production.
//
// spec.behaviorDigest and spec.contentDigest are absent by construction: both
// are elided before either hash is computed, so neither is a registry entry.
var Registry = []Field{
	{
		Path: "agent", Inclusion: InBoth, Null: Required,
		Why: "Agent identity. Without it two unrelated agents that happen to share a " +
			"configuration would produce the same behaviorDigest, and M1 corpus keying " +
			"would merge their recorded traffic.",
	},

	{
		Path: "code.image", Inclusion: InBoth, Null: Required,
		Why: "Only the @sha256: portion enters behaviorDigest; the registry and repository " +
			"prefix does not. The digest pins the bytes, so moving an image between " +
			"registries is provably not a behaviour change and must promote without a " +
			"canary. contentDigest covers the full reference.",
	},

	{Path: "model.provider", Inclusion: InBoth, Null: Required, Why: "Determines which API serves the request."},
	{Path: "model.id", Inclusion: InBoth, Null: Required, Why: "The model."},
	{
		Path: "model.pin", Inclusion: InBoth, Null: EmptyIsAbsent,
		Why: "Provider-side version pin. Unpinned and pinned-to-any-value are different " +
			"exposures to provider-side model drift.",
	},
	{
		Path: "model.params.temperature", Inclusion: InBoth, Null: DistinctZero,
		Why: "Absent takes the provider default; 0 is greedy decoding. These are different " +
			"behaviours and must hash differently.",
	},
	{Path: "model.params.topP", Inclusion: InBoth, Null: DistinctZero, Why: "Nucleus sampling cutoff; absent and 0 differ."},
	{Path: "model.params.topK", Inclusion: InBoth, Null: DistinctZero, Why: "Top-k cutoff; absent and 0 differ."},
	{Path: "model.params.maxTokens", Inclusion: InBoth, Null: DistinctZero, Why: "Truncation point; absent and 0 differ."},
	{Path: "model.params.stopSequences", Inclusion: InBoth, Null: EmptyIsAbsent, Why: "No stop sequences and an empty list are the same agent."},

	{Path: "prompts[].name", Inclusion: InBoth, Null: Required, Why: "Identifies which prompt slot the body fills."},
	{
		Path: "prompts[].source", Inclusion: ContentOnly, Null: EmptyIsAbsent,
		Why: "Provenance. The prompt body is pinned by prompts[].digest, so moving a prompt " +
			"between repositories or branches leaves behaviour untouched.",
	},
	{Path: "prompts[].digest", Inclusion: InBoth, Null: Required, Why: "The prompt body. Directly model input."},

	{Path: "tools[].name", Inclusion: InBoth, Null: Required, Why: "The name the model calls, and itself model input."},
	{
		Path: "tools[].server", Inclusion: InBoth, Null: EmptyIsAbsent,
		Why: "Included despite being an endpoint. Repointing an agent from a production to a " +
			"staging MCP server is a behaviour change even when the contract digest is " +
			"byte-identical, and the fail-safe direction is to include.",
	},
	{
		Path: "tools[].contractDigest", Inclusion: InBoth, Null: Required,
		Why: "Hashes the tool description text as well as the JSON schema, because the " +
			"description is prompt input. This is why a description rewrite is both a " +
			"behavioural change and a detectable tool-poisoning attempt.",
	},
	{Path: "tools[].effect", Inclusion: InBoth, Null: Required, Why: "Decides whether the tool may run during replay. Changing it changes what an evaluation does."},
	{Path: "tools[].replayPolicy", Inclusion: InBoth, Null: EmptyIsAbsent, Why: "Decides how the call is served during replay."},

	{Path: "retrieval.indexSnapshot", Inclusion: InBoth, Null: EmptyIsAbsent, Why: "Names the retrievable document set. A different snapshot is different context."},
	{Path: "retrieval.embeddingModel", Inclusion: InBoth, Null: EmptyIsAbsent, Why: "Determines what retrieval returns."},

	{Path: "policy.bundleDigest", Inclusion: InBoth, Null: Required, Why: "Determines which actions are permitted at runtime."},

	{Path: "judges[].name", Inclusion: InBoth, Null: Required, Why: "Identifies the gate metric this judge produces."},
	{
		Path: "judges[].model", Inclusion: InBoth, Null: Required,
		Why: "The judge model is part of the release artifact. A silent judge model update is " +
			"a release-blocking change disguised as a no-op.",
	},
	{Path: "judges[].rubricDigest", Inclusion: InBoth, Null: Required, Why: "The scoring rubric. Changing it changes what the gate measures."},
	{
		Path: "judges[].calibration.kappa", Inclusion: ContentOnly, Null: DistinctZero,
		Why: "A measurement of the judge, not a configuration input to the agent. " +
			"Re-measuring kappa on an unchanged judge must not mint a new agent identity. " +
			"It still surfaces in waveoff diff, because a stale calibration blocks promotion.",
	},
	{Path: "judges[].calibration.goldSetDigest", Inclusion: ContentOnly, Null: EmptyIsAbsent, Why: "Identifies what the calibration was measured against. Measurement metadata."},
	{Path: "judges[].calibration.measuredAt", Inclusion: ContentOnly, Null: EmptyIsAbsent, Why: "When the calibration was taken. Measurement metadata."},
}

// byPath indexes Registry for lookup during projection.
var byPath = func() map[string]Field {
	m := make(map[string]Field, len(Registry))
	for _, f := range Registry {
		if _, dup := m[f.Path]; dup {
			panic("digest: duplicate registry path " + f.Path)
		}
		m[f.Path] = f
	}
	return m
}()

// Lookup returns the classification for a path.
func Lookup(path string) (Field, bool) {
	f, ok := byPath[path]
	return f, ok
}

// Paths returns every registered path, in declaration order.
func Paths() []string {
	out := make([]string, 0, len(Registry))
	for _, f := range Registry {
		out = append(out, f.Path)
	}
	return out
}
