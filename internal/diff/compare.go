// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package diff

import (
	"fmt"
	"sort"

	"github.com/waveoffai/waveoff/api/v1alpha1"
	"github.com/waveoffai/waveoff/internal/digest"
)

// BodyLookup optionally resolves a prompt's text so the diff can show how many
// lines moved. It returns false when the body is not reachable — the diff must
// never depend on a git remote being up, so an unresolvable prompt degrades to
// showing digests and nothing else breaks.
type BodyLookup func(v1alpha1.PromptRef) (string, bool)

// Options tune the comparison.
type Options struct {
	// NameA and NameB are the object names, for the header.
	NameA, NameB string
	// Bodies resolves prompt text. May be nil.
	Bodies BodyLookup
	// AllowCrossAgent permits comparing manifests of two different agents.
	// Off by default: a cross-agent comparison has no meaningful verdict, and
	// silently producing one invites someone to act on it.
	AllowCrossAgent bool
}

// Compare produces the full explanation of what moved between two manifests.
func Compare(a, b *v1alpha1.AgentManifestSpec, opts Options) (*Result, error) {
	aB, aC, err := digest.Both(a)
	if err != nil {
		return nil, fmt.Errorf("digest left-hand manifest: %w", err)
	}
	bB, bC, err := digest.Both(b)
	if err != nil {
		return nil, fmt.Errorf("digest right-hand manifest: %w", err)
	}

	r := &Result{
		SchemaVersion:  "waveoff.ai/diff/v1alpha1",
		AgentA:         a.Agent,
		AgentB:         b.Agent,
		NameA:          opts.NameA,
		NameB:          opts.NameB,
		BehaviorDigest: Digests{A: aB, B: bB},
		ContentDigest:  Digests{A: aC, B: bC},
	}

	if a.Agent != b.Agent && !opts.AllowCrossAgent {
		r.Verdict = VerdictCrossAgent
		return r, nil
	}

	var ch []Change
	ch = append(ch, compareCode(a, b)...)
	ch = append(ch, compareModel(a, b)...)
	ch = append(ch, comparePrompts(a, b, opts.Bodies)...)
	ch = append(ch, compareTools(a, b)...)
	ch = append(ch, compareRetrieval(a, b)...)
	ch = append(ch, comparePolicy(a, b)...)
	ch = append(ch, compareJudges(a, b)...)
	sortChanges(ch)
	r.Changes = ch

	switch {
	case aB == bB && aC == bC:
		r.Verdict = VerdictIdentical
	case aB == bB:
		r.Verdict = VerdictProvenanceOnly
	default:
		r.Verdict = VerdictBehavioural
	}
	return r, nil
}

func scalar(plane Plane, field, from, to string, tags ...Tag) *Change {
	if from == to {
		return nil
	}
	c := &Change{Plane: plane, Field: field, Op: OpChanged, From: orNone(from), To: orNone(to), Tags: tags}
	c.Severity = severe(*c)
	return c
}

func severe(c Change) bool { return c.has(TagSecurity) || c.has(TagGate) }

func compareCode(a, b *v1alpha1.AgentManifestSpec) []Change {
	var out []Change
	if c := scalar(PlaneCode, "image", abbrevImage(a.Code.Image), abbrevImage(b.Code.Image), TagExec); c != nil {
		// Distinguish the two ways an image reference can move, because they
		// carry opposite consequences: different bytes needs a canary, the same
		// bytes from a new registry does not.
		if imageID(a.Code.Image) == imageID(b.Code.Image) {
			c.Detail = append(c.Detail, "same bytes, different registry — provenance only")
			c.Tags = nil
		}
		out = append(out, *c)
	}
	return out
}

func imageID(ref string) string {
	for i := 0; i < len(ref); i++ {
		if ref[i] == '@' {
			return ref[i+1:]
		}
	}
	return ref
}

