// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package replay_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/waveoffai/waveoff/api/v1alpha1"
	"github.com/waveoffai/waveoff/internal/cassette"
	"github.com/waveoffai/waveoff/internal/recorder"
	"github.com/waveoffai/waveoff/internal/replay"
)

func h(n byte) string { return "sha256:" + strings.Repeat(string("0123456789abcdef"[n%16]), 64) }

// The exact bodies the tests send. The cassette is built by hashing these with
// the same function the recorder uses, so the fixture exercises real matching
// rather than agreeing with itself about made-up hashes.
const (
	modelReq1  = `{"model":"m","step":1}`
	modelReq2  = `{"model":"m","step":2}`
	searchArgs = `{"q":"refund"}`
	createArgs = `{"summary":"x"}`
)

func hashOf(body string) string { return recorder.NormalisedHash([]byte(body)) }

// buildCassette writes a session: a model call, a read, another model call, a
// write. That shape is the one the modes actually differ on.
func buildCassette(t *testing.T) (*replay.Index, *cassette.Reader) {
	t.Helper()
	var buf bytes.Buffer
	w := cassette.NewWriter(&buf, nil)
	header := cassette.Header{
		SessionID: "sess-1", Agent: "support-agent",
		BehaviorDigest: h(1), RecordedAt: time.Now(),
	}
	if err := w.WriteHeader(header); err != nil {
		t.Fatal(err)
	}

	steps := []*cassette.Span{
		{Name: "chat m", Kind: cassette.KindModel, Attributes: map[string]any{
			cassette.AttrRequestHash:    hashOf(modelReq1),
			cassette.AttrResponseBody:   `{"content":[{"type":"tool_use","name":"docs.search"}]}`,
			cassette.AttrUpstreamStatus: 200,
		}},
		{Name: "execute_tool docs.search", Kind: cassette.KindTool, Attributes: map[string]any{
			cassette.AttrRequestHash:        hashOf(toolCall("docs.search", searchArgs)),
			cassette.AttrToolArgsHash:       hashOf(searchArgs),
			cassette.AttrToolContractDigest: h(3),
			cassette.AttrResponseBody:       `{"jsonrpc":"2.0","result":{"content":[{"type":"text","text":"30 days"}]}}`,
			cassette.AttrUpstreamStatus:     200,
			"mcp.tool.name":                 "docs.search",
			"server.address":                "https://docs-gw.internal/mcp",
		}},
		{Name: "chat m", Kind: cassette.KindModel, Attributes: map[string]any{
			cassette.AttrRequestHash:    hashOf(modelReq2),
			cassette.AttrResponseBody:   `{"content":[{"type":"tool_use","name":"jira.create_issue"}]}`,
			cassette.AttrUpstreamStatus: 200,
		}},
		{Name: "execute_tool jira.create_issue", Kind: cassette.KindTool, Attributes: map[string]any{
			cassette.AttrRequestHash:        hashOf(toolCall("jira.create_issue", createArgs)),
			cassette.AttrToolArgsHash:       hashOf(createArgs),
			cassette.AttrToolContractDigest: h(4),
			cassette.AttrResponseBody:       `{"jsonrpc":"2.0","result":{"content":[]}}`,
			cassette.AttrUpstreamStatus:     200,
			"mcp.tool.name":                 "jira.create_issue",
			"server.address":                "https://jira-gw.internal/mcp",
		}},
	}
	for _, s := range steps {
		if err := w.WriteSpan(context.Background(), s); err != nil {
			t.Fatal(err)
		}
	}

	r, err := cassette.NewReader(bytes.NewReader(buf.Bytes()), nil)
	if err != nil {
		t.Fatal(err)
	}
	spans, err := r.All()
	if err != nil {
		t.Fatal(err)
	}
	// A fresh reader for serving, since All consumed the first one.
	serving, err := cassette.NewReader(bytes.NewReader(buf.Bytes()), nil)
	if err != nil {
		t.Fatal(err)
	}
	return replay.NewIndex(header, spans), serving
}

