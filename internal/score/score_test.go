// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package score_test

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/waveoffai/waveoff/internal/score"
)

func result(item, arm string, value float64) score.Result {
	return score.Result{Item: item, Arm: arm, Metrics: map[string]float64{"task-completion": value}}
}

// TestPairingIsByItem is the property the whole paired design rests on: the two
// arms are matched on the corpus item they were both driven from, never on the
// replay's own session id, which differs by construction.
func TestPairingIsByItem(t *testing.T) {
	results := []score.Result{
		result("sess-a", "incumbent", 0.9),
		result("sess-a", "candidate", 0.8),
		result("sess-b", "candidate", 1.0),
		result("sess-b", "incumbent", 1.0),
	}
	pairs, dropped := score.Pairs(results, "task-completion")
	if len(pairs) != 2 {
		t.Fatalf("pairs = %d, want 2", len(pairs))
	}
	if len(dropped) != 0 {
		t.Errorf("dropped = %v", dropped)
	}
	// Order must be deterministic, or a bootstrap seeded the same way gives
	// different answers on different runs.
	if pairs[0].Item != "sess-a" || pairs[1].Item != "sess-b" {
		t.Errorf("pairs are not in a stable order: %+v", pairs)
	}

	inc, cand := score.Values(pairs, "task-completion")
	if inc[0] != 0.9 || cand[0] != 0.8 {
		t.Errorf("values misaligned: %v %v", inc, cand)
	}
}

// TestIncompletePairsAreDroppedNotFilled: substituting a mean or a zero for a
// missing arm invents data exactly where the test is most sensitive to it.
func TestIncompletePairsAreDroppedNotFilled(t *testing.T) {
	results := []score.Result{
		result("sess-a", "incumbent", 0.9),
		result("sess-a", "candidate", 0.8),
		result("sess-b", "incumbent", 1.0), // candidate never scored
	}
	pairs, dropped := score.Pairs(results, "task-completion")
	if len(pairs) != 1 {
		t.Errorf("pairs = %d, want 1", len(pairs))
	}
	if len(dropped) != 1 || dropped[0] != "sess-b" {
		t.Errorf("dropped = %v, want [sess-b]; a half-scored item must be reported, not imputed", dropped)
	}
}

// TestAFailedScoreIsNotAZero is the single most important distinction here. A
// judge that timed out has not decided the agent failed, and counting it as 0.0
// is how a gate rolls back a good release because a scoring service was slow.
func TestAFailedScoreIsNotAZero(t *testing.T) {
	failed := score.Result{Item: "sess-a", Arm: "candidate", Error: "judge timed out"}
	if failed.Scored() {
		t.Error("a result carrying an error was treated as a measurement")
	}

	pairs, dropped := score.Pairs([]score.Result{
		result("sess-a", "incumbent", 0.9),
		failed,
	}, "task-completion")
	if len(pairs) != 0 {
		t.Errorf("a failed score was paired as if it were a measurement: %+v", pairs)
	}
	if len(dropped) != 1 {
		t.Errorf("dropped = %v", dropped)
	}
}

// TestNonFiniteMetricsAreRejected: a NaN reaching a bootstrap produces a
// confidence interval of NaN, which compares false against every threshold —
// a gate that silently never fires.
func TestNonFiniteMetricsAreRejected(t *testing.T) {
	for name, v := range map[string]float64{
		"NaN": math.NaN(), "+Inf": math.Inf(1), "-Inf": math.Inf(-1),
	} {
		err := score.Validate([]score.Result{{
			Item: "a", Arm: "candidate", Metrics: map[string]float64{"m": v},
		}})
		if err == nil {
			t.Errorf("%s was accepted as a metric", name)
		}
	}
}

