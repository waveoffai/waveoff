// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

// Package integration drives the recorder with a real agent framework.
//
// Everything in the recorder's own package tests it against traffic we
// generated, which proves it matches our assumptions about what an agent
// framework does. This proves it against what one actually does: a genuine
// LangGraph agent, a genuine Anthropic SDK, a genuine tool-calling loop. Only
// the model is fake, which is the right place to draw the line — the recorder
// never sees a model, it sees HTTP.
package integration

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/waveoffai/waveoff/internal/cas"
	"github.com/waveoffai/waveoff/internal/cassette"
	"github.com/waveoffai/waveoff/internal/corpus"
	"github.com/waveoffai/waveoff/internal/recorder"
	"github.com/waveoffai/waveoff/test/fixtures/fakeanthropic"
)

// python locates an interpreter with the fixture's dependencies installed.
func python(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("WAVEOFF_PYTHON"); p != "" {
		return p
	}
	candidate := filepath.Join("..", "..", ".venv", "bin", "python")
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}
	t.Skip("no LangGraph environment; run: make langgraph-env")
	return ""
}

type harness struct {
	corpus    corpus.Store
	blobs     cas.Store
	corpusDir string
	blobDir   string
	sink      *recorder.CassetteSink
	api       *fakeanthropic.Server
	addr      string
}

func newHarness(t *testing.T, turns ...fakeanthropic.Turn) *harness {
	t.Helper()
	return newHarnessWithTools(t, nil, turns...)
}