func manifest() *v1alpha1.AgentManifestSpec {
	return &v1alpha1.AgentManifestSpec{
		Agent: "support-agent",
		Tools: []v1alpha1.ToolRef{
			{Name: "docs.search", Effect: v1alpha1.EffectRead},
			{Name: "jira.create_issue", Effect: v1alpha1.EffectWrite},
		},
	}
}

func newServer(t *testing.T, mode replay.Mode, modelUpstream string, liveTools ...string) (*httptest.Server, *replay.Server) {
	t.Helper()
	idx, reader := buildCassette(t)
	policy := replay.NewPolicy(mode, manifest(), liveTools)
	tracker := replay.NewTracker(idx, mode, h(2))

	srv, err := replay.NewServer(replay.ServerConfig{
		Index: idx, Reader: reader, Policy: policy, Tracker: tracker,
		ModelUpstream: modelUpstream,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, srv
}

// serveTest starts a replay server on a test listener.
func serveTest(t *testing.T, srv *replay.Server) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func post(t *testing.T, url, body string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func toolCall(name, args string) string {
	return `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + name + `","arguments":` + args + `}}`
}

// TestFailModeReplaysEverything: nothing is allowed to differ, so the cassette
// answers every call and the first request not in it is the finding.
func TestFailModeReplaysEverything(t *testing.T) {
	ts, srv := newServer(t, replay.ModeFail, "")

	resp, body := post(t, ts.URL, modelReq1)
	if resp.Header.Get("X-Waveoff-Replay") != string(replay.ActionServe) {
		t.Errorf("model call was not served from the cassette: %s", resp.Header.Get("X-Waveoff-Replay"))
	}
	if !strings.Contains(body, "tool_use") {
		t.Errorf("body = %s", body)
	}

	report := srv.Report()
	if len(report.Steps) != 1 {
		t.Fatalf("steps = %d", len(report.Steps))
	}
}

// TestModelRunsLiveInTheEvaluationMode is the whole point of the primary mode:
// serving a recorded completion would test the cassette, not the candidate's
// prompts.
func TestModelRunsLiveInTheEvaluationMode(t *testing.T) {
	var reached int
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached++
		io.WriteString(w, `{"content":[{"type":"text","text":"live answer"}]}`)
	}))
	defer live.Close()

	ts, _ := newServer(t, replay.ModeModelLiveToolsReplayed, live.URL)
	resp, body := post(t, ts.URL, `{"model":"m","messages":[{"role":"user","content":"changed prompt"}]}`)

	if reached != 1 {
		t.Errorf("the live model was called %d times, want 1", reached)
	}
	if resp.Header.Get("X-Waveoff-Replay") != string(replay.ActionForward) {
		t.Errorf("model call was not forwarded: %s", resp.Header.Get("X-Waveoff-Replay"))
	}
	if !strings.Contains(body, "live answer") {
		t.Errorf("the agent did not receive the live response: %s", body)
	}
}

// TestReadsAreServedFromSnapshot: an offline evaluation must not depend on the
// state of the world at the moment it runs.
func TestReadsAreServedFromSnapshot(t *testing.T) {
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "{}")
	}))
	defer live.Close()

	ts, _ := newServer(t, replay.ModeModelLiveToolsReplayed, live.URL)
	// Get past the model step first.
	post(t, ts.URL, modelReq1)

	resp, body := post(t, ts.URL+"/mcp/docs", toolCall("docs.search", searchArgs))
	if resp.Header.Get("X-Waveoff-Replay") != string(replay.ActionServe) {
		t.Errorf("a read was not served from the recording: %s", resp.Header.Get("X-Waveoff-Replay"))
	}
	if !strings.Contains(body, "30 days") {
		t.Errorf("body = %s", body)
	}
}

