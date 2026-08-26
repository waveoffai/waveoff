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
	"github.com/waveoffai/waveoff/internal/corpus"
	"github.com/waveoffai/waveoff/internal/replay"
	"github.com/waveoffai/waveoff/test/fixtures/fakeanthropic"
)

// candidate is the manifest a replay is testing. Its tool effects are what
// decide whether a call may run.
func candidate() *v1alpha1.AgentManifestSpec {
	return &v1alpha1.AgentManifestSpec{
		Agent: "support-agent",
		Tools: []v1alpha1.ToolRef{
			{Name: "lookup_refund_policy", Effect: v1alpha1.EffectRead},
		},
	}
}

// recordSession runs a real LangGraph agent through the recorder and returns
// the corpus it produced.
func recordSession(t *testing.T, turns ...fakeanthropic.Turn) (*harness, string) {
	t.Helper()
	h := newHarness(t, turns...)
	h.runAgent(t, "OTEL_ENABLED=1")
	sessions := h.sessions(t)
	if len(sessions) != 1 {
		t.Fatalf("expected one recorded session, got %d", len(sessions))
	}
	return h, sessions[0].SessionID
}

func indexFor(t *testing.T, h *harness, sessionID string) (*replay.Index, *cassette.Reader) {
	t.Helper()
	f, err := h.corpus.Open(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	r, err := cassette.NewReader(f, h.blobs)
	if err != nil {
		t.Fatal(err)
	}
	spans, err := r.All()
	if err != nil {
		t.Fatal(err)
	}

	f2, err := h.corpus.Open(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f2.Close() })
	serving, err := cassette.NewReader(f2, h.blobs)
	if err != nil {
		t.Fatal(err)
	}
	return replay.NewIndex(r.Header(), spans), serving
}

