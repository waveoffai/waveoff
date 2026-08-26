// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package replay_test

import (
	"context"
	"strings"
	"testing"

	"github.com/waveoffai/waveoff/internal/cassette"
	"github.com/waveoffai/waveoff/internal/corpus"
	"github.com/waveoffai/waveoff/internal/replay"
)

func outputStore(t *testing.T) corpus.Store {
	t.Helper()
	s, err := corpus.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestReplayWritesWhatTheCandidateProduced is the gap that made a gate
// impossible: a divergence report says where the candidate left the recorded
// path, not what it said. A judge scoring "did this complete the task?" needs
// the latter, and nothing was writing it down.
func TestReplayWritesWhatTheCandidateProduced(t *testing.T) {
	idx, reader := buildCassette(t)
	store := outputStore(t)
	ctx := context.Background()

	out, err := replay.NewOutput(ctx, store, idx.Header(), "sess-1", replay.ArmCandidate, nil)
	if err != nil {
		t.Fatal(err)
	}

	policy := replay.NewPolicy(replay.ModeFail, manifest(), nil)
	tracker := replay.NewTracker(idx, replay.ModeFail, h(2))
	srv, err := replay.NewServer(replay.ServerConfig{
		Index: idx, Reader: reader, Policy: policy, Tracker: tracker, Output: out,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := serveTest(t, srv)

	post(t, ts.URL, modelReq1)
	post(t, ts.URL+"/mcp/docs", toolCall("docs.search", searchArgs))
	srv.Finish()
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}

	// The output is a cassette, readable by the same tooling as a recording.
	f, err := store.Open(ctx, replay.OutputName("sess-1", replay.ArmCandidate))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r, err := cassette.NewReader(f, nil)
	if err != nil {
		t.Fatal(err)
	}

	header := r.Header()
	// The pairing key must be recoverable from the artifact itself.
	if header.SourceSession != "sess-1" {
		t.Errorf("sourceSession = %q; without it a scorer cannot pair the two arms", header.SourceSession)
	}
	if header.Arm != string(replay.ArmCandidate) {
		t.Errorf("arm = %q", header.Arm)
	}

	spans, err := r.All()
	if err != nil {
		t.Fatal(err)
	}
	if len(spans) != 2 {
		t.Fatalf("output has %d spans, want 2", len(spans))
	}

	// What the agent was actually given back is the thing a judge scores.
	body, _ := spans[0].Attributes[cassette.AttrResponseBody].(string)
	if !strings.Contains(body, "tool_use") {
		t.Errorf("the output does not carry what the replayer answered with:\n%s", body)
	}

	// And how each step was handled, because a score over a session where
	// tools were no-op'd means something different from one where they ran.
	if spans[0].String("waveoff.replay.action") != string(replay.ActionServe) {
		t.Errorf("the output does not record how the step was handled: %+v", spans[0].Attributes)
	}
}

// TestBothArmsCoexist: the two sides of a comparison are stored side by side
// and neither overwrites the other.
func TestBothArmsCoexist(t *testing.T) {
	store := outputStore(t)
	ctx := context.Background()

	for _, arm := range []replay.ArmLabel{replay.ArmIncumbent, replay.ArmCandidate} {
		idx, reader := buildCassette(t)
		out, err := replay.NewOutput(ctx, store, idx.Header(), "sess-1", arm, nil)
		if err != nil {
			t.Fatalf("%s: %v", arm, err)
		}
		srv, err := replay.NewServer(replay.ServerConfig{
			Index: idx, Reader: reader,
			Policy:  replay.NewPolicy(replay.ModeFail, manifest(), nil),
			Tracker: replay.NewTracker(idx, replay.ModeFail, h(2)),
			Output:  out,
		})
		if err != nil {
			t.Fatal(err)
		}
		ts := serveTest(t, srv)
		post(t, ts.URL, modelReq1)
		srv.Finish()
		out.Close()
	}

	headers, err := store.List(ctx, corpus.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(headers) != 2 {
		t.Fatalf("got %d outputs, want one per arm", len(headers))
	}
	seen := map[string]bool{}
	for _, h := range headers {
		seen[h.Arm] = true
		if h.SourceSession != "sess-1" {
			t.Errorf("output %s lost its pairing key", h.SessionID)
		}
	}
	if !seen["incumbent"] || !seen["candidate"] {
		t.Errorf("arms = %v", seen)
	}
}

// TestOutputIsOptional: a divergence-only run should not be forced to write an
// artifact nobody is going to score.
func TestOutputIsOptional(t *testing.T) {
	idx, reader := buildCassette(t)
	srv, err := replay.NewServer(replay.ServerConfig{
		Index: idx, Reader: reader,
		Policy:  replay.NewPolicy(replay.ModeFail, manifest(), nil),
		Tracker: replay.NewTracker(idx, replay.ModeFail, h(2)),
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := serveTest(t, srv)
	if _, body := post(t, ts.URL, modelReq1); !strings.Contains(body, "tool_use") {
		t.Errorf("body = %s", body)
	}
}