// TestWritesNeverExecute is the property that makes evaluating a candidate
// against production traffic safe at all. It is the unsolved part of tool
// replay, and it must hold whether or not the call was matched.
func TestWritesNeverExecute(t *testing.T) {
	var reached int
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached++
		io.WriteString(w, "{}")
	}))
	defer live.Close()

	ts, srv := newServer(t, replay.ModeModelLiveToolsReplayed, live.URL)
	post(t, ts.URL, modelReq1)
	post(t, ts.URL+"/mcp/docs", toolCall("docs.search", searchArgs))

	resp, body := post(t, ts.URL+"/mcp/jira", toolCall("jira.create_issue", createArgs))

	if resp.Header.Get("X-Waveoff-Replay") != string(replay.ActionNoOp) {
		t.Errorf("a write tool was not no-op'd: %s", resp.Header.Get("X-Waveoff-Replay"))
	}
	// The agent must see a plausible success, and one honest about being
	// synthetic.
	if !strings.Contains(body, "was not executed") {
		t.Errorf("the synthesised result does not say nothing happened: %s", body)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d; a no-op should look like success to the agent", resp.StatusCode)
	}
	if srv.Report().NoOps != 1 {
		t.Errorf("noOps = %d", srv.Report().NoOps)
	}
}

// TestUnclassifiedToolFailsClosed: the mandatory effect field exists to stop a
// tool nobody classified running during an evaluation.
func TestUnclassifiedToolFailsClosed(t *testing.T) {
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "{}")
	}))
	defer live.Close()

	ts, srv := newServer(t, replay.ModeModelLiveToolsReplayed, live.URL)
	post(t, ts.URL, modelReq1)

	resp, body := post(t, ts.URL+"/mcp/x", toolCall("mystery.tool", `{}`))
	if resp.StatusCode == http.StatusOK {
		t.Error("a tool with no asserted effect was allowed to run")
	}
	if !strings.Contains(body, "no asserted effect") {
		t.Errorf("the refusal should say why: %s", body)
	}
	if srv.Report().Refused != 1 {
		t.Errorf("refused = %d", srv.Report().Refused)
	}
}

// TestDivergenceIsReportedAtTheFirstDeparture: everything after the first
// departure is downstream of it, so the report has to lead with where the
// paths separated.
func TestDivergenceIsReportedAtTheFirstDeparture(t *testing.T) {
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "{}")
	}))
	defer live.Close()

	ts, srv := newServer(t, replay.ModeModelLiveToolsReplayed, live.URL)
	post(t, ts.URL, modelReq1)
	// The recording calls docs.search here. The candidate does something else.
	post(t, ts.URL+"/mcp/jira", toolCall("jira.create_issue", createArgs))

	report := srv.Report()
	first := report.FirstDivergence()
	if first == nil {
		t.Fatalf("no divergence was reported; steps were %+v", report.Steps)
	}
	if first.Kind != replay.DivergedTool {
		t.Errorf("kind = %s, want %s", first.Kind, replay.DivergedTool)
	}
	if first.Step != 1 {
		t.Errorf("step = %d, want 1", first.Step)
	}
	if first.Recorded != "docs.search" || first.Replayed != "jira.create_issue" {
		t.Errorf("divergence = %+v", first)
	}
}

// TestIdenticalReplayDoesNotDiverge: a candidate that behaves identically must
// produce a clean report, or divergence detection cries wolf on every run.
func TestIdenticalReplayDoesNotDiverge(t *testing.T) {
	ts, srv := newServer(t, replay.ModeFail, "")

	post(t, ts.URL, modelReq1)
	post(t, ts.URL+"/mcp/docs", toolCall("docs.search", searchArgs))

	report := srv.Report()
	for _, d := range report.Divergences {
		t.Errorf("unexpected divergence: %+v", d)
	}
}

// TestMissingStepsAreReported: a candidate that stops early has diverged just
// as much as one that did something different.
func TestMissingStepsAreReported(t *testing.T) {
	ts, srv := newServer(t, replay.ModeFail, "")
	post(t, ts.URL, modelReq1)

	report := srv.Finish()
	var missing int
	for _, d := range report.Divergences {
		if d.Kind == replay.DivergedMissing {
			missing++
		}
	}
	if missing != 3 {
		t.Errorf("missing steps = %d, want 3 (the recording has four and the candidate took one)", missing)
	}
}