func TestValidateRejectsUnusableResults(t *testing.T) {
	cases := map[string]score.Result{
		"no item":     {Arm: "candidate", Metrics: map[string]float64{"m": 1}},
		"unknown arm": {Item: "a", Arm: "control", Metrics: map[string]float64{"m": 1}},
		"empty arm":   {Item: "a", Metrics: map[string]float64{"m": 1}},
	}
	for name, r := range cases {
		if err := score.Validate([]score.Result{r}); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// TestExecScorer covers the universal adapter: anything that reads and writes
// JSON is a scorer, with no vendor in this repository's dependency graph.
func TestExecScorer(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "scorer.sh")
	script := `#!/bin/sh
# A scorer reads refs on stdin and writes results on stdout. This one is the
# shape a real vendor adapter takes, minus the vendor.
cat > /dev/null
cat <<'JSON'
{"results":[
  {"item":"sess-a","arm":"incumbent","metrics":{"task-completion":0.9}},
  {"item":"sess-a","arm":"candidate","metrics":{"task-completion":0.8},"metadata":{"judge":"opus-5"}}
]}
JSON
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	s := &score.ExecScorer{Command: "sh", Args: []string{path}, Timeout: 30 * time.Second}
	got, err := s.Score(context.Background(), []score.Ref{
		{Item: "sess-a", Arm: "incumbent", Session: "sess-a.incumbent"},
		{Item: "sess-a", Arm: "candidate", Session: "sess-a.candidate"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("results = %d", len(got))
	}
	if got[1].Metadata["judge"] != "opus-5" {
		t.Errorf("metadata was lost: %+v", got[1])
	}
}

// TestExecScorerReceivesRefs: the scorer is given references, not payloads. A
// transcript can be megabytes and a run covers hundreds of them.
func TestExecScorerReceivesRefs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "echo.sh")
	out := filepath.Join(dir, "stdin.json")
	script := "#!/bin/sh\ncat > " + out + "\necho '{\"results\":[]}'\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	s := &score.ExecScorer{Command: "sh", Args: []string{path}}
	if _, err := s.Score(context.Background(), []score.Ref{{
		Item: "sess-a", Arm: "candidate", Session: "sess-a.candidate",
		Corpus: "/corpus", Degraded: true, Reason: "two writes were synthesised",
	}}); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var sent struct {
		SchemaVersion string      `json:"schemaVersion"`
		Refs          []score.Ref `json:"refs"`
	}
	if err := json.Unmarshal(raw, &sent); err != nil {
		t.Fatalf("the scorer did not receive valid JSON: %v\n%s", err, raw)
	}
	if sent.SchemaVersion != score.SchemaVersion {
		t.Errorf("schema version = %q", sent.SchemaVersion)
	}
	// A scorer must be told the replay was degraded rather than left to infer
	// it: the same number means something different when half the tools were
	// no-op'd.
	if !sent.Refs[0].Degraded || sent.Refs[0].Reason == "" {
		t.Errorf("degradation was not passed to the scorer: %+v", sent.Refs[0])
	}
}

func TestExecScorerSurfacesFailures(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho 'judge unreachable' >&2\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := &score.ExecScorer{Command: "sh", Args: []string{path}}
	_, err := s.Score(context.Background(), []score.Ref{{Item: "a", Arm: "candidate"}})
	if err == nil {
		t.Fatal("a failing scorer was reported as success")
	}
	// The scorer's own diagnostics are the useful part.
	if !strings.Contains(err.Error(), "judge unreachable") {
		t.Errorf("the scorer's stderr was swallowed: %v", err)
	}
}

func TestExecScorerTimesOut(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "slow.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := &score.ExecScorer{Command: "sh", Args: []string{path}, Timeout: 300 * time.Millisecond}
	_, err := s.Score(context.Background(), []score.Ref{{Item: "a", Arm: "candidate"}})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("a hanging scorer must not hang a rollout: %v", err)
	}
}

func TestHTTPScorer(t *testing.T) {
	var seenAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		var req struct {
			Refs []score.Ref `json:"refs"`
		}
		json.NewDecoder(r.Body).Decode(&req)

		var out struct {
			Results []score.Result `json:"results"`
		}
		for _, ref := range req.Refs {
			out.Results = append(out.Results, score.Result{
				Item: ref.Item, Arm: ref.Arm,
				Metrics: map[string]float64{"task-completion": 0.75},
			})
		}
		json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()

	s := &score.HTTPScorer{Endpoint: srv.URL, Headers: map[string]string{"Authorization": "Bearer k"}}
	got, err := s.Score(context.Background(), []score.Ref{{Item: "a", Arm: "candidate"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Metrics["task-completion"] != 0.75 {
		t.Errorf("results = %+v", got)
	}
	if seenAuth != "Bearer k" {
		t.Error("authentication headers were not sent")
	}
}

// TestHTTPScorerFailureIsNotAnAbsenceOfScores: a scoring service being down is
// an infrastructure failure, not a verdict, and must not read as a pass.
func TestHTTPScorerFailureIsNotAnAbsenceOfScores(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream judge unavailable", http.StatusBadGateway)
	}))
	defer srv.Close()

	s := &score.HTTPScorer{Endpoint: srv.URL}
	if _, err := s.Score(context.Background(), []score.Ref{{Item: "a", Arm: "candidate"}}); err == nil {
		t.Fatal("a 502 from the scoring service was reported as success with no scores")
	}
}