// newHarnessWithTools additionally fronts MCP servers, so the tool plane is
// exercised over the wire rather than by a tool local to the agent process.
func newHarnessWithTools(t *testing.T, tools map[string]string, turns ...fakeanthropic.Turn) *harness {
	t.Helper()
	api := fakeanthropic.New(turns...)
	provider := httptest.NewServer(api)
	t.Cleanup(provider.Close)

	blobDir, corpusDir := t.TempDir(), t.TempDir()
	blobs, err := cas.NewFS(blobDir)
	if err != nil {
		t.Fatal(err)
	}
	store, err := corpus.NewFS(corpusDir)
	if err != nil {
		t.Fatal(err)
	}
	sink := recorder.NewCassetteSink(recorder.SinkConfig{
		Store: store, Blobs: blobs,
		Agent: "support-agent", BehaviorDigest: "sha256:" + strings.Repeat("a", 64),
		IdleTimeout: time.Second,
	})

	// Bind explicitly so the test knows the port.
	ln := listenLoopback(t)
	addr := ln.Addr().String()
	ln.Close()
	var srv *recorder.Server

	srv, err = recorder.NewServer(recorder.Config{
		ModelUpstream: provider.URL, Listen: addr, Sink: sink,
		ToolUpstreams: tools,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go srv.Serve(ctx)
	t.Cleanup(func() { cancel(); sink.Close() })

	waitReady(t, "http://"+addr+"/healthz")
	return &harness{
		corpus: store, blobs: blobs,
		corpusDir: corpusDir, blobDir: blobDir,
		sink: sink, api: api, addr: addr,
	}
}

func (h *harness) runAgent(t *testing.T, env ...string) string {
	t.Helper()
	cmd := exec.Command(python(t), filepath.Join("..", "fixtures", "langgraph", "agent.py"))
	cmd.Env = append(os.Environ(),
		"ANTHROPIC_BASE_URL=http://"+h.addr,
		"ANTHROPIC_API_KEY=sk-ant-fake-key-for-tests")
	cmd.Env = append(cmd.Env, env...)

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("agent failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "AGENT_OK") {
		t.Fatalf("agent did not complete:\n%s", out)
	}
	return string(out)
}

// sessions waits for cassettes to be flushed and returns them.
func (h *harness) sessions(t *testing.T) []cassette.Header {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		h.sink.Close() // idempotent; flushes
		got, err := h.corpus.List(context.Background(), corpus.Filter{})
		if err == nil && len(got) > 0 {
			return got
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("no cassettes were written")
	return nil
}

func (h *harness) spans(t *testing.T, sessionID string) []*cassette.Span {
	t.Helper()
	f, err := h.corpus.Open(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r, err := cassette.NewReader(f, h.blobs)
	if err != nil {
		t.Fatal(err)
	}
	spans, err := r.All()
	if err != nil {
		t.Fatal(err)
	}
	return spans
}

// TestInstrumentedAgentIsOneSession is the finding this fixture exists to pin
// down.
//
// The recorder derives session identity from W3C trace context. An agent whose
// HTTP client is instrumented propagates it, so a multi-step tool-calling loop
// is recorded as one correlated session — which is what divergence detection in
// replay needs. See TestUninstrumentedAgentDegrades for what happens without it.
func TestInstrumentedAgentIsOneSession(t *testing.T) {
	h := newHarness(t,
		fakeanthropic.Turn{ToolName: "lookup_refund_policy", ToolInput: map[string]any{"topic": "laptops"}},
		fakeanthropic.Turn{Text: "Laptops can be returned within 30 days."},
	)
	h.runAgent(t, "OTEL_ENABLED=1")

	got := h.sessions(t)
	if len(got) != 1 {
		t.Fatalf("an instrumented agent produced %d sessions, want 1; a multi-step loop must be one recording", len(got))
	}

	spans := h.spans(t, got[0].SessionID)
	if len(spans) != 2 {
		t.Fatalf("got %d spans, want 2 (the model call that asked for a tool, and the one after)", len(spans))
	}
	for i, s := range spans {
		if idx, ok := s.StepIndex(); !ok || idx != i {
			t.Errorf("span %d step index = (%d,%v)", i, idx, ok)
		}
		if s.Kind != cassette.KindModel {
			t.Errorf("span %d kind = %q", i, s.Kind)
		}
		if s.RequestHash() == "" {
			t.Errorf("span %d has no matching key; replay could not find it", i)
		}
	}
	// Each step must be distinguishable, or replay cannot tell them apart.
	if spans[0].RequestHash() == spans[1].RequestHash() {
		t.Error("both steps hashed identically; the matching key does not distinguish them")
	}
}

// TestUninstrumentedAgentDegrades documents the honest limitation.
//
// Without trace context there is no session boundary to derive, so each call
// becomes its own session. That still supports payload-level replay, but it
// cannot support divergence detection, because there is no recorded path to
// diverge from. This is a real constraint on adoption and it is better stated
// than discovered.
func TestUninstrumentedAgentDegrades(t *testing.T) {
	h := newHarness(t,
		fakeanthropic.Turn{ToolName: "lookup_refund_policy", ToolInput: map[string]any{"topic": "laptops"}},
		fakeanthropic.Turn{Text: "Thirty days."},
	)
	h.runAgent(t, "OTEL_ENABLED=0")

	got := h.sessions(t)
	if len(got) < 2 {
		t.Fatalf("expected an uninstrumented agent to fall back to one session per call, got %d", len(got))
	}
}

// TestRealFrameworkRequestShape checks the recorder captured what a manifest
// actually pins: the model, its decode parameters, and the tool contracts the
// framework declared.
func TestRealFrameworkRequestShape(t *testing.T) {
	h := newHarness(t,
		fakeanthropic.Turn{ToolName: "lookup_refund_policy", ToolInput: map[string]any{"topic": "laptops"}},
		fakeanthropic.Turn{Text: "Thirty days."},
	)
	h.runAgent(t, "OTEL_ENABLED=1")

	spans := h.spans(t, h.sessions(t)[0].SessionID)
	first := spans[0]

	if first.Attributes["gen_ai.request.model"] != "claude-sonnet-4-6" {
		t.Errorf("model = %v", first.Attributes["gen_ai.request.model"])
	}

	var req struct {
		Temperature *float64 `json:"temperature"`
		MaxTokens   *int     `json:"max_tokens"`
		Tools       []struct {
			Name string `json:"name"`
		} `json:"tools"`
		Messages []any `json:"messages"`
	}
	body, _ := first.Attributes[cassette.AttrRequestBody].(string)
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("captured request is not the JSON the SDK sent: %v\n%s", err, body)
	}
	if req.Temperature == nil || *req.Temperature != 0.2 {
		t.Errorf("temperature = %v; decode parameters must be captured, a manifest pins them", req.Temperature)
	}
	if req.MaxTokens == nil || *req.MaxTokens != 1024 {
		t.Errorf("max_tokens = %v", req.MaxTokens)
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "lookup_refund_policy" {
		t.Errorf("tools = %+v; the tool contracts the framework declared must be captured", req.Tools)
	}

	// The second call carries the loop's growing history, which is what makes
	// the two steps different inputs rather than a retry.
	var second struct {
		Messages []any `json:"messages"`
	}
	body2, _ := spans[1].Attributes[cassette.AttrRequestBody].(string)
	if err := json.Unmarshal([]byte(body2), &second); err != nil {
		t.Fatal(err)
	}
	if len(second.Messages) <= len(req.Messages) {
		t.Errorf("the second call has %d messages and the first %d; the agent loop should have grown the history",
			len(second.Messages), len(req.Messages))
	}
}

// TestStreamingThroughARealSDK exercises the path that matters most for
// perceived latency, driven by the Anthropic SDK's own stream parser rather
// than by a hand-rolled client.
func TestStreamingThroughARealSDK(t *testing.T) {
	h := newHarness(t, fakeanthropic.Turn{Text: "Laptops can be returned within thirty days."})
	h.runAgent(t, "OTEL_ENABLED=1", "AGENT_STREAM=1")

	spans := h.spans(t, h.sessions(t)[0].SessionID)
	if len(spans) == 0 {
		t.Fatal("nothing was recorded")
	}
	s := spans[0]
	if streamed, _ := s.Attributes[cassette.AttrStreamed].(bool); !streamed {
		t.Skip("the fixture did not stream; ChatAnthropic may not have used the streaming API")
	}
	chunks, _ := s.Attributes[cassette.AttrStreamChunks].(float64)
	if chunks < 2 {
		t.Errorf("stream chunks = %v; replay must reproduce the streaming shape", chunks)
	}
}

// TestCredentialsNeverReachTheCorpus is the property that decides whether a
// cassette can be committed or shared, checked against traffic a real SDK
// produced rather than one we wrote.
func TestCredentialsNeverReachTheCorpus(t *testing.T) {
	h := newHarness(t, fakeanthropic.Turn{Text: "ok"})
	h.runAgent(t, "OTEL_ENABLED=1")

	for _, header := range h.sessions(t) {
		f, err := h.corpus.Open(context.Background(), header.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := readAll(f)
		f.Close()
		if strings.Contains(string(raw), "sk-ant-fake-key-for-tests") {
			t.Errorf("the API key reached the cassette for session %s", header.SessionID)
		}
	}
}
