// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package rollout

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/waveoffai/waveoff/internal/analysis"
	"github.com/waveoffai/waveoff/internal/cassette"
	"github.com/waveoffai/waveoff/internal/corpus"
	"github.com/waveoffai/waveoff/internal/score"
)

// CorpusObservations measures a live or shadow stage from recorded traffic.
//
// The measurement path is deliberately the same one an offline stage uses: the
// sessions are cassettes written by the recorder, selected from a corpus store,
// and handed to the scorer named in the stage's own spec. A live stage that
// measured through some other route would be comparing a candidate against an
// incumbent on a different instrument, and a promotion decision made that way
// says more about the two measurement paths than about the two agents.
//
// The pairing differs and cannot be made not to. In shadow, both arms saw the
// same request, so a candidate cassette names the incumbent session it was
// mirrored from and the comparison is paired. In a live canary each session is
// served by one arm only; there is no counterpart, and the comparison is
// unpaired and therefore wider. That is a property of the deployment, not of
// the analyzer — see docs/gating.md.
type CorpusObservations struct {
	// Store holds the sessions the recorder is writing as traffic arrives.
	Store corpus.Store

	// Dir is where those sessions live on disk, handed to the scorer so it can
	// open the cassettes it is being asked about.
	Dir string

	// Blobs is where large bodies were offloaded to, passed to the scorer so
	// it can resolve them.
	Blobs string

	// Scorer is built from the stage's ScorerSpec — the same spec, and so the
	// same scorer, an offline stage would use.
	Scorer score.Scorer

	// Limit caps how many sessions are scored per step, mirroring the corpus
	// selector's limit. Zero means everything recorded since the stage began.
	Limit int
}

var (
	_ ObservationSource = (*CorpusObservations)(nil)
	_ ActivitySource    = (*CorpusObservations)(nil)
)

// Since scores the sessions recorded since a stage started.
func (c *CorpusObservations) Since(ctx context.Context, agent, incumbent, candidate string,
	since time.Time) ([]analysis.Observation, analysis.Missingness, error) {

	headers, err := c.sessions(ctx, agent, incumbent, candidate, since)
	if err != nil {
		return nil, analysis.Missingness{}, err
	}
	if len(headers) == 0 {
		return nil, analysis.Missingness{}, nil
	}

	refs := make([]score.Ref, 0, len(headers))
	for _, h := range headers {
		arm := "incumbent"
		if h.BehaviorDigest == candidate {
			arm = "candidate"
		}
		refs = append(refs, score.Ref{
			Item:     itemOf(h),
			Arm:      arm,
			Session:  h.SessionID,
			Corpus:   c.Dir,
			Blobs:    c.Blobs,
			Manifest: h.BehaviorDigest,
		})
	}

	results, err := c.Scorer.Score(ctx, refs)
	if err != nil {
		// A scoring failure is infrastructure, not a verdict. Returning an
		// error holds the stage; returning no observations would read to the
		// gate as a quiet stretch of traffic, which is the opposite.
		return nil, analysis.Missingness{}, fmt.Errorf("scoring live sessions: %w", err)
	}
	if err := score.Validate(results); err != nil {
		return nil, analysis.Missingness{}, err
	}

	obs, missing := observationsFrom(results, len(refs))
	return obs, missing, nil
}

// Activity counts what each arm attempted to write.
//
// The counts come out of the cassettes rather than from the sidecar's live
// stats, because the cassette is the durable copy: a sidecar restart loses its
// counters, and a shadow finding that disappears when a pod cycles is not a
// finding anybody can act on.
func (c *CorpusObservations) Activity(ctx context.Context, agent, incumbent, candidate string,
	since time.Time) (analysis.Activity, analysis.Activity, error) {

	headers, err := c.sessions(ctx, agent, incumbent, candidate, since)
	if err != nil {
		return analysis.Activity{}, analysis.Activity{}, err
	}

	inc := analysis.Activity{Arm: "incumbent", Attempts: map[string]int{}}
	cand := analysis.Activity{Arm: "candidate", Attempts: map[string]int{}}

	for _, h := range headers {
		side := &inc
		if h.BehaviorDigest == candidate {
			side = &cand
		}
		side.Sessions++
		attempts, err := c.writeAttempts(ctx, h.SessionID)
		if err != nil {
			return analysis.Activity{}, analysis.Activity{}, err
		}
		for tool, n := range attempts {
			side.Attempts[tool] += n
		}
	}
	return inc, cand, nil
}

