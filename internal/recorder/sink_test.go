// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package recorder_test

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/waveoffai/waveoff/internal/cas"
	"github.com/waveoffai/waveoff/internal/cassette"
	"github.com/waveoffai/waveoff/internal/corpus"
	"github.com/waveoffai/waveoff/internal/recorder"
)

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func sinkFor(t *testing.T) (*recorder.CassetteSink, corpus.Store, cas.Store) {
	t.Helper()
	store, err := corpus.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	blobs, err := cas.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := recorder.NewCassetteSink(recorder.SinkConfig{
		Store: store, Blobs: blobs, Log: quietLog(),
		Agent:          "support-agent",
		BehaviorDigest: "sha256:" + strings.Repeat("a", 64),
		ContentDigest:  "sha256:" + strings.Repeat("b", 64),
		IdleTimeout:    50 * time.Millisecond,
	})
	t.Cleanup(func() { s.Close() })
	return s, store, blobs
}

func rec(session string, step int, req, resp string) *recorder.Record {
	now := time.Now()
	return &recorder.Record{
		Session: session, Plane: recorder.PlaneModel, Step: step,
		Method: "POST", URL: "/v1/messages", Target: "https://api.anthropic.com",
		ReqBody: []byte(req), RespBody: []byte(resp), Status: 200,
		Start: now, End: now.Add(800 * time.Millisecond), Upstream: 790 * time.Millisecond,
	}
}

func TestSinkWritesACassette(t *testing.T) {
	s, store, blobs := sinkFor(t)
	ctx := context.Background()

	s.Record(rec("sess-a", 0, `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`,
		`{"content":[{"type":"text","text":"hello"}]}`))
	s.Record(rec("sess-a", 1, `{"model":"claude-sonnet-4-6","messages":[]}`, `{"content":[]}`))
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	f, err := store.Open(ctx, "sess-a")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	r, err := cassette.NewReader(f, blobs)
	if err != nil {
		t.Fatal(err)
	}
	h := r.Header()
	if h.Agent != "support-agent" {
		t.Errorf("agent = %q", h.Agent)
	}
	// A cassette that cannot name its manifest cannot be replayed meaningfully.
	if h.BehaviorDigest == "" || h.ContentDigest == "" {
		t.Error("the cassette does not pin the manifest it was recorded against")
	}

	spans, err := r.All()
	if err != nil {
		t.Fatal(err)
	}
	if len(spans) != 2 {
		t.Fatalf("got %d spans, want 2", len(spans))
	}
	if spans[0].Name != "chat claude-sonnet-4-6" {
		t.Errorf("span name = %q; the model should come from the request body", spans[0].Name)
	}
	if spans[0].Attributes["gen_ai.request.model"] != "claude-sonnet-4-6" {
		t.Error("the GenAI semantic convention attribute is missing")
	}
	// The matching key replay depends on.
	if spans[0].RequestHash() == "" {
		t.Error("no normalised request hash was recorded; replay could not match this")
	}
	for i, sp := range spans {
		if idx, ok := sp.StepIndex(); !ok || idx != i {
			t.Errorf("span %d has step index (%d,%v)", i, idx, ok)
		}
	}
}

// TestNormalisedHashIgnoresFormatting: a request that is semantically identical
// but differs in key order must still match its recording, or replay misses
// constantly for reasons unrelated to the agent.
func TestNormalisedHashIgnoresFormatting(t *testing.T) {
	a := recorder.NormalisedHash([]byte(`{"model":"m","temperature":1.0,"messages":[]}`))
	b := recorder.NormalisedHash([]byte("{\n  \"messages\": [],\n  \"temperature\": 1,\n  \"model\": \"m\"\n}"))
	if a != b {
		t.Errorf("reordering and reformatting changed the matching key:\n %s\n %s", a, b)
	}

	// But a real difference must change it.
	c := recorder.NormalisedHash([]byte(`{"model":"m","temperature":0.7,"messages":[]}`))
	if a == c {
		t.Error("a changed temperature did not change the matching key")
	}
}

// TestSinkDropsRatherThanBlocks is the rule the whole design rests on. Losing a
// recording is a bad day; adding latency to every model call is a product that
// does not get deployed.
func TestSinkDropsRatherThanBlocks(t *testing.T) {
	store, _ := corpus.NewFS(t.TempDir())
	s := recorder.NewCassetteSink(recorder.SinkConfig{
		Store: store, Log: quietLog(),
		QueueDepth:  1,
		IdleTimeout: time.Minute,
	})
	defer s.Close()

	// Far more than the queue can hold, from the request path.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 5000; i++ {
			s.Record(rec("flood", i, `{"model":"m"}`, `{}`))
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Record blocked; the proxy would have stalled every model call behind the sink")
	}

	_, dropped, _ := s.Stats()
	if dropped == 0 {
		t.Error("nothing was dropped under a flood; the queue bound is not doing anything")
	}
}

// TestSinkClosesIdleSessions: a session has no explicit end, the agent just
// stops calling, so quiet is the only signal a cassette can be finalised on.
func TestSinkClosesIdleSessions(t *testing.T) {
	s, store, _ := sinkFor(t)
	s.Record(rec("sess-idle", 0, `{"model":"m"}`, `{}`))

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		headers, err := store.List(context.Background(), corpus.Filter{})
		if err == nil && len(headers) == 1 {
			// Present in the listing means the header is readable, which is
			// what a later replay needs.
			if headers[0].SessionID == "sess-idle" {
				return
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("an idle session's cassette never became listable")
}

func TestSinkSeparatesSessions(t *testing.T) {
	s, store, blobs := sinkFor(t)
	for i := 0; i < 3; i++ {
		s.Record(rec("sess-x", i, `{"model":"m"}`, `{}`))
		s.Record(rec("sess-y", i, `{"model":"m"}`, `{}`))
	}
	s.Close()

	for _, id := range []string{"sess-x", "sess-y"} {
		f, err := store.Open(context.Background(), id)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		r, err := cassette.NewReader(f, blobs)
		if err != nil {
			t.Fatal(err)
		}
		spans, _ := r.All()
		f.Close()
		if len(spans) != 3 {
			t.Errorf("%s has %d spans, want 3; sessions are being mixed", id, len(spans))
		}
	}
}

func TestSinkRecordsFailures(t *testing.T) {
	s, store, blobs := sinkFor(t)
	r := rec("sess-err", 0, `{"model":"m"}`, `{"error":"overloaded"}`)
	r.Status = 529
	s.Record(r)
	s.Close()

	f, _ := store.Open(context.Background(), "sess-err")
	defer f.Close()
	cr, err := cassette.NewReader(f, blobs)
	if err != nil {
		t.Fatal(err)
	}
	spans, _ := cr.All()
	if len(spans) != 1 {
		t.Fatalf("got %d spans", len(spans))
	}
	// "The provider 529ed here" is exactly the kind of thing a replay needs to
	// reproduce, so a failed call is still a recording.
	if spans[0].Status.Code != "ERROR" {
		t.Errorf("a 529 was not recorded as an error: %+v", spans[0].Status)
	}
}