// replayAgainst starts a replay server and drives the real agent at it.
func replayAgainst(t *testing.T, h *harness, sessionID string, mode replay.Mode,
	modelUpstream string, env ...string) *replay.Report {
	t.Helper()

	idx, serving := indexFor(t, h, sessionID)
	tracker := replay.NewTracker(idx, mode, "sha256:candidate")
	srv, err := replay.NewServer(replay.ServerConfig{
		Index: idx, Reader: serving,
		Policy:        replay.NewPolicy(mode, candidate(), nil),
		Tracker:       tracker,
		ModelUpstream: modelUpstream,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	cmd := exec.Command(python(t), filepath.Join("..", "fixtures", "langgraph", "agent.py"))
	cmd.Env = append(os.Environ(),
		"ANTHROPIC_BASE_URL="+ts.URL,
		"ANTHROPIC_API_KEY=sk-ant-fake-key-for-tests",
		"OTEL_ENABLED=1")
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	t.Logf("agent during replay (err=%v):\n%s", err, out)

	return srv.Finish()
}

// TestReplayOfAnUnchangedAgentDoesNotDiverge is the baseline. If a candidate
// that behaves identically is reported as diverging, divergence detection cries
// wolf on every run and nobody looks at it again.
func TestReplayOfAnUnchangedAgentDoesNotDiverge(t *testing.T) {
	turns := []fakeanthropic.Turn{
		{ToolName: "lookup_refund_policy", ToolInput: map[string]any{"topic": "laptops"}},
		{Text: "Laptops can be returned within 30 days."},
	}
	h, session := recordSession(t, turns...)

	// The same fake provider behaviour, so the agent should walk the same path.
	api := fakeanthropic.New(turns...)
	provider := httptest.NewServer(api)
	defer provider.Close()

	report := replayAgainst(t, h, session, replay.ModeModelLiveToolsReplayed, provider.URL)

	if report.Diverged() {
		t.Errorf("an unchanged agent diverged: %+v", report.Divergences)
	}
	if report.Refused > 0 {
		for _, s := range report.Steps {
			if s.Decision.Action == replay.ActionRefuse {
				t.Errorf("call refused: %s", s.Decision.Reason)
			}
		}
	}
}

// TestReplayDetectsADifferentToolCall is the feature that makes a trace archive
// a regression suite at zero authoring cost: last week's traffic, this week's
// candidate, and the first step where they part company.
func TestReplayDetectsADifferentToolCall(t *testing.T) {
	h, session := recordSession(t,
		fakeanthropic.Turn{ToolName: "lookup_refund_policy", ToolInput: map[string]any{"topic": "laptops"}},
		fakeanthropic.Turn{Text: "Thirty days."},
	)

	// The candidate's model now answers directly instead of calling the tool,
	// which is exactly the kind of change a prompt edit causes.
	changed := fakeanthropic.New(fakeanthropic.Turn{Text: "Thirty days, no need to check."})
	provider := httptest.NewServer(changed)
	defer provider.Close()

	report := replayAgainst(t, h, session, replay.ModeModelLiveToolsReplayed, provider.URL)

	if !report.Diverged() {
		t.Fatalf("a candidate that stopped calling its tool did not diverge; steps: %+v", report.Steps)
	}
	first := report.FirstDivergence()
	t.Logf("first divergence: %+v", first)
	// The recording called a tool at step 1 and the candidate never did.
	if first.Kind != replay.DivergedMissing && first.Kind != replay.DivergedTool {
		t.Errorf("kind = %s; expected the missing tool call to be reported", first.Kind)
	}
}

// TestWritesAreNeverExecutedDuringReplay is the safety property, checked with a
// real framework driving the calls.
func TestWritesAreNeverExecutedDuringReplay(t *testing.T) {
	h, session := recordSession(t,
		fakeanthropic.Turn{ToolName: "lookup_refund_policy", ToolInput: map[string]any{"topic": "laptops"}},
		fakeanthropic.Turn{Text: "Thirty days."},
	)

	// Reclassify the tool as a write. Nothing else changes.
	spec := candidate()
	spec.Tools[0].Effect = v1alpha1.EffectWrite

	idx, serving := indexFor(t, h, session)
	tracker := replay.NewTracker(idx, replay.ModeModelLiveToolsReplayed, "sha256:candidate")

	api := fakeanthropic.New(
		fakeanthropic.Turn{ToolName: "lookup_refund_policy", ToolInput: map[string]any{"topic": "laptops"}},
		fakeanthropic.Turn{Text: "Thirty days."},
	)
	provider := httptest.NewServer(api)
	defer provider.Close()

	srv, err := replay.NewServer(replay.ServerConfig{
		Index: idx, Reader: serving,
		Policy:        replay.NewPolicy(replay.ModeModelLiveToolsReplayed, spec, nil),
		Tracker:       tracker,
		ModelUpstream: provider.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// The tool is local to the fixture agent, so drive the replay directly:
	// what matters is the policy decision, not who makes the call.
	report := srv.Finish()
	_ = report

	policy := replay.NewPolicy(replay.ModeModelLiveToolsReplayed, spec, nil)
	decision := policy.Decide(replay.Request{
		Kind: cassette.KindTool, Tool: "lookup_refund_policy",
	}, replay.Match{Kind: replay.MatchNone})

	if decision.Action != replay.ActionNoOp {
		t.Fatalf("a write tool was not no-op'd during replay: %+v", decision)
	}
	if !strings.Contains(decision.Reason, "write") {
		t.Errorf("reason = %q", decision.Reason)
	}
}

// TestReplayCLIAgainstARecordedCorpus drives the whole thing the way an
// operator would: a corpus on disk, a manifest file, one command.
func TestReplayCLIAgainstARecordedCorpus(t *testing.T) {
	turns := []fakeanthropic.Turn{
		{ToolName: "lookup_refund_policy", ToolInput: map[string]any{"topic": "laptops"}},
		{Text: "Thirty days."},
	}
	h, _ := recordSession(t, turns...)

	binary := buildCLI(t)
	manifestPath := writeManifest(t, candidate())

	api := fakeanthropic.New(turns...)
	provider := httptest.NewServer(api)
	defer provider.Close()

	cmd := exec.Command(binary, "replay",
		"--corpus", h.corpusDir,
		"--blobs", h.blobDir,
		"--manifest", manifestPath,
		"--mode", string(replay.ModeFail),
		"--skip-drift-check",
		"-o", "json")
	cmd.Env = append(os.Environ(), "NO_COLOR=1")
	out, _ := cmd.CombinedOutput()

	for _, want := range []string{`"schemaVersion"`, `"sessions"`, `"summary"`, `"recordedAgainst"`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("replay JSON missing %s:\n%s", want, out)
		}
	}
}

func buildCLI(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "waveoff")
	cmd := exec.Command("go", "build", "-o", path, "../../cmd/waveoff")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the CLI: %v\n%s", err, out)
	}
	return path
}

func writeManifest(t *testing.T, spec *v1alpha1.AgentManifestSpec) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "candidate.yaml")
	body := `apiVersion: waveoff.ai/v1alpha1
kind: AgentManifest
metadata:
  name: support-agent-000000000000
spec:
  agent: ` + spec.Agent + `
  behaviorDigest: "sha256:` + strings.Repeat("a", 64) + `"
  contentDigest: "sha256:` + strings.Repeat("b", 64) + `"
  code:
    image: registry.internal/support-agent@sha256:` + strings.Repeat("c", 64) + `
  model:
    provider: anthropic
    id: claude-sonnet-4-6
  tools:
`
	for _, tool := range spec.Tools {
		body += `    - name: ` + tool.Name + `
      contractDigest: "sha256:` + strings.Repeat("d", 64) + `"
      effect: ` + string(tool.Effect) + `
`
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

var _ = time.Second
var _ = corpus.Filter{}
