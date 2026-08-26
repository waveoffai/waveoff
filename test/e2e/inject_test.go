// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/waveoffai/waveoff/internal/webhook/inject"
)

// TestSidecarInjectionOnALivePod is the adoption path for the recorder, proved
// against a real cluster.
//
// The webhook's own tests check its logic against a fake client. What they
// cannot check is whether the resulting pod actually works: whether the
// rewritten base URL redirects real traffic, whether the sidecar starts under
// a read-only root filesystem, whether the volume it writes cassettes to is
// mounted where it thinks. All of those are wiring, and wiring is what breaks.
func TestSidecarInjectionOnALivePod(t *testing.T) {
	manifestName := applyManifestFor(t, "recorded-agent")
	applyInjectFixtures(t, manifestName)
	waitForDeployment(t, "fake-provider")
	waitForDeployment(t, "recorded-agent")

	pod := waitForInjectedPod(t)

	var agent, sidecar *corev1.Container
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == "agent" {
			agent = &pod.Spec.Containers[i]
		}
	}
	// A native sidecar lives among the init containers, which is what makes
	// the kubelet start it before the agent.
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == inject.SidecarName {
			sidecar = &pod.Spec.InitContainers[i]
		}
	}
	if sidecar == nil {
		t.Fatalf("no %s sidecar was injected; containers are %v", inject.SidecarName, containerNames(pod))
	}
	if agent == nil {
		t.Fatal("the agent container disappeared")
	}

	// The zero-code-change trick: the agent thinks it is talking to its
	// provider and is talking to the sidecar.
	var baseURL string
	for _, e := range agent.Env {
		if e.Name == "ANTHROPIC_BASE_URL" {
			baseURL = e.Value
		}
	}
	if !strings.Contains(baseURL, "127.0.0.1") {
		t.Errorf("ANTHROPIC_BASE_URL = %q; traffic would bypass the recorder entirely", baseURL)
	}

	// The manifest identity must have been resolved from the cluster, or the
	// corpus cannot be attributed to a version.
	args := strings.Join(sidecar.Args, " ")
	if !strings.Contains(args, "-behavior-digest") {
		t.Errorf("the sidecar was not told which manifest this agent runs: %v", sidecar.Args)
	}

	// The sidecar must actually be running, not merely present. A read-only
	// root filesystem and a non-root user are easy to get wrong and only fail
	// at runtime.
	waitForContainerReady(t, pod.Name, inject.SidecarName)

	// And the recording must have happened: cassettes on disk, inside the
	// cluster, written by a sidecar nobody configured by hand.
	sessions := waitForCassettes(t, pod.Name)
	t.Logf("sessions recorded in the pod: %v", sessions)

	// The fixed traceparent the agent sends means every call is one session.
	if len(sessions) != 1 {
		t.Errorf("got %d sessions, want 1: the agent sends a fixed traceparent, "+
			"so all its calls should correlate into one recording", len(sessions))
	}

	// A cassette exists as soon as its header is written, which is before any
	// span lands in it — so listing the corpus is not the same as the recording
	// having happened. Poll the contents rather than assert on whatever the
	// first dump happens to contain.
	body := waitForCassetteContaining(t, pod.Name, sessions[0], "claude-sonnet-4-6")

	if !strings.Contains(body, "waveoff.ai/cassette/") {
		t.Errorf("the cassette has no schema version:\n%s", truncate(body))
	}
	// Credentials must not survive into a cassette, checked on the artifact
	// that actually exists on disk in a cluster.
	if strings.Contains(body, "sk-ant-fake-key-for-tests") {
		t.Error("the API key reached a cassette written in the cluster")
	}
	if !strings.Contains(body, "REDACTED") {
		t.Errorf("nothing was recorded as redacted, though the agent sent an x-api-key header:\n%s", truncate(body))
	}
	// §5 pins the provider's resolved model version, which arrives as a
	// response header. Dropping headers would leave the manifest unable to
	// record what actually served a call.
	if !strings.Contains(body, "anthropic-version") {
		t.Errorf("the provider's version header was not recorded:\n%s", truncate(body))
	}
}

// TestPodWithoutOptInIsUntouched: this webhook sees every pod creation in the
// cluster, so leaving pods alone is the behaviour that matters most.
func TestPodWithoutOptInIsUntouched(t *testing.T) {
	// The fake-provider deployment from the fixtures carries no inject
	// annotation, so its pod is the control.
	waitForDeployment(t, "fake-provider")

	pods := &corev1.PodList{}
	sel, _ := labels.Parse("app=fake-provider")
	err := k8s.List(ctx(t), pods, client.InNamespace(namespace), client.MatchingLabelsSelector{Selector: sel})
	if err != nil {
		t.Fatal(err)
	}
	if len(pods.Items) == 0 {
		t.Fatal("no fake-provider pod")
	}
	for _, p := range pods.Items {
		for _, c := range append(append([]corev1.Container{}, p.Spec.InitContainers...), p.Spec.Containers...) {
			if c.Name == inject.SidecarName {
				t.Errorf("pod %s was injected without opting in", p.Name)
			}
		}
	}
}

// applyManifestFor creates a sealed AgentManifest for the injector to resolve.
func applyManifestFor(t *testing.T, agent string, edits ...func(string) string) string {
	t.Helper()
	path := manifestFile(t, agent, edits...)
	name := seal(t, path)
	if _, err := apply(t, path); err != nil {
		t.Fatalf("could not create the manifest the injector will look up: %v", err)
	}
	return name
}

func applyInjectFixtures(t *testing.T, manifestName string) {
	t.Helper()
	applyFixture(t, "inject.yaml", manifestName)
}