func compareModel(a, b *v1alpha1.AgentManifestSpec) []Change {
	var out []Change
	add := func(c *Change) {
		if c != nil {
			out = append(out, *c)
		}
	}
	add(scalar(PlaneModel, "provider", a.Model.Provider, b.Model.Provider, TagModel))
	add(scalar(PlaneModel, "id", a.Model.ID, b.Model.ID, TagModel))
	add(scalar(PlaneModel, "pin", a.Model.Pin, b.Model.Pin, TagModel))

	pa, pb := a.Model.Params, b.Model.Params
	add(scalar(PlaneModel, "temperature", fmtFloat(pa.Temperature), fmtFloat(pb.Temperature), TagModel))
	add(scalar(PlaneModel, "topP", fmtFloat(pa.TopP), fmtFloat(pb.TopP), TagModel))
	add(scalar(PlaneModel, "topK", fmtInt(pa.TopK), fmtInt(pb.TopK), TagModel))
	add(scalar(PlaneModel, "maxTokens", fmtInt(pa.MaxTokens), fmtInt(pb.MaxTokens), TagModel))
	add(scalar(PlaneModel, "stopSequences", fmt.Sprint(pa.StopSequences), fmt.Sprint(pb.StopSequences), TagModel))
	return out
}

func comparePrompts(a, b *v1alpha1.AgentManifestSpec, bodies BodyLookup) []Change {
	am := index(a.Prompts, func(p v1alpha1.PromptRef) string { return p.Name })
	bm := index(b.Prompts, func(p v1alpha1.PromptRef) string { return p.Name })
	var out []Change

	for _, name := range union(am, bm) {
		pa, inA := am[name]
		pb, inB := bm[name]
		switch {
		case !inA:
			out = append(out, Change{Plane: PlanePrompts, Element: name, Op: OpAdded,
				To: abbrev(pb.Digest), Tags: []Tag{TagInput}})
		case !inB:
			out = append(out, Change{Plane: PlanePrompts, Element: name, Op: OpRemoved,
				From: abbrev(pa.Digest), Tags: []Tag{TagInput}})
		default:
			if pa.Digest != pb.Digest {
				// No Field: a prompt's digest is the prompt, so this reads
				// on one line rather than as "system" then "digest".
				c := Change{Plane: PlanePrompts, Element: name, Op: OpChanged,
					From: abbrev(pa.Digest), To: abbrev(pb.Digest), Tags: []Tag{TagInput}}
				if d, ok := lineDelta(pa, pb, bodies); ok {
					c.Detail = append(c.Detail, d)
				}
				out = append(out, c)
			}
			if pa.Source != pb.Source {
				// Provenance: recorded and shown, but it does not move identity.
				out = append(out, Change{Plane: PlanePrompts, Element: name, Field: "source", Op: OpChanged,
					From: orNone(pa.Source), To: orNone(pb.Source),
					Detail: []string{"provenance only — same prompt body"}})
			}
		}
	}
	return out
}

func lineDelta(a, b v1alpha1.PromptRef, bodies BodyLookup) (string, bool) {
	if bodies == nil {
		return "", false
	}
	ta, okA := bodies(a)
	tb, okB := bodies(b)
	if !okA || !okB {
		return "", false
	}
	added, removed := lineDiffCounts(ta, tb)
	return fmt.Sprintf("(+%d −%d)", added, removed), true
}

// effectRank orders effects by blast radius, so that widening can be detected.
var effectRank = map[v1alpha1.ToolEffect]int{
	v1alpha1.EffectRead: 0, v1alpha1.EffectIdempotentWrite: 1, v1alpha1.EffectWrite: 2,
}

