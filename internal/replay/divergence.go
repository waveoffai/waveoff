// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package replay

import (
	"fmt"

	"github.com/waveoffai/waveoff/internal/cassette"
)

// DivergenceKind says how a replayed step departed from its recording.
type DivergenceKind string

const (
	// DivergedTool: the candidate called a different tool.
	DivergedTool DivergenceKind = "different-tool"
	// DivergedArguments: the same tool, called with different arguments.
	DivergedArguments DivergenceKind = "different-arguments"
	// DivergedInput: the model was sent a different request.
	DivergedInput DivergenceKind = "different-model-input"
	// DivergedExtra: the candidate took a step the recording does not have.
	DivergedExtra DivergenceKind = "extra-step"
	// DivergedMissing: the recording has a step the candidate never took.
	DivergedMissing DivergenceKind = "missing-step"
)

// Divergence is one departure from the recorded path.
type Divergence struct {
	Step int            `json:"step"`
	Kind DivergenceKind `json:"kind"`

	Recorded string `json:"recorded,omitempty"`
	Replayed string `json:"replayed,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// Step is one thing the candidate did during a replay.
type Step struct {
	Index int
	Kind  cassette.Kind
	Tool  string
	// Hash and ArgsHash are the normalised request and argument hashes.
	Hash     string
	ArgsHash string
	// Match records how this step was found in the cassette, if at all.
	Match MatchKind
	// Decision records what the replayer did with it.
	Decision Decision
}

// Report is the outcome of replaying one session.
type Report struct {
	Session string `json:"session"`
	Agent   string `json:"agent,omitempty"`
	Mode    Mode   `json:"mode"`

	// RecordedAgainst and ReplayedAgainst are the manifest identities of the
	// session and the candidate. A replay across two manifests that are
	// behaviourally identical should not diverge at all, which makes them
	// worth stating.
	RecordedAgainst string `json:"recordedAgainst,omitempty"`
	ReplayedAgainst string `json:"replayedAgainst,omitempty"`

	Steps       []Step       `json:"steps"`
	Divergences []Divergence `json:"divergences"`
	Drift       *DriftReport `json:"drift,omitempty"`

	// Refused counts calls the policy would not run. A replay full of refusals
	// is not a passing replay; it is a replay that did not happen.
	Refused int `json:"refused"`
	// NoOps counts write tools that were refused and synthesised.
	NoOps int `json:"noOps"`
}

// Diverged reports whether the candidate left the recorded path.
func (r *Report) Diverged() bool { return len(r.Divergences) > 0 }

// FirstDivergence is the one that matters. Everything after the first
// departure is downstream of it, so an operator reading a report needs the
// point where the paths separated, not a list of consequences.
func (r *Report) FirstDivergence() *Divergence {
	if len(r.Divergences) == 0 {
		return nil
	}
	return &r.Divergences[0]
}

// Usable reports whether this replay can be scored.
//
// A session whose contracts have drifted is scored against a fiction, and a
// session where the policy refused most calls did not really run. Both must be
// excluded from a gate rather than counted, which is the difference between a
// corpus that reports its own decay and one that rots silently.
func (r *Report) Usable() (bool, string) {
	if r.Drift != nil && r.Drift.Stale() {
		return false, "tool contracts changed since this session was recorded, so it would be scored against a fiction"
	}
	if r.Refused > 0 {
		return false, fmt.Sprintf("%d call(s) were refused, so the candidate did not complete the session", r.Refused)
	}
	if len(r.Steps) == 0 {
		return false, "nothing was replayed"
	}
	return true, ""
}

// Tracker accumulates a replay and works out where it departed from the
// recording.
type Tracker struct {
	idx    *Index
	report *Report
	next   int
}

// NewTracker starts a replay report.
func NewTracker(idx *Index, mode Mode, replayedAgainst string) *Tracker {
	h := idx.Header()
	return &Tracker{
		idx: idx,
		report: &Report{
			Session:         h.SessionID,
			Agent:           h.Agent,
			Mode:            mode,
			RecordedAgainst: h.BehaviorDigest,
			ReplayedAgainst: replayedAgainst,
			Steps:           []Step{},
			Divergences:     []Divergence{},
		},
	}
}

// Observe records one replayed step and compares it against the recording.
func (t *Tracker) Observe(req Request, match Match, decision Decision) {
	step := Step{
		Index: t.next, Kind: req.Kind, Tool: req.Tool,
		Hash: req.Hash, ArgsHash: req.ArgsHash,
		Match: match.Kind, Decision: decision,
	}
	t.report.Steps = append(t.report.Steps, step)

	switch decision.Action {
	case ActionRefuse:
		t.report.Refused++
	case ActionNoOp:
		t.report.NoOps++
	}

	t.compare(req, match)
	t.next++
}

// compare works out whether this step departed from the recorded path.
//
// The model plane is deliberately not compared on input in
// model-live-tools-replayed: the model runs live there precisely so it can
// produce different requests, and reporting that as a divergence would flag
// the intended behaviour of the mode on every single step.
func (t *Tracker) compare(req Request, match Match) {
	recorded := t.recordedAt(t.next)

	if recorded == nil {
		t.diverge(Divergence{
			Step: t.next, Kind: DivergedExtra,
			Replayed: describeRequest(req),
			Detail:   "the recording ends before this step",
		})
		return
	}

	recordedTool := recorded.String("mcp.tool.name")

	switch {
	case req.Kind == cassette.KindTool && recorded.Kind == cassette.KindTool && req.Tool != recordedTool:
		// The clearest signal there is: at this point the candidate did
		// something else.
		t.diverge(Divergence{
			Step: t.next, Kind: DivergedTool,
			Recorded: recordedTool, Replayed: req.Tool,
			Detail: "the candidate called a different tool",
		})

	case req.Kind != recorded.Kind:
		t.diverge(Divergence{
			Step: t.next, Kind: DivergedTool,
			Recorded: string(recorded.Kind), Replayed: string(req.Kind),
			Detail: "the candidate took a different kind of step",
		})

	case req.Kind == cassette.KindTool && req.ArgsHash != "" &&
		recorded.String(cassette.AttrToolArgsHash) != "" &&
		req.ArgsHash != recorded.String(cassette.AttrToolArgsHash):
		t.diverge(Divergence{
			Step: t.next, Kind: DivergedArguments,
			Recorded: short(recorded.String(cassette.AttrToolArgsHash)), Replayed: short(req.ArgsHash),
			Detail: fmt.Sprintf("%s was called with different arguments", req.Tool),
		})

	case req.Kind == cassette.KindModel && t.report.Mode == ModeFail &&
		req.Hash != "" && recorded.RequestHash() != "" && req.Hash != recorded.RequestHash():
		t.diverge(Divergence{
			Step: t.next, Kind: DivergedInput,
			Recorded: short(recorded.RequestHash()), Replayed: short(req.Hash),
			Detail: "the model was sent a different request",
		})
	}
}

// Finish closes the report, accounting for steps the recording has that the
// candidate never reached.
func (t *Tracker) Finish() *Report {
	recorded := t.idx.Spans()
	for i := t.next; i < len(recorded); i++ {
		s := recorded[i]
		t.diverge(Divergence{
			Step: i, Kind: DivergedMissing,
			Recorded: describeSpan(s),
			Detail:   "the recording has this step and the candidate stopped before it",
		})
	}
	return t.report
}

// Report returns the report as it stands.
func (t *Tracker) Report() *Report { return t.report }

// AttachDrift records the contract drift check on the report.
func (t *Tracker) AttachDrift(d *DriftReport) { t.report.Drift = d }

func (t *Tracker) recordedAt(step int) *cassette.Span {
	spans := t.idx.Spans()
	if step < 0 || step >= len(spans) {
		return nil
	}
	return spans[step]
}

// diverge appends a divergence. Everything after the first departure is
// downstream of it, so they are all recorded but the first is the one a
// report leads with.
func (t *Tracker) diverge(d Divergence) {
	t.report.Divergences = append(t.report.Divergences, d)
}

func describeRequest(req Request) string {
	if req.Kind == cassette.KindTool && req.Tool != "" {
		return cassette.OpTool + " " + req.Tool
	}
	return string(req.Kind)
}

func describeSpan(s *cassette.Span) string {
	if s.Name != "" {
		return s.Name
	}
	return string(s.Kind)
}

func short(hash string) string {
	if len(hash) > 7+8 {
		return hash[7:7+8] + "…"
	}
	return hash
}
