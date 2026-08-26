// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

// Package diff explains what changed between two AgentManifests.
//
// The audience is an on-call engineer at 2am who needs to know which of an
// agent's behavioural inputs moved, and which of those could plausibly be the
// cause of what they are looking at. Everything here is arranged around that:
// a fixed plane order so the eye lands in the same place every time, explicit
// naming of what did *not* change, and a one-line verdict at the bottom.
package diff

import (
	"fmt"
	"sort"
	"strings"
)

// Plane is a group of behavioural inputs. The order of these constants is the
// order they render in, and it never varies between runs.
type Plane int

const (
	PlaneCode Plane = iota
	PlaneModel
	PlanePrompts
	PlaneTools
	PlaneRetrieval
	PlanePolicy
	PlaneJudges
)

// AllPlanes in render order.
var AllPlanes = []Plane{PlaneCode, PlaneModel, PlanePrompts, PlaneTools, PlaneRetrieval, PlanePolicy, PlaneJudges}

func (p Plane) String() string {
	switch p {
	case PlaneCode:
		return "code"
	case PlaneModel:
		return "model"
	case PlanePrompts:
		return "prompts"
	case PlaneTools:
		return "tools"
	case PlaneRetrieval:
		return "retrieval"
	case PlanePolicy:
		return "policy"
	default:
		return "judges"
	}
}

// MarshalJSON renders a plane by name. The JSON shape is a supported interface
// that CI will branch on, and an ordinal would silently change meaning the
// first time a plane is inserted.
func (p Plane) MarshalJSON() ([]byte, error) {
	return []byte(`"` + p.String() + `"`), nil
}

// UnmarshalJSON accepts the name form.
func (p *Plane) UnmarshalJSON(b []byte) error {
	name := strings.Trim(string(b), `"`)
	for _, candidate := range AllPlanes {
		if candidate.String() == name {
			*p = candidate
			return nil
		}
	}
	return fmt.Errorf("unknown plane %q", name)
}

// Op is what happened to an element.
type Op string

const (
	OpChanged Op = "changed"
	OpAdded   Op = "added"
	OpRemoved Op = "removed"
)

// Tag says what kind of consequence a change has. A change may carry several.
type Tag string

const (
	// TagModel: the model itself, or how it is sampled, changed.
	TagModel Tag = "model"
	// TagInput: what the model sees changed — a prompt, a tool description, the
	// retrievable document set.
	TagInput Tag = "input"
	// TagExec: what runs changed, without necessarily reaching the model.
	TagExec Tag = "exec"
	// TagGate: how the candidate is *scored* changed. Gating on a moved
	// yardstick is its own failure mode, so this is always called out.
	TagGate Tag = "gate"
	// TagSecurity: the change is also a security event. A rewritten tool
	// description is the shape of an MCP tool-poisoning or rug-pull attack.
	TagSecurity Tag = "security"
)

// Explain renders a tag the way it reads in the terminal.
func (t Tag) Explain() string {
	switch t {
	case TagModel:
		return "affects model output"
	case TagInput:
		return "affects model input"
	case TagExec:
		return "affects execution"
	case TagGate:
		return "affects how the GATE scores"
	case TagSecurity:
		return "security-relevant"
	}
	return string(t)
}

// Change is one difference between the two manifests.
type Change struct {
	Plane Plane `json:"plane"`
	// Element names the tool, prompt or judge this change belongs to. Empty on
	// planes that have no elements.
	Element string `json:"element,omitempty"`
	// Field is the leaf that moved, e.g. "temperature" or "description".
	Field string `json:"field,omitempty"`
	Op    Op     `json:"op"`
	From  string `json:"from,omitempty"`
	To    string `json:"to,omitempty"`
	Tags  []Tag  `json:"tags,omitempty"`
	// Detail carries lines that qualify the change: the effect classification
	// of a tool, a stale calibration, a widened effect.
	Detail []string `json:"detail,omitempty"`
	// Severity marks a change that must not be skimmed past.
	Severity bool `json:"severity,omitempty"`
}

func (c Change) has(t Tag) bool {
	for _, x := range c.Tags {
		if x == t {
			return true
		}
	}
	return false
}

// Verdict is the bottom line, and the part that actually gets read.
type Verdict string

const (
	// VerdictIdentical: both digests match. The same agent, byte for byte.
	VerdictIdentical Verdict = "identical"
	// VerdictProvenanceOnly: contentDigest moved, behaviorDigest did not. The
	// same agent from a different place — a registry migration, a prompt moved
	// between repositories, a re-measured judge calibration. This is the case
	// the dual digest exists to serve: it promotes with no canary.
	VerdictProvenanceOnly Verdict = "provenance-only"
	// VerdictBehavioural: behaviorDigest moved. This needs a canary.
	VerdictBehavioural Verdict = "behavioural"
	// VerdictCrossAgent: the two manifests are versions of different agents,
	// so no comparison between them means anything.
	VerdictCrossAgent Verdict = "cross-agent"
)