func compareTools(a, b *v1alpha1.AgentManifestSpec) []Change {
	am := index(a.Tools, func(t v1alpha1.ToolRef) string { return t.Name })
	bm := index(b.Tools, func(t v1alpha1.ToolRef) string { return t.Name })
	var out []Change

	for _, name := range union(am, bm) {
		ta, inA := am[name]
		tb, inB := bm[name]
		switch {
		case !inA:
			// A new tool is new model input — its name and description enter
			// the prompt — and a new write tool is a new blast radius.
			c := Change{Plane: PlaneTools, Element: name, Op: OpAdded, Tags: []Tag{TagInput}}
			c.Detail = append(c.Detail, fmt.Sprintf("effect=%s  replayPolicy=%s",
				orNone(string(tb.Effect)), orNone(string(tb.ReplayPolicy))))
			if effectRank[tb.Effect] > 0 {
				c.Tags = append(c.Tags, TagSecurity)
				c.Detail = append(c.Detail, "new tool can write")
			}
			c.Severity = severe(c)
			out = append(out, c)
		case !inB:
			out = append(out, Change{Plane: PlaneTools, Element: name, Op: OpRemoved,
				Tags: []Tag{TagInput}, Detail: []string{"the model can no longer call this"}})
		default:
			if ta.ContractDigest != tb.ContractDigest {
				// The contract digest covers the description text as well as
				// the schema, because the description is prompt input. A
				// silent rewrite is the shape of an MCP tool-poisoning or
				// rug-pull attack, so this is always security-relevant.
				c := Change{Plane: PlaneTools, Element: name, Field: "contract", Op: OpChanged,
					From: abbrev(ta.ContractDigest), To: abbrev(tb.ContractDigest),
					Tags:   []Tag{TagInput, TagSecurity},
					Detail: []string{fmt.Sprintf("description or schema changed · effect=%s", orNone(string(tb.Effect)))}}
				c.Severity = true
				out = append(out, c)
			}
			if ta.Effect != tb.Effect {
				c := Change{Plane: PlaneTools, Element: name, Field: "effect", Op: OpChanged,
					From: orNone(string(ta.Effect)), To: orNone(string(tb.Effect)), Tags: []Tag{TagExec}}
				if effectRank[tb.Effect] > effectRank[ta.Effect] {
					c.Tags = append(c.Tags, TagSecurity)
					c.Detail = append(c.Detail, "effect widened — this tool may now do more")
					c.Severity = true
				}
				out = append(out, c)
			}
			if ta.Server != tb.Server {
				out = append(out, Change{Plane: PlaneTools, Element: name, Field: "server", Op: OpChanged,
					From: orNone(ta.Server), To: orNone(tb.Server), Tags: []Tag{TagExec, TagSecurity},
					Detail:   []string{"same contract, different server — it may not return the same data"},
					Severity: true})
			}
			if ta.ReplayPolicy != tb.ReplayPolicy {
				out = append(out, Change{Plane: PlaneTools, Element: name, Field: "replayPolicy", Op: OpChanged,
					From: orNone(string(ta.ReplayPolicy)), To: orNone(string(tb.ReplayPolicy)), Tags: []Tag{TagExec}})
			}
		}
	}
	return out
}

func compareRetrieval(a, b *v1alpha1.AgentManifestSpec) []Change {
	ra, rb := a.Retrieval, b.Retrieval
	if ra == nil {
		ra = &v1alpha1.RetrievalSpec{}
	}
	if rb == nil {
		rb = &v1alpha1.RetrievalSpec{}
	}
	var out []Change
	if c := scalar(PlaneRetrieval, "indexSnapshot", ra.IndexSnapshot, rb.IndexSnapshot, TagInput); c != nil {
		c.Detail = append(c.Detail, "a different snapshot is a different set of retrievable documents")
		out = append(out, *c)
	}
	if c := scalar(PlaneRetrieval, "embeddingModel", ra.EmbeddingModel, rb.EmbeddingModel, TagInput); c != nil {
		out = append(out, *c)
	}
	return out
}

func comparePolicy(a, b *v1alpha1.AgentManifestSpec) []Change {
	var da, db string
	if a.Policy != nil {
		da = a.Policy.BundleDigest
	}
	if b.Policy != nil {
		db = b.Policy.BundleDigest
	}
	if c := scalar(PlanePolicy, "bundleDigest", abbrev(da), abbrev(db), TagExec, TagSecurity); c != nil {
		c.Detail = append(c.Detail, "what the agent is permitted to do changed")
		return []Change{*c}
	}
	return nil
}