// TestRecordedFailuresAreReplayed: "the provider 529ed here" is exactly what an
// agent's retry path needs to meet again.
func TestRecordedFailuresAreReplayed(t *testing.T) {
	var buf bytes.Buffer
	w := cassette.NewWriter(&buf, nil)
	header := cassette.Header{SessionID: "s", BehaviorDigest: h(1), RecordedAt: time.Now()}
	w.WriteHeader(header)
	w.WriteSpan(context.Background(), &cassette.Span{
		Name: "chat m", Kind: cassette.KindModel,
		Attributes: map[string]any{
			cassette.AttrRequestHash:    hashOf(modelReq1),
			cassette.AttrResponseBody:   `{"error":{"type":"overloaded_error"}}`,
			cassette.AttrUpstreamStatus: 529,
		},
	})
	r, _ := cassette.NewReader(bytes.NewReader(buf.Bytes()), nil)
	spans, _ := r.All()
	serving, _ := cassette.NewReader(bytes.NewReader(buf.Bytes()), nil)

	idx := replay.NewIndex(header, spans)
	srv, err := replay.NewServer(replay.ServerConfig{
		Index: idx, Reader: serving,
		Policy:  replay.NewPolicy(replay.ModeFail, manifest(), nil),
		Tracker: replay.NewTracker(idx, replay.ModeFail, h(2)),
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, body := post(t, ts.URL, modelReq1)
	if resp.StatusCode != 529 {
		t.Errorf("status = %d, want the recorded 529", resp.StatusCode)
	}
	if !strings.Contains(body, "overloaded_error") {
		t.Errorf("body = %s", body)
	}
}

// TestReportIsNotUsableWhenCallsWereRefused: a replay full of refusals is not a
// passing replay, it is a replay that did not happen.
func TestReportIsNotUsableWhenCallsWereRefused(t *testing.T) {
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "{}")
	}))
	defer live.Close()

	ts, srv := newServer(t, replay.ModeModelLiveToolsReplayed, live.URL)
	post(t, ts.URL, modelReq1)
	post(t, ts.URL+"/mcp/x", toolCall("mystery.tool", `{}`))

	ok, why := srv.Finish().Usable()
	if ok {
		t.Error("a replay with refused calls was reported as usable")
	}
	if !strings.Contains(why, "refused") {
		t.Errorf("reason = %q", why)
	}
}

func TestModeValidation(t *testing.T) {
	for _, m := range replay.Modes {
		if _, err := replay.ParseMode(string(m)); err != nil {
			t.Errorf("%s: %v", m, err)
		}
	}
	if _, err := replay.ParseMode("whatever"); err == nil {
		t.Error("an unknown mode was accepted")
	}
}

// TestEvaluationModeNeedsAModelUpstream: the mode runs the model live, so
// starting without one would fail on the first call instead of at startup.
func TestEvaluationModeNeedsAModelUpstream(t *testing.T) {
	idx, reader := buildCassette(t)
	_, err := replay.NewServer(replay.ServerConfig{
		Index: idx, Reader: reader,
		Policy:  replay.NewPolicy(replay.ModeModelLiveToolsReplayed, manifest(), nil),
		Tracker: replay.NewTracker(idx, replay.ModeModelLiveToolsReplayed, h(2)),
	})
	if err == nil {
		t.Fatal("the evaluation mode started with no model upstream")
	}
}

func TestJSONReportShape(t *testing.T) {
	ts, srv := newServer(t, replay.ModeFail, "")
	post(t, ts.URL, modelReq1)

	b, err := json.Marshal(srv.Finish())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"session"`, `"mode"`, `"steps"`, `"divergences"`, `"recordedAgainst"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("report JSON missing %s:\n%s", want, b)
		}
	}
}
