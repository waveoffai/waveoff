// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package integration

import (
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/waveoffai/waveoff/test/fixtures/fakeanthropic"
)

// scorerScript writes a scorer that reads refs and returns a fixed effect.
//
// It is the shape a real vendor adapter takes: refs on stdin, named metrics on
// stdout, nothing about Waveoff in its dependencies.
func scorerScript(t *testing.T, candidateEffect float64) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "score.py")
	body := fmt.Sprintf(`import json, sys, hashlib
req = json.load(sys.stdin)
out = []
for ref in req["refs"]:
    # A per-item base so the arms are correlated, which is what pairing removes.
    h = int(hashlib.sha256(ref["item"].encode()).hexdigest()[:8], 16) %% 1000 / 1000.0
    value = h * 0.4 + 0.3
    if ref["arm"] == "candidate":
        value += %f
    out.append({"item": ref["item"], "arm": ref["arm"],
                "metrics": {"task-completion": value, "policy-violations": 0}})
json.dump({"results": out}, sys.stdout)
`, candidateEffect)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func rolloutFile(t *testing.T, incumbent, candidate, scorer string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rollout.yaml")
	body := `apiVersion: waveoff.ai/v1alpha1
kind: AgentRollout
metadata:
  name: integration
spec:
  incumbentRef: ` + incumbent + `
  candidateRef: ` + candidate + `
  stages:
    - name: replay
      mode: offline-replay
      corpus:
        ref: corpus
      scorer:
        exec:
          command: python3
          args: ["` + scorer + `"]
        timeoutSeconds: 120
      gate:
        primary:
          metric: task-completion
          test: paired-bootstrap
          direction: higher-is-better
          margin: -0.02
          alpha: 0.05
        guardrails:
          - metric: policy-violations
            test: threshold
            direction: lower-is-better
            threshold: 0
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// recordCorpus records enough sessions for a gate to have something to decide
// on, driving a real agent each time.
func recordCorpus(t *testing.T, sessions int) *harness {
	t.Helper()
	h := newHarness(t,
		fakeanthropic.Turn{ToolName: "lookup_refund_policy", ToolInput: map[string]any{"topic": "laptops"}},
		fakeanthropic.Turn{Text: "Thirty days."},
	)
	for i := 0; i < sessions; i++ {
		// A distinct prompt per run, so each session is a distinct corpus item.
		h.runAgent(t, "OTEL_ENABLED=1",
			fmt.Sprintf("AGENT_PROMPT=refund policy question number %d", i))
	}
	return h
}

// TestRolloutPromotesAnEquivalentCandidate is the whole chain: a real agent
// recorded, replayed against both arms, scored by an external process and
// decided by the gate — driven the way an operator would drive it.
func TestRolloutPromotesAnEquivalentCandidate(t *testing.T) {
	h := recordCorpus(t, 32)

	provider := httptest.NewServer(fakeanthropic.New(
		fakeanthropic.Turn{ToolName: "lookup_refund_policy", ToolInput: map[string]any{"topic": "laptops"}},
		fakeanthropic.Turn{Text: "Thirty days."},
	))
	defer provider.Close()

	incumbent := writeManifest(t, candidate())
	cand := writeManifest(t, candidate())
	spec := rolloutFile(t, incumbent, cand, scorerScript(t, 0.0))

	out, code := runRollout(t, spec, h, provider.URL)
	t.Logf("%s", out)
	if code != 0 {
		t.Errorf("an equivalent candidate was not promoted (exit %d):\n%s", code, out)
	}
	if !strings.Contains(out, "promote") {
		t.Errorf("output should say promote:\n%s", out)
	}
}

// TestRolloutWavesOffARegression: the same chain, with a candidate the scorer
// rates materially worse.
func TestRolloutWavesOffARegression(t *testing.T) {
	h := recordCorpus(t, 32)

	provider := httptest.NewServer(fakeanthropic.New(
		fakeanthropic.Turn{ToolName: "lookup_refund_policy", ToolInput: map[string]any{"topic": "laptops"}},
		fakeanthropic.Turn{Text: "Thirty days."},
	))
	defer provider.Close()

	incumbent := writeManifest(t, candidate())
	cand := writeManifest(t, candidate())
	spec := rolloutFile(t, incumbent, cand, scorerScript(t, -0.25))

	out, code := runRollout(t, spec, h, provider.URL)
	t.Logf("%s", out)
	if code != 2 {
		t.Errorf("a 25-point regression exited %d, want 2:\n%s", code, out)
	}
	if !strings.Contains(out, "waved off") {
		t.Errorf("output should say waved off:\n%s", out)
	}
}

func runRollout(t *testing.T, specPath string, h *harness, modelUpstream string) (string, int) {
	t.Helper()
	binary := buildCLI(t)

	agent, err := filepath.Abs(filepath.Join("..", "fixtures", "langgraph", "agent.py"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "rollout", specPath,
		"--corpus", h.corpusDir,
		"--blobs", h.blobDir,
		"--output-corpus", t.TempDir(),
		"--model-upstream", modelUpstream,
		"--skip-drift-check",
		// The agent is what produces the output a scorer reads. Without it a
		// replay serves nothing and every session is correctly excluded.
		"--agent", python(t),
		"--agent", agent,
		"--no-color")
	cmd.Env = append(os.Environ(), "NO_COLOR=1", "OTEL_ENABLED=1",
		"ANTHROPIC_API_KEY=sk-ant-fake-key-for-tests")

	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running the rollout: %v\n%s", err, out)
	}
	return string(out), code
}