// applyFixture renders one fixture file into this run's namespace and applies
// it. The namespace is generated per run, so the fixtures carry placeholders
// rather than a name that would collide with a concurrent suite.
func applyFixture(t *testing.T, file, manifestName string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("fixtures", file))
	if err != nil {
		t.Fatal(err)
	}
	body := strings.NewReplacer(
		"NAMESPACE", namespace,
		"MANIFEST_NAME", manifestName,
	).Replace(string(raw))

	cmd := exec.Command("kubectl", "apply", "-n", namespace, "-f", "-")
	cmd.Stdin = strings.NewReader(body)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("applying %s: %v\n%s", file, err, out)
	}
}

func waitForInjectedPod(t *testing.T) *corev1.Pod {
	t.Helper()
	return waitForPodWithLabel(t, "app=recorded-agent")
}

// waitForPodWithLabel returns the first pod matching a selector that has an
// init container, which is how an injected pod is distinguishable from the one
// the Deployment controller has not yet replaced.
func waitForPodWithLabel(t *testing.T, selector string) *corev1.Pod {
	t.Helper()
	sel, _ := labels.Parse(selector)
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		pods := &corev1.PodList{}
		err := k8s.List(context.Background(), pods,
			client.InNamespace(namespace), client.MatchingLabelsSelector{Selector: sel})
		if err == nil {
			for i := range pods.Items {
				if len(pods.Items[i].Spec.InitContainers) > 0 {
					return &pods.Items[i]
				}
			}
		}
		time.Sleep(2 * time.Second)
	}
	out, _ := exec.Command("kubectl", "get", "pods", "-n", namespace, "-o", "wide").CombinedOutput()
	t.Fatalf("no injected pod appeared for %s:\n%s", selector, out)
	return nil
}

func waitForContainerReady(t *testing.T, pod, container string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		var p corev1.Pod
		err := k8s.Get(context.Background(), typesName(namespace, pod), &p)
		if err == nil {
			statuses := append(append([]corev1.ContainerStatus{},
				p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...)
			for _, cs := range statuses {
				if cs.Name == container && cs.Ready {
					return
				}
			}
		}
		time.Sleep(2 * time.Second)
	}
	out, _ := exec.Command("kubectl", "describe", "pod", pod, "-n", namespace).CombinedOutput()
	logs, _ := exec.Command("kubectl", "logs", pod, "-c", container, "-n", namespace).CombinedOutput()
	t.Fatalf("container %s never became ready\n--- describe ---\n%s\n--- logs ---\n%s", container, out, logs)
}

// waitForCassettes asks the recorder what it has captured.
//
// The sidecar image is distroless, so there is no shell and no ls to exec. The
// binary answers for itself, which is the only way an operator can interrogate
// a running recorder either.
func waitForCassettes(t *testing.T, pod string) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		out := kubectlExecAllowFail(t, pod, inject.SidecarName,
			inject.BinaryPath, "-corpus-dir", "/var/lib/waveoff/corpus", "-list")
		var sessions []string
		for _, line := range strings.Split(out, "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "{") {
				continue
			}
			var h struct {
				SessionID string `json:"sessionId"`
			}
			if err := json.Unmarshal([]byte(line), &h); err == nil && h.SessionID != "" {
				sessions = append(sessions, h.SessionID)
			}
		}
		if len(sessions) > 0 {
			return sessions
		}
		time.Sleep(5 * time.Second)
	}
	logs, _ := exec.Command("kubectl", "logs", pod, "-c", inject.SidecarName, "-n", namespace).CombinedOutput()
	agentLogs, _ := exec.Command("kubectl", "logs", pod, "-c", "agent", "-n", namespace).CombinedOutput()
	t.Fatalf("no cassettes were written in the cluster\n--- recorder ---\n%s\n--- agent ---\n%s", logs, agentLogs)
	return nil
}

// waitForCassetteContaining dumps one session until the needle appears.
//
// The sink is asynchronous by design — nothing on the request path waits for a
// recording to be written — so a cassette's header can be on disk while its
// spans are still in flight.
func waitForCassetteContaining(t *testing.T, pod, session, needle string) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	var body string
	for time.Now().Before(deadline) {
		body = kubectlExecAllowFail(t, pod, inject.SidecarName,
			inject.BinaryPath, "-corpus-dir", "/var/lib/waveoff/corpus", "-dump", session)
		if strings.Contains(body, needle) {
			return body
		}
		time.Sleep(3 * time.Second)
	}
	t.Fatalf("%q never appeared in session %s:\n%s", needle, session, truncate(body))
	return ""
}

func kubectlExec(t *testing.T, pod, container string, args ...string) string {
	t.Helper()
	full := append([]string{"exec", "-n", namespace, pod, "-c", container, "--"}, args...)
	out, err := exec.Command("kubectl", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl exec %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func kubectlExecAllowFail(t *testing.T, pod, container string, args ...string) string {
	t.Helper()
	full := append([]string{"exec", "-n", namespace, pod, "-c", container, "--"}, args...)
	out, _ := exec.Command("kubectl", full...).CombinedOutput()
	return string(out)
}

func containerNames(p *corev1.Pod) []string {
	var out []string
	for _, c := range p.Spec.InitContainers {
		out = append(out, "init:"+c.Name)
	}
	for _, c := range p.Spec.Containers {
		out = append(out, c.Name)
	}
	return out
}

func truncate(s string) string {
	if len(s) > 2000 {
		return s[:2000] + fmt.Sprintf("\n... (%d bytes total)", len(s))
	}
	return s
}
