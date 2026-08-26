// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package digest

import (
	"fmt"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/waveoffai/waveoff/api/v1alpha1"
)

// Project builds the JSON-shaped value that gets canonicalised and hashed.
//
// It is written field by field against the Registry and never marshals
// AgentManifestSpec. That is deliberate: if the projection came from
// json.Marshal, then adding `omitempty` to a pointer field — an edit that looks
// purely cosmetic — would silently change every digest ever issued. Struct tags
// are not part of the digest contract.
//
// Both spec.behaviorDigest and spec.contentDigest are elided, since a hash
// cannot cover itself.
func Project(spec *v1alpha1.AgentManifestSpec, scope Scope) map[string]any {
	p := &proj{scope: scope, out: map[string]any{}}

	p.str("agent", spec.Agent)

	// code.image is the one field whose projection differs between the two
	// scopes. behaviorDigest sees only the content hash, so a registry
	// migration leaves identity untouched; contentDigest sees the whole
	// reference, so the move is still recorded and still diffed.
	p.require("code.image")
	if scope == ScopeBehavior {
		p.set("code.imageDigest", imageContentID(spec.Code.Image))
	} else {
		p.set("code.image", spec.Code.Image)
	}

	p.str("model.provider", spec.Model.Provider)
	p.str("model.id", spec.Model.ID)
	p.str("model.pin", spec.Model.Pin)
	p.f64("model.params.temperature", spec.Model.Params.Temperature)
	p.f64("model.params.topP", spec.Model.Params.TopP)
	p.i64("model.params.topK", spec.Model.Params.TopK)
	p.i64("model.params.maxTokens", spec.Model.Params.MaxTokens)
	p.strs("model.params.stopSequences", spec.Model.Params.StopSequences)

	// Sets keyed by name, not ordered lists: sorting happens here, in the
	// hash input, and never on the stored object. Reordering a manifest must
	// not change its identity, and a webhook that reordered it for us would
	// leave Argo CD and Flux showing permanent un-reconcilable drift.
	if prompts := sortedByName(spec.Prompts, func(x v1alpha1.PromptRef) string { return x.Name }); len(prompts) > 0 {
		items := make([]any, 0, len(prompts))
		for _, pr := range prompts {
			e := p.elem("prompts[]")
			e.str("name", pr.Name)
			e.str("source", pr.Source)
			e.str("digest", pr.Digest)
			items = append(items, e.out)
		}
		p.out["prompts"] = items
	}

	if tools := sortedByName(spec.Tools, func(x v1alpha1.ToolRef) string { return x.Name }); len(tools) > 0 {
		items := make([]any, 0, len(tools))
		for _, t := range tools {
			e := p.elem("tools[]")
			e.str("name", t.Name)
			e.str("server", t.Server)
			e.str("contractDigest", t.ContractDigest)
			e.str("effect", string(t.Effect))
			e.str("replayPolicy", string(t.ReplayPolicy))
			items = append(items, e.out)
		}
		p.out["tools"] = items
	}

	if spec.Retrieval != nil {
		p.str("retrieval.indexSnapshot", spec.Retrieval.IndexSnapshot)
		p.str("retrieval.embeddingModel", spec.Retrieval.EmbeddingModel)
	}

	if spec.Policy != nil {
		p.str("policy.bundleDigest", spec.Policy.BundleDigest)
	}

	if judges := sortedByName(spec.Judges, func(x v1alpha1.JudgeSpec) string { return x.Name }); len(judges) > 0 {
		items := make([]any, 0, len(judges))
		for _, j := range judges {
			e := p.elem("judges[]")
			e.str("name", j.Name)
			e.str("model", j.Model)
			e.str("rubricDigest", j.RubricDigest)
			if j.Calibration != nil {
				e.f64("calibration.kappa", j.Calibration.Kappa)
				e.str("calibration.goldSetDigest", j.Calibration.GoldSetDigest)
				e.time("calibration.measuredAt", j.Calibration.MeasuredAt)
			}
			items = append(items, e.out)
		}
		p.out["judges"] = items
	}

	return p.out
}

// imageContentID returns the part of an image reference that pins the bytes.
//
// For a digest-pinned reference that is the sha256, which is what
// behaviorDigest hashes. An unpinned reference has no content identity, so the
// whole string is returned instead: the fail-safe direction is to include more,
// and API validation rejects unpinned images separately. Keeping this total
// rather than fallible means `waveoff diff` still works on a half-written local
// manifest.
func imageContentID(ref string) string {
	if i := strings.Index(ref, "@"); i >= 0 {
		return ref[i+1:]
	}
	return ref
}

func sortedByName[T any](in []T, name func(T) string) []T {
	if len(in) == 0 {
		return nil
	}
	out := make([]T, len(in))
	copy(out, in)
	sort.SliceStable(out, func(i, j int) bool { return name(out[i]) < name(out[j]) })
	return out
}

// proj accumulates one projected object. regPfx is the Registry path prefix
// for list elements, so a ToolRef writes "name" but classifies against
// "tools[].name".
type proj struct {
	scope  Scope
	regPfx string
	out    map[string]any
}

func (p *proj) elem(kind string) *proj {
	return &proj{scope: p.scope, regPfx: kind + ".", out: map[string]any{}}
}

// classify resolves a relative path against the Registry and reports whether it
// belongs in this scope. An unregistered path is a programming error, not a
// data error: it means a field reached the API without anyone deciding whether
// it determines behaviour. TestRegistryIsExhaustive catches it in CI, and this
// panic catches it if that test is ever deleted.
func (p *proj) classify(rel string) (Field, bool) {
	full := p.regPfx + rel
	f, ok := Lookup(full)
	if !ok {
		panic(fmt.Sprintf("digest: %q is not in the classification map; "+
			"every spec field must be classified as InBoth or ContentOnly before it can be hashed", full))
	}
	if p.scope == ScopeBehavior && f.Inclusion == ContentOnly {
		return f, false
	}
	return f, true
}

// require asserts a path is registered without projecting it, for the fields
// whose value is written by hand.
func (p *proj) require(rel string) { p.classify(rel) }

// set writes a value at a dotted path, creating intermediate objects.
func (p *proj) set(path string, v any) {
	parts := strings.Split(path, ".")
	m := p.out
	for _, k := range parts[:len(parts)-1] {
		next, ok := m[k].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[k] = next
		}
		m = next
	}
	m[parts[len(parts)-1]] = v
}

func (p *proj) str(rel, v string) {
	f, in := p.classify(rel)
	if !in {
		return
	}
	if f.Null == EmptyIsAbsent && v == "" {
		return
	}
	p.set(rel, v)
}

func (p *proj) strs(rel string, v []string) {
	f, in := p.classify(rel)
	if !in {
		return
	}
	if f.Null == EmptyIsAbsent && len(v) == 0 {
		return
	}
	items := make([]any, 0, len(v))
	for _, s := range v {
		items = append(items, s)
	}
	p.set(rel, items)
}

// f64 emits whenever the pointer is set, including for a zero value. Absent
// and zero are different behaviours; see NullSemantics.
func (p *proj) f64(rel string, v *float64) {
	_, in := p.classify(rel)
	if !in || v == nil {
		return
	}
	p.set(rel, *v)
}

func (p *proj) i64(rel string, v *int64) {
	_, in := p.classify(rel)
	if !in || v == nil {
		return
	}
	p.set(rel, *v)
}

func (p *proj) time(rel string, v *metav1.Time) {
	_, in := p.classify(rel)
	if !in || v == nil || v.IsZero() {
		return
	}
	p.set(rel, v.UTC().Format("2006-01-02T15:04:05Z"))
}
