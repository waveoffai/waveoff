// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

// Package replay serves a recorded session back to an agent.
//
// It is the mirror image of the recorder: the same sidecar shape, the same
// session and step keying, but answering from a cassette instead of forwarding
// upstream. That symmetry is deliberate — an agent cannot tell the difference,
// which is what makes a replay a fair test of the agent rather than of the
// harness.
package replay

import (
	"fmt"
	"sort"

	"github.com/waveoffai/waveoff/internal/cassette"
)

// MatchKind says how a request was found in a cassette, which matters because
// a weaker match is weaker evidence.
type MatchKind string

const (
	// MatchExact: the step index and the normalised request hash both agree.
	// The agent is doing what it did before.
	MatchExact MatchKind = "exact"
	// MatchHash: the request is byte-identical after normalisation, but at a
	// different step. Something reordered.
	MatchHash MatchKind = "hash"
	// MatchTool: the same tool with the same arguments, at a different point.
	// This is the workhorse in model-live-tools-replayed, where the model runs
	// live so step indices shift by design.
	MatchTool MatchKind = "tool"
	// MatchNone: nothing in the cassette corresponds to this request.
	MatchNone MatchKind = "none"
)

// Match is the result of looking a request up in a cassette.
type Match struct {
	Span *cassette.Span
	Kind MatchKind
	// Step is the recorded step index the match came from, or -1 on a miss.
	Step int
}

// Found reports whether anything was matched.
func (m Match) Found() bool { return m.Kind != MatchNone }

// Request is what an agent asked for during replay, reduced to the parts that
// identify it.
type Request struct {
	// Step is where the agent is in its own sequence.
	Step int
	// Hash is the normalised request hash, computed the same way the recorder
	// computes it.
	Hash string
	// Kind narrows the search to model or tool spans.
	Kind cassette.Kind
	// Tool is the tool name, for tool-plane requests.
	Tool string
	// ArgsHash is the normalised hash of the tool arguments.
	ArgsHash string
}

// Index makes one cassette searchable.
//
// A cassette is a list, and replay needs to find things in it by three
// different keys depending on how much the candidate has been allowed to
// diverge. Building the index once per session keeps the lookup off the
// agent's latency path.
type Index struct {
	header cassette.Header
	spans  []*cassette.Span

	byStep map[int]*cassette.Span
	byHash map[string][]*cassette.Span
	byTool map[string][]*cassette.Span

	// consumed tracks which spans have already been served, so a cassette with
	// two identical calls hands back the first one first rather than the same
	// one twice.
	consumed map[string]bool
}

// NewIndex builds an index over a cassette's spans.
func NewIndex(header cassette.Header, spans []*cassette.Span) *Index {
	idx := &Index{
		header:   header,
		spans:    spans,
		byStep:   map[int]*cassette.Span{},
		byHash:   map[string][]*cassette.Span{},
		byTool:   map[string][]*cassette.Span{},
		consumed: map[string]bool{},
	}
	for _, s := range spans {
		if s.Kind == cassette.KindSession {
			continue
		}
		if step, ok := s.StepIndex(); ok {
			idx.byStep[step] = s
		}
		if h := s.RequestHash(); h != "" {
			idx.byHash[h] = append(idx.byHash[h], s)
		}
		if tool := s.String("mcp.tool.name"); tool != "" {
			idx.byTool[tool] = append(idx.byTool[tool], s)
		}
	}
	return idx
}

// Header returns the cassette header, which names the manifest the session was
// recorded against.
func (i *Index) Header() cassette.Header { return i.header }

// Spans returns every step span in recorded order.
func (i *Index) Spans() []*cassette.Span {
	out := make([]*cassette.Span, 0, len(i.spans))
	for _, s := range i.spans {
		if s.Kind != cassette.KindSession {
			out = append(out, s)
		}
	}
	sort.SliceStable(out, func(a, b int) bool {
		x, _ := out[a].StepIndex()
		y, _ := out[b].StepIndex()
		return x < y
	})
	return out
}

// Lookup finds the recorded response for a request.
//
// The three keys are tried strongest first, because how a request was matched
// is itself evidence. An exact match says the agent is on the recorded path; a
// tool match says it got to the same call by some other route; a miss says it
// is doing something that was never recorded, which in a strict mode is the
// finding rather than an error.
func (i *Index) Lookup(req Request) Match {
	// Strongest: same position, same bytes.
	if s, ok := i.byStep[req.Step]; ok && req.Hash != "" && s.RequestHash() == req.Hash {
		if !i.isConsumed(s) {
			i.consume(s)
			return Match{Span: s, Kind: MatchExact, Step: req.Step}
		}
	}

	// Same request, different position.
	if req.Hash != "" {
		if s := i.firstUnconsumed(i.byHash[req.Hash]); s != nil {
			step, _ := s.StepIndex()
			i.consume(s)
			return Match{Span: s, Kind: MatchHash, Step: step}
		}
	}

	// Same tool, same arguments. This is what carries
	// model-live-tools-replayed, where the model runs live and step indices
	// shift by design.
	if req.Kind == cassette.KindTool && req.Tool != "" {
		candidates := i.byTool[req.Tool]
		if req.ArgsHash != "" {
			for _, s := range candidates {
				if s.String(cassette.AttrToolArgsHash) == req.ArgsHash && !i.isConsumed(s) {
					step, _ := s.StepIndex()
					i.consume(s)
					return Match{Span: s, Kind: MatchTool, Step: step}
				}
			}
		}
		// Same tool, different arguments: still the same call in the sense
		// that matters for a read, and reported as a weaker match so a caller
		// can decide whether to trust it.
		if s := i.firstUnconsumed(candidates); s != nil {
			step, _ := s.StepIndex()
			i.consume(s)
			return Match{Span: s, Kind: MatchTool, Step: step}
		}
	}

	return Match{Kind: MatchNone, Step: -1}
}

func (i *Index) firstUnconsumed(spans []*cassette.Span) *cassette.Span {
	for _, s := range spans {
		if !i.isConsumed(s) {
			return s
		}
	}
	return nil
}

func (i *Index) isConsumed(s *cassette.Span) bool { return i.consumed[spanKey(s)] }
func (i *Index) consume(s *cassette.Span)         { i.consumed[spanKey(s)] = true }

func spanKey(s *cassette.Span) string {
	step, _ := s.StepIndex()
	return fmt.Sprintf("%s/%d", s.SpanID, step)
}
