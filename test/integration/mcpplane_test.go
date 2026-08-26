// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package integration

import (
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/waveoffai/waveoff/api/v1alpha1"
	"github.com/waveoffai/waveoff/internal/cassette"
	"github.com/waveoffai/waveoff/internal/mcp"
	"github.com/waveoffai/waveoff/internal/replay"
	"github.com/waveoffai/waveoff/test/fixtures/fakeanthropic"
)

// mcpCandidate classifies the reference server's echo tool.
func mcpCandidate(effect v1alpha1.ToolEffect) *v1alpha1.AgentManifestSpec {
	return &v1alpha1.AgentManifestSpec{
		Agent: "support-agent",
		Tools: []v1alpha1.ToolRef{{Name: "echo", Effect: effect}},
	}
}

// recordOverMCP runs the real agent against the real reference MCP server,
// through the recorder, and returns the harness and session.
func recordOverMCP(t *testing.T) (*harness, string, string) {
	t.Helper()
	endpoint := startReferenceMCP(t)

	h := newHarnessWithTools(t, map[string]string{"everything": endpoint},
		fakeanthropic.Turn{ToolName: "echo", ToolInput: map[string]any{"message": "refund policy"}},
		fakeanthropic.Turn{Text: "Echoed."},
	)
	h.runAgent(t, "OTEL_ENABLED=1",
		"WAVEOFF_MCP_ENDPOINT=http://"+h.addr+"/mcp/everything")

	sessions := h.sessions(t)
	if len(sessions) != 1 {
		var ids []string
		for _, s := range sessions {
			ids = append(ids, s.SessionID)
		}
		t.Fatalf("expected one session, got %d (%v); the agent's MCP handshake must run inside its trace", len(sessions), ids)
	}
	return h, sessions[0].SessionID, endpoint
}

// TestMCPTrafficIsRecorded proves the tool plane end to end: a real agent, the
// real MCP protocol, the official reference server.
//
// A tool local to the agent process never leaves it, so it proves nothing
// about the MCP proxy. This is what exercises session headers, SSE-framed
// responses and the JSON-RPC shapes — all of which were got wrong first time.
func TestMCPTrafficIsRecorded(t *testing.T) {
	h, session, endpoint := recordOverMCP(t)

	spans := h.spans(t, session)

	var call *cassette.Span
	var sawList bool
	for _, s := range spans {
		switch s.String("mcp.method.name") {
		case "tools/call":
			call = s
		case "tools/list":
			sawList = true
		}
	}
	if !sawList {
		t.Error("no tools/list was recorded; discovery did not cross the proxy")
	}
	if call == nil {
		t.Fatalf("no tools/call was recorded; spans were %d", len(spans))
	}

	if got := call.String("mcp.tool.name"); got != "echo" {
		t.Errorf("tool name = %q", got)
	}
	// Without the contract digest there is nothing for drift detection to
	// compare, and the corpus rots invisibly.
	if !strings.HasPrefix(call.String(cassette.AttrToolContractDigest), "sha256:") {
		t.Error("the tool call did not record the contract the server advertised")
	}
	// Without the argument hash, replay cannot find this call again once the
	// model runs live and step indices shift.
	if !strings.HasPrefix(call.String(cassette.AttrToolArgsHash), "sha256:") {
		t.Error("the tool call did not record an argument hash")
	}
	if call.String("server.address") == "" {
		t.Error("the tool call did not record which server served it")
	}

	// The digest recorded through the proxy must equal the one computed by
	// asking the server directly. If those disagree, drift detection reports
	// drift on every unchanged tool.
	live, err := mcp.New(endpoint).ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range live {
		if tool.Name != "echo" {
			continue
		}
		want, err := tool.ContractDigest()
		if err != nil {
			t.Fatal(err)
		}
		if got := call.String(cassette.AttrToolContractDigest); got != want {
			t.Errorf("contract digest recorded through the proxy does not match the server's:\n  recorded %s\n  live     %s", got, want)
		}
	}
}

// TestDriftDetectionAgainstARealServer closes the loop on §6's requirement: on
// every replay, compare the recorded contract against what the server
// advertises now.
func TestDriftDetectionAgainstARealServer(t *testing.T) {
	h, session, _ := recordOverMCP(t)
	idx, _ := indexFor(t, h, session)

	report, err := replay.CheckDrift(context.Background(), idx, replay.LiveTools)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Tools) == 0 {
		t.Fatal("drift checking found no tools to check")
	}
	// Nothing changed between recording and checking, so the corpus must be
	// reported as current. A drift check that flags an unchanged server is
	// worse than none: every session would be excluded and no gate could run.
	if report.Stale() {
		t.Errorf("an unchanged server was reported as drifted: %+v", report.Tools)
	}
	if report.Unknown() {
		t.Errorf("the check could not be completed: %+v", report.Tools)
	}
}