func compareJudges(a, b *v1alpha1.AgentManifestSpec) []Change {
	am := index(a.Judges, func(j v1alpha1.JudgeSpec) string { return j.Name })
	bm := index(b.Judges, func(j v1alpha1.JudgeSpec) string { return j.Name })
	var out []Change

	for _, name := range union(am, bm) {
		ja, inA := am[name]
		jb, inB := bm[name]
		switch {
		case !inA:
			out = append(out, Change{Plane: PlaneJudges, Element: name, Op: OpAdded,
				Tags: []Tag{TagGate}, Severity: true,
				Detail: []string{"a new metric now gates promotion"}})
		case !inB:
			out = append(out, Change{Plane: PlaneJudges, Element: name, Op: OpRemoved,
				Tags: []Tag{TagGate}, Severity: true,
				Detail: []string{"this metric no longer gates promotion"}})
		default:
			judgeMoved := ja.Model != jb.Model || ja.RubricDigest != jb.RubricDigest

			if ja.Model != jb.Model {
				out = append(out, Change{Plane: PlaneJudges, Element: name, Field: "model", Op: OpChanged,
					From: ja.Model, To: jb.Model, Tags: []Tag{TagGate}, Severity: true})
			}
			if ja.RubricDigest != jb.RubricDigest {
				out = append(out, Change{Plane: PlaneJudges, Element: name, Field: "rubric", Op: OpChanged,
					From: abbrev(ja.RubricDigest), To: abbrev(jb.RubricDigest), Tags: []Tag{TagGate}, Severity: true})
			}
			// Gate the gate. If the judge moved but its agreement with the human
			// gold set was not re-measured, the number the gate is about to
			// trust was measured against a judge that no longer exists.
			if judgeMoved {
				if d := staleCalibration(ja, jb); d != "" {
					out = append(out, Change{Plane: PlaneJudges, Element: name, Field: "calibration", Op: OpChanged,
						Tags: []Tag{TagGate}, Severity: true, Detail: []string{d}})
				}
			} else if c := calibrationChanged(ja, jb); c != nil {
				out = append(out, *c)
			}
		}
	}
	return out
}

func staleCalibration(a, b v1alpha1.JudgeSpec) string {
	if b.Calibration == nil || b.Calibration.MeasuredAt == nil {
		return "judge changed and no calibration is recorded — κ against the gold set is unknown"
	}
	if a.Calibration != nil && a.Calibration.MeasuredAt != nil &&
		!b.Calibration.MeasuredAt.After(a.Calibration.MeasuredAt.Time) {
		return fmt.Sprintf("κ=%s measured %s, against the OLD judge — stale",
			fmtFloat(b.Calibration.Kappa), b.Calibration.MeasuredAt.UTC().Format("2006-01-02"))
	}
	return ""
}

func calibrationChanged(a, b v1alpha1.JudgeSpec) *Change {
	ka, kb := "(none)", "(none)"
	if a.Calibration != nil {
		ka = fmtFloat(a.Calibration.Kappa)
	}
	if b.Calibration != nil {
		kb = fmtFloat(b.Calibration.Kappa)
	}
	if ka == kb {
		return nil
	}
	return &Change{Plane: PlaneJudges, Element: b.Name, Field: "κ", Op: OpChanged, From: ka, To: kb,
		Detail: []string{"re-measured, judge unchanged — provenance only"}}
}

func index[T any](in []T, key func(T) string) map[string]T {
	m := make(map[string]T, len(in))
	for _, v := range in {
		m[key(v)] = v
	}
	return m
}

func union[T any](a, b map[string]T) []string {
	seen := map[string]bool{}
	var out []string
	for k := range a {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	for k := range b {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
