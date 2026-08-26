// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package rollout_test

import (
	"context"
	"testing"
	"time"

	"github.com/waveoffai/waveoff/internal/cassette"
	"github.com/waveoffai/waveoff/internal/corpus"
	"github.com/waveoffai/waveoff/internal/rollout"
	"github.com/waveoffai/waveoff/internal/score"
)

const (
	incumbentDigest = "sha256:aaaa"
	candidateDigest = "sha256:bbbb"
)

// writeSession lays down one cassette, with the tool calls given as
// (name, effect, suppressed) triples.
func writeSession(t *testing.T, store *corpus.FS, h cassette.Header, calls ...toolCall) {
	t.Helper()
	wc, err := store.Create(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	w := cassette.NewWriter(wc, nil)
	if err := w.AdoptHeader(); err != nil {
		t.Fatal(err)
	}
	for i, c := range calls {
		attrs := map[string]any{
			cassette.AttrSessionID: h.SessionID,
			cassette.AttrStepIndex: i,
			"mcp.tool.name":        c.name,
		}
		if c.effect != "" {
			attrs[cassette.AttrToolEffect] = c.effect
		}
		if c.suppressed {
			attrs[cassette.AttrToolSuppressed] = true
		}
		span := &cassette.Span{
			Name: "tool " + c.name, Kind: cassette.KindTool,
			StartTime: h.RecordedAt, EndTime: h.RecordedAt, Attributes: attrs,
		}
		if err := w.WriteSpan(context.Background(), span); err != nil {
			t.Fatal(err)
		}
	}
	if err := wc.Close(); err != nil {
		t.Fatal(err)
	}
}

type toolCall struct {
	name       string
	effect     string
	suppressed bool
}

func liveStore(t *testing.T) (*corpus.FS, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := corpus.NewFS(dir)
	if err != nil {
		t.Fatal(err)
	}
	return store, dir
}

func header(session, digest string, at time.Time) cassette.Header {
	return cassette.Header{
		SchemaVersion:  cassette.SchemaVersion,
		SessionID:      session,
		Agent:          "support-agent",
		BehaviorDigest: digest,
		RecordedAt:     at,
	}
}

// mapScorer returns a fixed metric per session.
type mapScorer struct {
	byArm map[string]float64
	fail  map[string]bool
}

func (m *mapScorer) Score(_ context.Context, refs []score.Ref) ([]score.Result, error) {
	out := make([]score.Result, 0, len(refs))
	for _, r := range refs {
		res := score.Result{Item: r.Item, Arm: r.Arm}
		if m.fail[r.Session] {
			res.Error = "judge timed out"
		} else {
			res.Metrics = map[string]float64{"task-completion": m.byArm[r.Arm]}
		}
		out = append(out, res)
	}
	return out, nil
}

// TestShadowSessionsPairThroughTheSourceSession.
//
// Both arms saw the same request, and the candidate cassette names the session
// it was mirrored from. That name is the pairing key, and it is what makes a
// shadow comparison paired — and therefore narrower than a live one on the same
// amount of traffic.
func TestShadowSessionsPairThroughTheSourceSession(t *testing.T) {
	store, dir := liveStore(t)
	at := time.Now().Add(-time.Minute)

	for _, item := range []string{"req-1", "req-2"} {
		writeSession(t, store, header(item, incumbentDigest, at))
		h := header(item+"-shadow", candidateDigest, at)
		h.SourceSession = item
		writeSession(t, store, h)
	}

	src := &rollout.CorpusObservations{
		Store: store, Dir: dir,
		Scorer: &mapScorer{byArm: map[string]float64{"incumbent": 0.8, "candidate": 0.9}},
	}
	obs, missing, err := src.Since(context.Background(), "support-agent",
		incumbentDigest, candidateDigest, at.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(obs) != 2 {
		t.Fatalf("got %d observations, want 2 paired ones: %+v", len(obs), obs)
	}
	for _, o := range obs {
		if len(o.Incumbent) == 0 || len(o.Candidate) == 0 {
			t.Errorf("item %q is one-sided, so the mirroring did not pair: %+v", o.Item, o)
		}
	}
	if missing.BothScored != 2 {
		t.Errorf("BothScored = %d", missing.BothScored)
	}
}

// TestLiveSessionsAreUnpaired.
//
// A live canary serves each session from one arm. There is no counterpart, so
// each session is its own item and the comparison is unpaired — a property of
// the deployment, not a choice the analyzer makes.
func TestLiveSessionsAreUnpaired(t *testing.T) {
	store, dir := liveStore(t)
	at := time.Now().Add(-time.Minute)

	writeSession(t, store, header("s-1", incumbentDigest, at))
	writeSession(t, store, header("s-2", incumbentDigest, at))
	writeSession(t, store, header("s-3", candidateDigest, at))

	src := &rollout.CorpusObservations{
		Store: store, Dir: dir,
		Scorer: &mapScorer{byArm: map[string]float64{"incumbent": 0.8, "candidate": 0.9}},
	}
	obs, missing, err := src.Since(context.Background(), "support-agent",
		incumbentDigest, candidateDigest, at.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(obs) != 3 {
		t.Fatalf("got %d observations, want one per session", len(obs))
	}
	for _, o := range obs {
		if len(o.Incumbent) > 0 && len(o.Candidate) > 0 {
			t.Errorf("item %q was paired, but a live session has no counterpart", o.Item)
		}
	}
	// No item was attempted under both arms, so nothing was dropped: an
	// unpaired stage must not read as 100% differential missingness.
	if missing.Dropped() != 0 {
		t.Errorf("Dropped = %d on data that was never paired to begin with", missing.Dropped())
	}
}

// TestASessionThatCannotNameItsManifestIsNotAttributed.
//
// A recording with no behaviour digest, or one from a third version, belongs to
// neither arm. Guessing would put one arm's work on the other's side of the
// comparison, which is the one error no amount of statistics recovers from.
func TestASessionThatCannotNameItsManifestIsNotAttributed(t *testing.T) {
	store, dir := liveStore(t)
	at := time.Now().Add(-time.Minute)

	writeSession(t, store, header("s-1", incumbentDigest, at))
	writeSession(t, store, header("s-2", candidateDigest, at))
	writeSession(t, store, header("s-3", "", at))
	writeSession(t, store, header("s-4", "sha256:cccc", at))

	src := &rollout.CorpusObservations{
		Store: store, Dir: dir,
		Scorer: &mapScorer{byArm: map[string]float64{"incumbent": 0.8, "candidate": 0.9}},
	}
	obs, _, err := src.Since(context.Background(), "support-agent",
		incumbentDigest, candidateDigest, at.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(obs) != 2 {
		t.Fatalf("got %d observations; unattributable sessions were counted", len(obs))
	}
}

// TestWriteAttemptsComeOutOfTheCassettes.
//
// Not out of the sidecar's live counters: a pod restart loses those, and a
// shadow finding that disappears when a pod cycles is not one anybody can act
// on. The cassette is the durable copy.
func TestWriteAttemptsComeOutOfTheCassettes(t *testing.T) {
	store, dir := liveStore(t)
	at := time.Now().Add(-time.Minute)

	// The incumbent serves real traffic, so its writes actually happened and
	// are marked by effect rather than by suppression.
	writeSession(t, store, header("s-1", incumbentDigest, at),
		toolCall{name: "docs.search", effect: "read"},
		toolCall{name: "jira.create_issue", effect: "write"})

	// The candidate is in shadow, so its writes were suppressed.
	h := header("s-2", candidateDigest, at)
	h.SourceSession = "s-1"
	writeSession(t, store, h,
		toolCall{name: "docs.search", effect: "read"},
		toolCall{name: "jira.create_issue", effect: "write", suppressed: true},
		toolCall{name: "jira.delete_issue", effect: "write", suppressed: true})

	src := &rollout.CorpusObservations{Store: store, Dir: dir, Scorer: &mapScorer{}}
	inc, cand, err := src.Activity(context.Background(), "support-agent",
		incumbentDigest, candidateDigest, at.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	if inc.Attempts["docs.search"] != 0 {
		t.Error("a read was counted as a write attempt")
	}
	if inc.Attempts["jira.create_issue"] != 1 {
		t.Errorf("incumbent attempts = %v", inc.Attempts)
	}
	if cand.Attempts["jira.delete_issue"] != 1 {
		t.Errorf("candidate attempts = %v", cand.Attempts)
	}
	if inc.Sessions != 1 || cand.Sessions != 1 {
		t.Errorf("sessions = %d / %d", inc.Sessions, cand.Sessions)
	}
}

// TestAnArmThatFailedToScoreIsDiscordantNotAbsent.
//
// An item scored under one arm and not the other is the evidence of asymmetry
// that the missingness check reads. Counting it as simply absent hides the case
// where the candidate is the arm that keeps failing to score.
func TestAnArmThatFailedToScoreIsDiscordantNotAbsent(t *testing.T) {
	store, dir := liveStore(t)
	at := time.Now().Add(-time.Minute)

	for _, item := range []string{"req-1", "req-2"} {
		writeSession(t, store, header(item, incumbentDigest, at))
		h := header(item+"-shadow", candidateDigest, at)
		h.SourceSession = item
		writeSession(t, store, h)
	}

	src := &rollout.CorpusObservations{
		Store: store, Dir: dir,
		Scorer: &mapScorer{
			byArm: map[string]float64{"incumbent": 0.8, "candidate": 0.9},
			fail:  map[string]bool{"req-1-shadow": true},
		},
	}
	_, missing, err := src.Since(context.Background(), "support-agent",
		incumbentDigest, candidateDigest, at.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if missing.CandidateOnlyFailed != 1 {
		t.Errorf("CandidateOnlyFailed = %d, want 1", missing.CandidateOnlyFailed)
	}
	if missing.BothScored != 1 {
		t.Errorf("BothScored = %d, want 1", missing.BothScored)
	}
}