// TestReplayServesAToolCallFromCassette is the property the whole offline
// evaluation rests on: the agent makes a real MCP call and the replayer answers
// it from the recording, with the live server never involved.
func TestReplayServesAToolCallFromCassette(t *testing.T) {
	h, session, _ := recordOverMCP(t)

	idx, serving := indexFor(t, h, session)
	tracker := replay.NewTracker(idx, replay.ModeModelLiveToolsReplayed, "sha256:candidate")

	// The same scripted model behaviour, so the agent walks the same path.
	provider := httptest.NewServer(fakeanthropic.New(
		fakeanthropic.Turn{ToolName: "echo", ToolInput: map[string]any{"message": "refund policy"}},
		fakeanthropic.Turn{Text: "Echoed."},
	))
	defer provider.Close()

	srv, err := replay.NewServer(replay.ServerConfig{
		Index: idx, Reader: serving,
		Policy:        replay.NewPolicy(replay.ModeModelLiveToolsReplayed, mcpCandidate(v1alpha1.EffectRead), nil),
		Tracker:       tracker,
		ModelUpstream: provider.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// No MCP server is running for the replayer to forward to. If anything
	// reaches a live server, this fails rather than silently succeeding.
	out := runAgentAgainst(t, ts.URL, ts.URL+"/mcp/everything")
	t.Logf("agent during replay:\n%s", out)

	report := srv.Finish()
	var servedTool bool
	for _, step := range report.Steps {
		if step.Kind == cassette.KindTool && step.Decision.Action == replay.ActionServe {
			servedTool = true
		}
	}
	if !servedTool {
		t.Errorf("no tool call was served from the cassette; steps were %+v", report.Steps)
	}
	if !strings.Contains(out, "AGENT_OK") {
		t.Errorf("the agent did not complete against the replayer:\n%s", out)
	}
}

// TestReplayNoOpsAnMCPWrite is the safety property, exercised over the real
// protocol rather than asserted on a policy object.
func TestReplayNoOpsAnMCPWrite(t *testing.T) {
	h, session, _ := recordOverMCP(t)

	idx, serving := indexFor(t, h, session)
	tracker := replay.NewTracker(idx, replay.ModeModelLiveToolsReplayed, "sha256:candidate")

	provider := httptest.NewServer(fakeanthropic.New(
		fakeanthropic.Turn{ToolName: "echo", ToolInput: map[string]any{"message": "refund policy"}},
		fakeanthropic.Turn{Text: "Echoed."},
	))
	defer provider.Close()

	// Reclassify echo as a write. Nothing else changes.
	srv, err := replay.NewServer(replay.ServerConfig{
		Index: idx, Reader: serving,
		Policy:        replay.NewPolicy(replay.ModeModelLiveToolsReplayed, mcpCandidate(v1alpha1.EffectWrite), nil),
		Tracker:       tracker,
		ModelUpstream: provider.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	out := runAgentAgainst(t, ts.URL, ts.URL+"/mcp/everything")
	t.Logf("agent during replay:\n%s", out)

	report := srv.Finish()
	if report.NoOps == 0 {
		t.Errorf("a write-classified MCP tool was executed during replay; steps were %+v", report.Steps)
	}
}

// runAgentAgainst drives the fixture at a given model and MCP endpoint.
//
// Bounded on purpose. A replay bug that leaves a JSON-RPC client waiting for a
// response it will never correlate does not fail — it hangs, which without a
// deadline takes the whole suite down with it and hides which test was at
// fault.
func runAgentAgainst(t *testing.T, modelURL, mcpURL string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, python(t), filepath.Join("..", "fixtures", "langgraph", "agent.py"))
	cmd.Env = append(os.Environ(),
		"ANTHROPIC_BASE_URL="+modelURL,
		"ANTHROPIC_API_KEY=sk-ant-fake-key-for-tests",
		"WAVEOFF_MCP_ENDPOINT="+mcpURL,
		"OTEL_ENABLED=1")

	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("the agent did not finish within the deadline, which usually means a replayed "+
			"response was never correlated to its request:\n%s", out)
	}
	_ = err
	return string(out)
}