// writeAttempts reads one cassette and counts the tool calls that were
// suppressed.
//
// Only suppressed calls are counted, and only on the arm they were suppressed
// on. An incumbent serving real traffic writes for real, so its writes are not
// marked suppressed — which is why the incumbent side of a live comparison is
// counted from the effect classification instead.
func (c *CorpusObservations) writeAttempts(ctx context.Context, session string) (map[string]int, error) {
	rc, err := c.Store.Open(ctx, session)
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	out := map[string]int{}
	// No blob store: this reads attributes only, never a payload.
	reader, err := cassette.NewReader(rc, nil)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", session, err)
	}
	for {
		span, err := reader.Next()
		if err != nil || span == nil {
			break
		}
		tool := span.String("mcp.tool.name")
		if tool == "" {
			continue
		}
		// A call counts as a write attempt if it was suppressed, or if the
		// manifest classified it as writing. The first covers shadow; the
		// second covers a live arm whose writes actually happened.
		//
		// A refused call counts as neither. It was rejected because the
		// manifest does not classify the tool, so nobody knows whether it
		// writes — counting it would manufacture "new write classes" out of
		// gaps in the manifest rather than changes in the agent.
		if refused, _ := span.Attributes[cassette.AttrToolRefused].(bool); refused {
			continue
		}
		suppressed, _ := span.Attributes[cassette.AttrToolSuppressed].(bool)
		effect := span.String(cassette.AttrToolEffect)
		if suppressed || (effect != "" && effect != "read") {
			out[tool]++
		}
	}
	return out, nil
}

// sessions lists the recorded sessions belonging to either arm.
func (c *CorpusObservations) sessions(ctx context.Context, agent, incumbent, candidate string,
	since time.Time) ([]cassette.Header, error) {

	all, err := c.Store.List(ctx, corpus.Filter{Agent: agent, Since: since})
	if err != nil {
		return nil, err
	}
	out := make([]cassette.Header, 0, len(all))
	for _, h := range all {
		// A session that cannot name the manifest it ran against cannot be
		// attributed to an arm, and guessing would put the candidate's work on
		// the incumbent's side of the comparison.
		if h.BehaviorDigest != incumbent && h.BehaviorDigest != candidate {
			continue
		}
		out = append(out, h)
	}
	// Stable order so two steps over the same traffic produce the same item
	// sequence, which is what makes a re-run reproducible.
	sort.Slice(out, func(i, j int) bool { return out[i].SessionID < out[j].SessionID })
	if c.Limit > 0 && len(out) > c.Limit {
		out = out[:c.Limit]
	}
	return out, nil
}

// itemOf is the pairing key for a session.
//
// A shadow cassette names the session it was mirrored from, so both arms share
// an item and the comparison is paired. A live cassette has no counterpart, so
// it is its own item and the comparison is unpaired.
func itemOf(h cassette.Header) string {
	if h.SourceSession != "" {
		return h.SourceSession
	}
	return h.SessionID
}

// observationsFrom folds scorer output into the analyzer's shape.
//
// Items scored under both arms become paired observations; items scored under
// one become one-sided ones, which the unpaired path reads and the paired path
// drops. Deciding that here rather than by configuration means a shadow stage
// whose mirroring silently stopped shows up as unpaired data rather than as a
// paired comparison over half the traffic.
func observationsFrom(results []score.Result, attempted int) ([]analysis.Observation, analysis.Missingness) {
	byItem := map[string]*analysis.Observation{}
	order := []string{}

	for _, r := range results {
		if !r.Scored() {
			continue
		}
		o, ok := byItem[r.Item]
		if !ok {
			o = &analysis.Observation{Item: r.Item}
			byItem[r.Item] = o
			order = append(order, r.Item)
		}
		if r.Arm == "candidate" {
			o.Candidate = r.Metrics
		} else {
			o.Incumbent = r.Metrics
		}
	}

	obs := make([]analysis.Observation, 0, len(order))
	missing := analysis.Missingness{Attempted: attempted}
	for _, item := range order {
		o := *byItem[item]
		obs = append(obs, o)
		if len(o.Incumbent) > 0 && len(o.Candidate) > 0 {
			missing.BothScored++
		}
	}

	// Discordance is counted over items that were attempted under both arms.
	// An item only ever submitted once is not a dropped pair — in a live
	// canary that is every item, which is why the drop-rate check has to read
	// this and not a bare count of unscored results.
	attemptedBy := map[string]map[string]bool{}
	for _, r := range results {
		if attemptedBy[r.Item] == nil {
			attemptedBy[r.Item] = map[string]bool{}
		}
		attemptedBy[r.Item][r.Arm] = true
	}
	for item, arms := range attemptedBy {
		if !arms["incumbent"] || !arms["candidate"] {
			continue
		}
		o := byItem[item]
		gotInc := o != nil && len(o.Incumbent) > 0
		gotCand := o != nil && len(o.Candidate) > 0
		switch {
		case gotInc && gotCand:
			// counted above
		case gotInc:
			missing.CandidateOnlyFailed++
		case gotCand:
			missing.IncumbentOnlyFailed++
		default:
			missing.BothFailed++
		}
	}
	return obs, missing
}