// Digests carries both sides of one hash.
type Digests struct {
	A string `json:"a"`
	B string `json:"b"`
}

// Result is the whole comparison, and the JSON output shape.
type Result struct {
	SchemaVersion string `json:"schemaVersion"`

	AgentA string `json:"agentA"`
	AgentB string `json:"agentB"`

	NameA string `json:"nameA,omitempty"`
	NameB string `json:"nameB,omitempty"`

	BehaviorDigest Digests `json:"behaviorDigest"`
	ContentDigest  Digests `json:"contentDigest"`

	Verdict Verdict  `json:"verdict"`
	Changes []Change `json:"changes"`
}

// ChangedPlanes lists the planes carrying at least one change, in render order.
func (r *Result) ChangedPlanes() []Plane {
	seen := map[Plane]bool{}
	for _, c := range r.Changes {
		seen[c.Plane] = true
	}
	var out []Plane
	for _, p := range AllPlanes {
		if seen[p] {
			out = append(out, p)
		}
	}
	return out
}

// UnchangedPlanes is rendered explicitly. An engineer reading a diff at 2am
// needs to know a plane was checked and found equal, which is a different fact
// from it being absent from the output.
func (r *Result) UnchangedPlanes() []Plane {
	seen := map[Plane]bool{}
	for _, c := range r.Changes {
		seen[c.Plane] = true
	}
	var out []Plane
	for _, p := range AllPlanes {
		if !seen[p] {
			out = append(out, p)
		}
	}
	return out
}

// ReachModel counts the changes an operator should treat as capable of moving
// the agent's output.
func (r *Result) ReachModel() int {
	n := 0
	for _, c := range r.Changes {
		if c.has(TagInput) || c.has(TagModel) {
			n++
		}
	}
	return n
}

// ChangeGate counts the changes that move the yardstick rather than the agent.
func (r *Result) ChangeGate() int {
	n := 0
	for _, c := range r.Changes {
		if c.has(TagGate) {
			n++
		}
	}
	return n
}

// ExitCode maps a verdict onto a process exit status.
//
// 0/1/2 are the three verdicts, so CI can branch on "did behaviour change?"
// without parsing anything. Errors live in the sysexits range instead of at 3,
// because wrapper scripts routinely special-case small integers and a tool that
// returns 2 for both "different" and "broken" gets misread.
func (v Verdict) ExitCode() int {
	switch v {
	case VerdictIdentical:
		return 0
	case VerdictProvenanceOnly:
		return 1
	case VerdictBehavioural:
		return 2
	default:
		// A cross-agent comparison produced no verdict, so it must not return
		// one. CI branching on 0/1/2 would otherwise read a refusal as
		// "behaviour changed" and act on it.
		return ExitUsage
	}
}

// Exit codes outside the verdict range, following sysexits.h.
const (
	ExitUsage       = 64 // EX_USAGE
	ExitUnavailable = 69 // EX_UNAVAILABLE — cluster or network unreachable
	ExitInternal    = 70 // EX_SOFTWARE
)

// sortChanges puts changes into render order: plane, then element name, then
// the order fields were emitted by the differ (which is spec order, not
// alphabetical, because spec order is how people read a manifest).
func sortChanges(in []Change) {
	sort.SliceStable(in, func(i, j int) bool {
		if in[i].Plane != in[j].Plane {
			return in[i].Plane < in[j].Plane
		}
		return in[i].Element < in[j].Element
	})
}

// abbrevImage shortens an image reference for display. A digest-pinned
// reference is 70-odd characters of hex that nobody verifies by eye, and two of
// them on one line pushes everything else off the terminal.
func abbrevImage(ref string) string {
	if i := strings.Index(ref, "@"); i >= 0 {
		return ref[:i] + "@" + abbrev(ref[i+1:])
	}
	return ref
}

// abbrev shortens a sha256 for display. Digests are compared by full value and
// displayed short; showing 64 hex characters twice per line makes a diff
// unreadable and nobody verifies them by eye anyway.
func abbrev(s string) string {
	if strings.HasPrefix(s, "sha256:") && len(s) > 7+8 {
		return s[7:7+4] + "…"
	}
	return s
}

func fmtFloat(v *float64) string {
	if v == nil {
		return "(unset)"
	}
	return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%g", *v), "0"), ".")
}

func fmtInt(v *int64) string {
	if v == nil {
		return "(unset)"
	}
	return fmt.Sprintf("%d", *v)
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// element is the display label for a change that stands on its own line.
func (c Change) element() string { return c.Element }
