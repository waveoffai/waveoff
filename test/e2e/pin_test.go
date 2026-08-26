// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/waveoffai/waveoff/api/v1alpha1"
)

// TestPinAgainstALiveDeployment is the adoption path, end to end and with
// nothing faked.
//
// It is the case with the most moving parts and the least test coverage
// elsewhere: a real Deployment whose pod template carries a mutable tag, a real
// container runtime reporting an imageID, a real ConfigMap holding prompt
// bodies and an mcp.json, and a real MCP server answering a real
// initialize/tools/list handshake. The unit tests prove pin's logic against a
// fake client and a fake server written to my own reading of the protocol.
// This proves it against the protocol.
func TestPinAgainstALiveDeployment(t *testing.T) {
	applyFixtures(t)
	waitForDeployment(t, "mcp-everything")
	waitForDeployment(t, "support-agent")

	// pin talks to the MCP server over its cluster DNS name, which is only
	// resolvable from inside the cluster, so reach it through a port-forward
	// and point pin at the local end.
	local := portForward(t, "svc/mcp-everything", 3001)
	endpoint := "http://" + local + "/mcp"

	out := filepath.Join(t.TempDir(), "pinned.yaml")
	stdout, code := waveoff(t, "pin", "deployment/support-agent", "-n", namespace,
		"--container", "agent",
		"--agent", "support-agent",
		"--mcp-server", "everything="+endpoint,
		"-o", out)
	if code != 0 {
		t.Fatalf("pin failed (%d):\n%s", code, stdout)
	}

	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	yaml := string(body)
	t.Logf("pin emitted:\n%s", yaml)

	// The image must be the running digest, not the tag from the pod template.
	if strings.Contains(yaml, "busybox:1.36") {
		t.Error("pin recorded the mutable tag from the pod template instead of the running digest")
	}
	if !strings.Contains(yaml, "@sha256:") {
		t.Error("pin did not resolve a digest-pinned image; the manifest would be rejected at admission")
	}

	// Model configuration read out of the container environment.
	for _, want := range []string{"provider: anthropic", "id: claude-sonnet-4-6", "temperature: 0.2", "maxTokens: 4096"} {
		if !strings.Contains(yaml, want) {
			t.Errorf("expected %q in the emitted manifest", want)
		}
	}

	// Prompt bodies hashed out of the mounted ConfigMap, and only the ones that
	// are prompts.
	if !strings.Contains(yaml, "name: system") {
		t.Error("pin did not pick up system.md from the mounted ConfigMap")
	}
	if strings.Contains(yaml, "notes.bin") || strings.Contains(yaml, "name: notes") {
		t.Error("pin treated notes.bin as a prompt")
	}

	// Tool contracts pinned from the live server, with effect left for a human.
	if !strings.Contains(yaml, "name: echo") {
		t.Errorf("no tools were pinned from the live MCP server:\n%s", yaml)
	}
	if !strings.Contains(yaml, `effect: ""`) {
		t.Error("effect was filled in; it must be asserted by an operator, never inferred from the server")
	}
	if !strings.Contains(yaml, "untrusted") {
		t.Error("the emitted manifest should explain why the server's hints were not adopted")
	}
	if !strings.Contains(yaml, "NOT READY TO APPLY") {
		t.Error("a manifest with outstanding TODOs must say so")
	}

	// Unresolved, it must be rejected. This is the fail-closed rule reaching a
	// real cluster: a tool nobody classified can never be stored.
	if _, err := apply(t, out); err == nil {
		t.Error("a manifest with unclassified tools was admitted")
	}

	// Now walk the rest of the operator's path: classify the tools, seal, apply.
	resolved := strings.ReplaceAll(yaml, `effect: ""`, `effect: read`)
	if err := os.WriteFile(out, []byte(resolved), 0o644); err != nil {
		t.Fatal(err)
	}
	if o, c := waveoff(t, "verify", "--write", out); c != 0 {
		t.Fatalf("verify --write failed (%d):\n%s", c, o)
	}
	name, err := apply(t, out)
	if err != nil {
		t.Fatalf("the manifest pin produced was rejected after being resolved and sealed: %v", err)
	}

	stored := &v1alpha1.AgentManifest{}
	if err := k8s.Get(ctx(t), types.NamespacedName{Namespace: namespace, Name: name}, stored); err != nil {
		t.Fatal(err)
	}
	if len(stored.Spec.Tools) == 0 {
		t.Error("the stored manifest pins no tools")
	}
	for _, tool := range stored.Spec.Tools {
		if !strings.HasPrefix(tool.ContractDigest, "sha256:") {
			t.Errorf("tool %q has no contract digest", tool.Name)
		}
	}

	// And pinning the same unchanged Deployment again must produce the same
	// identity. If it did not, every re-pin would look like a release.
	second := filepath.Join(t.TempDir(), "again.yaml")
	if o, c := waveoff(t, "pin", "deployment/support-agent", "-n", namespace,
		"--container", "agent", "--agent", "support-agent",
		"--mcp-server", "everything="+endpoint, "-o", second); c != 0 {
		t.Fatalf("second pin failed (%d):\n%s", c, o)
	}
	againResolved := strings.ReplaceAll(readFile(t, second), `effect: ""`, `effect: read`)
	if err := os.WriteFile(second, []byte(againResolved), 0o644); err != nil {
		t.Fatal(err)
	}
	if o, c := waveoff(t, "verify", "--write", second); c != 0 {
		t.Fatalf("verify --write on the second pin failed (%d):\n%s", c, o)
	}
	if o, c := waveoff(t, "diff", out, second); c != 0 {
		t.Errorf("re-pinning an unchanged Deployment produced a different manifest (exit %d):\n%s", c, o)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func applyFixtures(t *testing.T) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("fixtures", "agent.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	body := strings.ReplaceAll(string(raw), "NAMESPACE", namespace)

	cmd := exec.Command("kubectl", "apply", "-n", namespace, "-f", "-")
	cmd.Stdin = strings.NewReader(body)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("applying fixtures: %v\n%s", err, out)
	}
}

func waitForDeployment(t *testing.T, name string) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Minute)
	for time.Now().Before(deadline) {
		dep := &appsv1.Deployment{}
		err := k8s.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: name}, dep)
		if err == nil && dep.Status.ReadyReplicas >= 1 {
			return
		}
		time.Sleep(2 * time.Second)
	}
	out, _ := exec.Command("kubectl", "describe", "deployment", name, "-n", namespace).CombinedOutput()
	t.Fatalf("deployment %s never became ready:\n%s", name, out)
}

// portForward opens a tunnel to an in-cluster service and returns the local
// address, torn down when the test ends.
func portForward(t *testing.T, target string, port int) string {
	t.Helper()
	cmd := exec.Command("kubectl", "port-forward", "-n", namespace, target, ":"+itoa(port))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	// kubectl prints "Forwarding from 127.0.0.1:54321 -> 3001".
	buf := make([]byte, 256)
	deadline := time.Now().Add(30 * time.Second)
	var seen string
	for time.Now().Before(deadline) {
		n, err := stdout.Read(buf)
		if n > 0 {
			seen += string(buf[:n])
			if i := strings.Index(seen, "127.0.0.1:"); i >= 0 {
				rest := seen[i:]
				if j := strings.IndexAny(rest, " \t\r\n"); j > 0 {
					return strings.TrimSpace(rest[:j])
				}
			}
		}
		if err != nil {
			break
		}
	}
	t.Fatalf("port-forward to %s never reported a local address; saw %q", target, seen)
	return ""
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
