// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/waveoffai/waveoff/internal/diff"
)

// run executes the CLI the way main does and returns its output and exit code.
func run(t *testing.T, args ...string) (string, int) {
	t.Helper()
	cmd := Root()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)

	err := cmd.Execute()
	code := 0
	if err != nil {
		var v VerdictExit
		if errors.As(err, &v) {
			code = v.Code
		} else {
			code = diff.ExitUsage
			out.WriteString("error: " + err.Error())
		}
	}
	return out.String(), code
}

const unsealed = `apiVersion: waveoff.ai/v1alpha1
kind: AgentManifest
metadata:
  name: PLACEHOLDER
  namespace: default
spec:
  agent: support-agent
  behaviorDigest: ""
  contentDigest: ""
  code:
    image: registry.internal/support-agent@sha256:1111111111111111111111111111111111111111111111111111111111111111
  model:
    provider: anthropic
    id: claude-sonnet-4-6
    pin: "2026-05-01"
    params:
      temperature: 0.2
  tools:
    - name: docs.search
      server: https://docs-gw.internal/mcp
      contractDigest: sha256:2222222222222222222222222222222222222222222222222222222222222222
      effect: read
      replayPolicy: snapshot
`

func write(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func sealedFile(t *testing.T, content string) string {
	t.Helper()
	path := write(t, "manifest.yaml", content)
	if out, code := run(t, "verify", "--write", path); code != 0 {
		t.Fatalf("verify --write failed (%d): %s", code, out)
	}
	return path
}

func TestVerifyFailsOnUnsealedManifest(t *testing.T) {
	path := write(t, "manifest.yaml", unsealed)
	out, code := run(t, "verify", path)

	if code == 0 {
		t.Fatalf("an unsealed manifest passed verification:\n%s", out)
	}
	for _, want := range []string{"FAILED", "behaviorDigest", "computed", "waveoff verify --write"} {
		if !strings.Contains(out, want) {
			t.Errorf("output should mention %q:\n%s", want, out)
		}
	}
}

func TestVerifyWriteThenVerifyPasses(t *testing.T) {
	path := sealedFile(t, unsealed)
	out, code := run(t, "verify", path)
	if code != 0 {
		t.Fatalf("a sealed manifest failed verification (%d):\n%s", code, out)
	}
	if !strings.Contains(out, "ok ") {
		t.Errorf("expected an ok line:\n%s", out)
	}
}

// TestVerifyWriteIsIdempotent: a pre-commit hook runs this on every commit, so
// a second run must be a no-op rather than a fresh diff.
func TestVerifyWriteIsIdempotent(t *testing.T) {
	path := sealedFile(t, unsealed)
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, code := run(t, "verify", "--write", path); code != 0 {
		t.Fatal("second --write failed")
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("verify --write is not idempotent:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}

func TestVerifyDetectsATamperedDigest(t *testing.T) {
	path := sealedFile(t, unsealed)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Change a value without resealing, the way a hand-edit would.
	tampered := strings.Replace(string(raw), "temperature: 0.2", "temperature: 0.9", 1)
	if err := os.WriteFile(path, []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}

	if out, code := run(t, "verify", path); code == 0 {
		t.Errorf("a hand-edited manifest passed verification:\n%s", out)
	}
}

func TestVerifyExplainShowsTheHashInput(t *testing.T) {
	path := sealedFile(t, unsealed)
	out, _ := run(t, "verify", "--explain", path)
	// A digest mismatch is otherwise two indistinguishable hex strings.
	if !strings.Contains(out, `"agent":"support-agent"`) {
		t.Errorf("--explain must print the canonical bytes:\n%s", out)
	}
	if !strings.Contains(out, "behaviorDigest covers") || !strings.Contains(out, "contentDigest covers") {
		t.Errorf("--explain must cover both digests:\n%s", out)
	}
}

// TestDiffExitCodes is the contract CI branches on.
func TestDiffExitCodes(t *testing.T) {
	a := sealedFile(t, unsealed)

	behavioural := sealedFile(t, strings.Replace(unsealed, "temperature: 0.2", "temperature: 0.7", 1))
	provenance := sealedFile(t, strings.Replace(unsealed,
		"registry.internal/support-agent@", "mirror.example.com/support-agent@", 1))

	cases := []struct {
		name string
		b    string
		code int
		want string
	}{
		{"identical", a, 0, "identical"},
		{"provenance", provenance, 1, "promotes without a canary"},
		{"behavioural", behavioural, 2, "behavioural change"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, code := run(t, "diff", a, tc.b)
			if code != tc.code {
				t.Errorf("exit %d, want %d:\n%s", code, tc.code, out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("output should contain %q:\n%s", tc.want, out)
			}
		})
	}
}

// TestCrossAgentDiffRefuses: a cross-agent comparison must not return a verdict
// exit code, or CI branching on 0/1/2 reads a refusal as "behaviour changed".
func TestCrossAgentDiffRefuses(t *testing.T) {
	a := sealedFile(t, unsealed)
	b := sealedFile(t, strings.Replace(unsealed, "agent: support-agent", "agent: billing-agent", 1))

	out, code := run(t, "diff", a, b)
	if code != diff.ExitUsage {
		t.Errorf("exit %d, want %d (usage):\n%s", code, diff.ExitUsage, out)
	}
	if !strings.Contains(out, "refusing to diff across agents") {
		t.Errorf("expected a refusal:\n%s", out)
	}

	forced, code := run(t, "diff", "--force", a, b)
	if code == diff.ExitUsage {
		t.Errorf("--force should produce a real verdict:\n%s", forced)
	}
}

func TestDiffJSONIsMachineReadable(t *testing.T) {
	a := sealedFile(t, unsealed)
	b := sealedFile(t, strings.Replace(unsealed, "temperature: 0.2", "temperature: 0.7", 1))

	out, code := run(t, "diff", "-o", "json", a, b)
	if code != 2 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	for _, want := range []string{
		`"schemaVersion": "waveoff.ai/diff/v1alpha1"`,
		`"verdict": "behavioural"`,
		`"plane": "model"`, // named, not an ordinal
	} {
		if !strings.Contains(out, want) {
			t.Errorf("JSON should contain %s:\n%s", want, out)
		}
	}
}

// TestDiffNeedsNoCluster: two local files must compare with no kubeconfig at
// all. At 2am the cluster is often exactly what is not reachable.
func TestDiffNeedsNoCluster(t *testing.T) {
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "does-not-exist"))
	t.Setenv("HOME", t.TempDir())

	a := sealedFile(t, unsealed)
	out, code := run(t, "diff", a, a)
	if code != 0 {
		t.Errorf("a file-to-file diff needed a cluster (exit %d):\n%s", code, out)
	}
}

func TestMultiDocumentSelector(t *testing.T) {
	path := sealedFile(t, unsealed)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	name := ""
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "  name: ") {
			name = strings.TrimSpace(strings.TrimPrefix(line, "  name:"))
		}
	}

	multi := write(t, "many.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm\n---\n"+string(raw))

	// Without a selector the file is unambiguous (one manifest), so this works.
	if out, code := run(t, "diff", multi, path); code != 0 {
		t.Errorf("exit %d:\n%s", code, out)
	}
	// And with one it still resolves.
	if out, code := run(t, "diff", multi+"#"+name, path); code != 0 {
		t.Errorf("selector form failed (exit %d):\n%s", code, out)
	}
}

func TestUnreadableFileIsAUsageError(t *testing.T) {
	out, code := run(t, "diff", "/nonexistent/a.yaml", "/nonexistent/b.yaml")
	if code != diff.ExitUsage {
		t.Errorf("exit %d, want %d:\n%s", code, diff.ExitUsage, out)
	}
}

// TestHelpCarriesNoUpsell: nothing in this tool is gated, so nothing in its
// output should suggest otherwise.
func TestHelpCarriesNoUpsell(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"diff", "--help"}, {"verify", "--help"}, {"pin", "--help"}} {
		out, _ := run(t, args...)
		for _, forbidden := range []string{"Enterprise", "enterprise", "upgrade", "Pro ", "license key"} {
			if strings.Contains(out, forbidden) {
				t.Errorf("%v help contains %q:\n%s", args, forbidden, out)
			}
		}
	}
}
