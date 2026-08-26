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
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/waveoffai/waveoff/internal/cassette"
	"github.com/waveoffai/waveoff/internal/webhook/inject"
)

// A shadow deployment makes one claim: the candidate meets real production
// requests and cannot change anything. Everything in this file exists to test
// that claim against a real cluster, because every part of it is wiring —
// whether the webhook refuses the unsafe shape, whether a write leaving a real
// pod through a real sidecar reaches the real MCP server behind it, and whether
// the NetworkPolicy that is supposed to close the remaining gap actually does.
//
// The last of those is the one worth being careful about. A NetworkPolicy on a
// CNI that does not enforce them is a no-op, and a test asserting "the pod
// could not reach the internet" would pass on such a cluster for the wrong
// reason. So enforcement is established first, against a control, and the test
// fails loudly rather than skipping when it cannot be.

const shadowAgentApp = "shadow-agent"

// TestShadowInjectionIsRefusedWithoutTheEgressAttestation.
//
// Suppression covers what goes through the MCP proxy and nothing else: plain
// HTTP, database drivers, object storage, queues and the filesystem all bypass
// it. A shadow pod that can reach any of those has a partial guarantee
// presented as a complete one, so the real webhook refuses to inject one.
//
// This is the same reasoning as refusing a shadow pod whose manifest classifies
// nothing, and it is checked here rather than only in the webhook's own tests
// because "the webhook is reachable and says no" is a different claim from
// "the function returns an error".
func TestShadowInjectionIsRefusedWithoutTheEgressAttestation(t *testing.T) {
	manifestName := applyManifestFor(t, "unconfined-agent")

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "unconfined-shadow",
			Namespace: namespace,
			Annotations: map[string]string{
				inject.AnnotationInject:    "true",
				inject.AnnotationContainer: "agent",
				inject.AnnotationManifest:  manifestName,
				inject.AnnotationShadow:    "true",
				// A real shadow pod names its upstreams. Without them the
				// webhook skips injection for that reason instead, and this
				// case would pass while proving nothing about egress.
				inject.AnnotationToolUpstreams: "everything=http://shadow-tools." + namespace +
					".svc.cluster.local:3001/mcp",
				// No waveoff.ai/egress-confined.
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name: "agent", Image: "busybox:1.36", ImagePullPolicy: corev1.PullIfNotPresent,
				Command: []string{"sh", "-c", "sleep 300"},
			}},
		},
	}
	if err := k8s.Create(ctx(t), pod); err != nil {
		t.Fatalf("the pod was rejected outright; injection is meant to be skipped, not refused: %v", err)
	}
	t.Cleanup(func() { _ = k8s.Delete(context.Background(), pod) })

	var got corev1.Pod
	if err := k8s.Get(ctx(t), typesName(namespace, "unconfined-shadow"), &got); err != nil {
		t.Fatal(err)
	}
	for _, c := range got.Spec.InitContainers {
		if c.Name == inject.SidecarName {
			t.Fatal("a shadow sidecar was injected into a pod whose egress nobody has confined")
		}
	}
	skipped := got.Annotations["waveoff.ai/inject-skipped"]
	if !strings.Contains(skipped, inject.AnnotationEgressConfined) {
		t.Errorf("the refusal does not name the annotation that fixes it: %q", skipped)
	}
	if !strings.Contains(skipped, "bypass") {
		t.Errorf("the refusal does not say what suppression cannot see: %q", skipped)
	}
}

// TestShadowPodSuppressesWritesInTheCluster.
//
// The end-to-end version of the safety claim: a write leaves a real agent
// container, passes through a real injected sidecar, and never arrives at the
// real MCP server on the other side. A read on the same connection does arrive,
// which is what distinguishes suppression from a broken proxy.
func TestShadowPodSuppressesWritesInTheCluster(t *testing.T) {
	pod := shadowPod(t)

	// A read is forwarded. The everything server answers `echo`, so a
	// successful round trip proves the tool plane is genuinely connected —
	// without this, every suppression assertion below would also pass on a
	// sidecar that could not reach anything at all.
	read := mcpCall(t, pod, "echo", `{"message":"hello"}`)
	if !strings.Contains(read, "hello") {
		t.Fatalf("a read did not reach the MCP server, so nothing below proves suppression:\n%s", read)
	}

	// A write is answered without being executed.
	write := mcpCall(t, pod, "add", `{"a":2,"b":3}`)
	if !strings.Contains(write, "waveoff") {
		t.Fatalf("a write classified in the manifest was not suppressed:\n%s", write)
	}
	// The suppressed answer must carry an identifier. An agent that has just
	// created something will use its id, and a result without one sends a
	// well-built agent down an error path that is then measured as a regression
	// it did not cause.
	var resp struct {
		Result struct {
			ID string `json:"id"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(write), &resp); err != nil {
		t.Fatalf("the suppressed write was not valid JSON-RPC: %v\n%s", err, write)
	}
	if resp.Result.ID == "" {
		t.Errorf("a suppressed write returned no identifier:\n%s", write)
	}

	// The arithmetic the real server would have done must not appear. `add`
	// with 2 and 3 returns 5 when it actually runs.
	if strings.Contains(write, `"5"`) || strings.Contains(write, "= 5") {
		t.Errorf("the write reached the MCP server and was executed:\n%s", write)
	}
}

// TestAnUnclassifiedToolIsRefusedInTheCluster.
//
// Fail closed, proved through the real sidecar. A tool nobody has classified is
// exactly the one that might write, and letting it through on the grounds that
// we do not know is the reverse of what the mandatory effect field is for.
func TestAnUnclassifiedToolIsRefusedInTheCluster(t *testing.T) {
	pod := shadowPod(t)

	out := mcpCall(t, pod, "printEnv", `{}`)
	if !strings.Contains(out, "no asserted effect") {
		t.Errorf("an unclassified tool was not refused:\n%s", out)
	}
}

// TestAnAgentCanReadBackItsOwnSuppressedWriteInTheCluster.
//
// The synthetic object registry, through the real sidecar. Without it the agent
// creates something, is told it succeeded, and then watches the object fail to
// exist — which measures as a regression in the candidate rather than as an
// artefact of the harness.
func TestAnAgentCanReadBackItsOwnSuppressedWriteInTheCluster(t *testing.T) {
	pod := shadowPod(t)

	first := mcpCall(t, pod, "add", `{"a":7,"b":9}`)
	var resp struct {
		Result struct {
			ID string `json:"id"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(first), &resp); err != nil || resp.Result.ID == "" {
		t.Fatalf("no synthetic id to read back:\n%s", first)
	}

	// A retry of the same call must yield the same object rather than a second
	// one: a retried create is one thing that was tried twice.
	again := mcpCall(t, pod, "add", `{"a":7,"b":9}`)
	if !strings.Contains(again, resp.Result.ID) {
		t.Errorf("a retried write minted a different object:\nfirst %s\nagain %s", first, again)
	}

	// And a read referring to the synthetic id is answered from the registry
	// rather than forwarded to a server that has never heard of it.
	back := mcpCall(t, pod, "echo", fmt.Sprintf(`{"message":%q}`, resp.Result.ID))
	if !strings.Contains(back, resp.Result.ID) {
		t.Errorf("a read of a synthetic object did not come back:\n%s", back)
	}
}

// TestSuppressedWritesAreRecordedInTheCluster.
//
// A suppressed call never reaches the proxy, so unless the suppressor records
// it explicitly the session reads as though the candidate never tried to write.
// That is exactly backwards: the attempts are the only evidence of writes that
// did not happen, and they are what the write-divergence guardrail compares.
func TestSuppressedWritesAreRecordedInTheCluster(t *testing.T) {
	pod := shadowPod(t)
	mcpCall(t, pod, "add", `{"a":1,"b":1}`)

	spans := waitForSuppressedSpan(t, pod)
	if len(spans) == 0 {
		t.Fatal("no suppressed call reached the cassette, so a shadow stage would report a " +
			"candidate that never attempted a write")
	}
	var suppressed, refused int
	for _, s := range spans {
		switch {
		case s.Attributes[cassette.AttrToolSuppressed] == true:
			suppressed++
			if s.Attributes["mcp.tool.name"] != "add" {
				t.Errorf("a suppressed span names tool %v, which is not classified as a write",
					s.Attributes["mcp.tool.name"])
			}
			if s.Attributes[cassette.AttrToolEffect] != "write" {
				t.Errorf("the suppressed span carries effect %v, so nothing downstream can tell "+
					"which class of write was attempted", s.Attributes[cassette.AttrToolEffect])
			}
		case s.Attributes[cassette.AttrToolRefused] == true:
			refused++
			// A refusal must not be marked as a suppression. One is a candidate
			// trying to do something and the other is a manifest that does not
			// describe the agent's tools; a shadow report that conflates them
			// sends somebody to fix the wrong thing.
			if s.Attributes[cassette.AttrToolEffect] != nil {
				t.Errorf("a refused call was recorded with an effect: %v",
					s.Attributes[cassette.AttrToolEffect])
			}
		}
	}
	if suppressed == 0 {
		t.Errorf("no span was marked %s; a reader would take the placeholder for an effect that "+
			"happened", cassette.AttrToolSuppressed)
	}
	if refused == 0 {
		t.Errorf("no span was marked %s, so a manifest that does not describe the agent's tools "+
			"would leave no trace", cassette.AttrToolRefused)
	}
}

// TestNetworkPolicyConfinesShadowEgress.
//
// The precondition the egress attestation attests to, checked for real.
//
// Suppression is total only if every side effect flows through the tool plane,
// and a shadow pod that can open arbitrary connections has other routes out:
// databases, object storage, queues, any HTTP API at all. This proves the
// sample policy closes them, using the fake model provider as a stand-in for
// "somewhere the agent has no business reaching".
func TestNetworkPolicyConfinesShadowEgress(t *testing.T) {
	pod := shadowPod(t)
	waitForDeployment(t, "fake-provider")
	elsewhere := podIP(t, "fake-provider")

	// Control first. If a direct connection does not work even without a
	// policy, the assertion below would pass for the wrong reason and prove
	// nothing at all.
	if !canReach(t, pod, elsewhere, 8000) {
		t.Fatalf("the agent container could not reach %s:8000 before any policy was applied, so "+
			"this test cannot distinguish a working policy from a broken fixture", elsewhere)
	}

	requireNetworkPolicyEnforcement(t)
	applyShadowEgressPolicy(t)

	// Policy propagation is not instantaneous. Poll rather than sleep-and-hope,
	// and fail with the control still in mind.
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if !canReach(t, pod, elsewhere, 8000) {
			return
		}
		time.Sleep(3 * time.Second)
	}
	t.Fatalf("the agent container still reached %s:8000 with the sample egress policy in place. "+
		"Every write path that does not speak MCP is open from this shadow pod.", elsewhere)
}

// TestPodLevelPolicyCannotHideTheToolPlaneFromTheAgent.
//
// The limitation, asserted rather than only documented.
//
// A NetworkPolicy selects pods and the sidecar shares the agent's pod, so the
// MCP servers the sidecar must reach are reachable by the agent container too —
// directly, going round the suppressor that would have refused the write.
//
// This is a real gap and it is stated in config/samples/shadow-egress.yaml.
// Closing it needs policy that can distinguish two containers in one pod, or a
// shadow deployment pointed at staging MCP servers rather than production ones.
// The test exists so that the gap cannot be quietly forgotten, and so that
// anybody who closes it has to come here and say so.
func TestPodLevelPolicyCannotHideTheToolPlaneFromTheAgent(t *testing.T) {
	pod := shadowPod(t)
	requireNetworkPolicyEnforcement(t)
	applyShadowEgressPolicy(t)
	toolPlane := podIP(t, "shadow-tools")

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if canReach(t, pod, toolPlane, 3001) {
			return // The documented gap is still the gap.
		}
		time.Sleep(3 * time.Second)
	}
	t.Fatal("the agent can no longer reach the tool plane directly, which is better than what " +
		"config/samples/shadow-egress.yaml claims. Either the policy now distinguishes containers " +
		"or the sidecar has lost its own egress — check which, then update the sample and this test")
}

// TestTheSidecarStillWorksUnderTheEgressPolicy.
//
// The policy has to confine the agent without cutting off the sidecar, which
// genuinely does need to reach the MCP servers it proxies. A policy that broke
// both would look like a passing safety test and a completely useless shadow
// stage.
func TestTheSidecarStillWorksUnderTheEgressPolicy(t *testing.T) {
	pod := shadowPod(t)
	requireNetworkPolicyEnforcement(t)
	applyShadowEgressPolicy(t)

	// A fresh handshake, because the policy may have invalidated the cached
	// session — and because a handshake is the part that would fail first if
	// the sidecar had lost its own egress.
	kubectlExecAllowFail(t, pod, "agent", "rm", "-f", "/tmp/mcp-session")

	deadline := time.Now().Add(90 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		last = mcpCallAllowFail(t, pod, "echo", `{"message":"still-here"}`)
		if strings.Contains(last, "still-here") {
			return
		}
		time.Sleep(3 * time.Second)
	}
	t.Fatalf("the egress policy cut off the sidecar as well as the agent, which leaves a shadow "+
		"stage that is perfectly safe and measures nothing:\n%s", last)
}

// --- fixtures and helpers -------------------------------------------------

// withEverythingTools classifies the reference MCP server's tools.
//
// Three cases, deliberately: a read that must be forwarded, a write that must
// be suppressed, and a tool left out of the manifest entirely so the fail-closed
// path is exercised on a tool the server really does advertise.
//
// `add` is a read in any ordinary reading of the word. It is classified as a
// write here because what the test needs is a tool whose real execution
// produces an observable result — 2 and 3 come back as 5 — so that "the write
// did not reach the server" can be asserted rather than assumed. Effects are
// operator assertions, and this is the operator asserting something for a
// reason.
func withEverythingTools(body string) string {
	// The namespace substitution has already happened by the time edits run,
	// so this interpolates it rather than using the placeholder.
	return body + `    - name: echo
      server: http://shadow-tools.` + namespace + `.svc.cluster.local:3001/mcp
      contractDigest: sha256:3333333333333333333333333333333333333333333333333333333333333333
      effect: read
      replayPolicy: snapshot
    - name: add
      server: http://shadow-tools.` + namespace + `.svc.cluster.local:3001/mcp
      contractDigest: sha256:4444444444444444444444444444444444444444444444444444444444444444
      effect: write
      replayPolicy: no-op
`
}

var shadowPodName string

// shadowPod brings the shadow deployment up once and returns the injected pod.
func shadowPod(t *testing.T) string {
	t.Helper()
	if shadowPodName != "" {
		return shadowPodName
	}
	manifestName := applyManifestFor(t, shadowAgentApp, withEverythingTools)
	applyFixture(t, "shadow.yaml", manifestName)
	waitForDeployment(t, "shadow-tools")
	waitForDeployment(t, shadowAgentApp)

	pod := waitForPodWithLabel(t, "app="+shadowAgentApp)
	waitForContainerReady(t, pod.Name, inject.SidecarName)

	var found bool
	for _, c := range pod.Spec.InitContainers {
		if c.Name == inject.SidecarName {
			found = true
		}
	}
	if !found {
		t.Fatalf("no sidecar was injected into the shadow pod; containers are %v", containerNames(pod))
	}
	shadowPodName = pod.Name
	warmUpToolPlane(t, shadowPodName)
	return shadowPodName
}

// warmUpToolPlane establishes an MCP session before anything is asserted.
//
// The reference server's readiness probe is a TCP check, which passes before it
// can complete a handshake — so the first call after a pod goes ready
// legitimately fails sometimes. Whether a fixture has finished starting is not
// what any of these cases is about, and letting that race decide a suppression
// assertion would make the suite lie in both directions.
func warmUpToolPlane(t *testing.T, pod string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	var last string
	for time.Now().Before(deadline) {
		// Drop any half-established session: a cached id from a failed
		// handshake is worse than none, because the retry keeps using it.
		kubectlExecAllowFail(t, pod, "agent", "rm", "-f", "/tmp/mcp-session")
		last = mcpCallAllowFail(t, pod, "echo", `{"message":"warmup"}`)
		if strings.Contains(last, "warmup") {
			return
		}
		time.Sleep(3 * time.Second)
	}
	t.Fatalf("the tool plane never answered a read through the sidecar:\n%s", last)
}

// mcpCall drives one tool call from inside the agent container, through the
// sidecar, using the streamable HTTP transport.
//
// From inside the agent container on purpose. Calling the sidecar from outside
// the pod would prove the sidecar works and say nothing about whether the
// agent's own traffic is routed through it.
func mcpCall(t *testing.T, pod, tool, args string) string {
	t.Helper()
	out := mcpCallAllowFail(t, pod, tool, args)
	if out == "" {
		t.Fatalf("no response to %s from inside the pod", tool)
	}
	return out
}

func mcpCallAllowFail(t *testing.T, pod, tool, args string) string {
	t.Helper()
	body := fmt.Sprintf(
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, tool, args)
	out := kubectlExecAllowFail(t, pod, "agent", "sh", "-c", mcpScript(body))
	// A streamable-HTTP server may answer with an event stream; the JSON-RPC
	// message is inside a data: frame.
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "data:"); ok {
			return strings.TrimSpace(rest)
		}
	}
	return strings.TrimSpace(out)
}

// mcpScript performs a real MCP handshake before the call.
//
// A fabricated Mcp-Session-Id will not do: the reference server issues its own
// on initialize and answers anything else with "no valid session ID". Which is
// the point — a suppressor that only ever sees calls the server would have
// rejected proves nothing about suppressing calls the server would have run.
//
// The session id is cached in the container so every call in this file belongs
// to one agent session. The synthetic object registry is session-scoped, so a
// fresh handshake per call would make a read-your-writes case unprovable for
// reasons that have nothing to do with the code under test.
func mcpScript(body string) string {
	const traceparent = "traceparent: 00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	return `
set -e
EP=http://127.0.0.1:8080/mcp/everything
S=/tmp/mcp-session
if [ ! -s "$S" ]; then
  curl -sS -D /tmp/mcp-headers -o /dev/null -X POST "$EP" \
    -H 'content-type: application/json' \
    -H 'accept: application/json, text/event-stream' \
    -H '` + traceparent + `' \
    -d '{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"waveoff-e2e","version":"0"}}}'
  grep -i '^mcp-session-id:' /tmp/mcp-headers | tr -d '\r' | awk '{print $2}' > "$S"
  [ -s "$S" ] || { echo "NO_SESSION_ID"; cat /tmp/mcp-headers; exit 1; }
  curl -sS -o /dev/null -X POST "$EP" \
    -H 'content-type: application/json' \
    -H 'accept: application/json, text/event-stream' \
    -H "mcp-session-id: $(cat $S)" \
    -H '` + traceparent + `' \
    -d '{"jsonrpc":"2.0","method":"notifications/initialized"}'
fi
curl -sS -X POST "$EP" \
  -H 'content-type: application/json' \
  -H 'accept: application/json, text/event-stream' \
  -H "mcp-session-id: $(cat $S)" \
  -H '` + traceparent + `' \
  -d '` + body + `'
`
}

// waitForSuppressedSpan reads the cassette the sidecar is writing.
//
// The sidecar image is distroless, so it answers for itself: -dump is the only
// way an operator can read a running recorder's corpus either.
func waitForSuppressedSpan(t *testing.T, pod string) []cassette.Span {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	var seen int
	for time.Now().Before(deadline) {
		var spans []cassette.Span
		for _, session := range corpusSessions(t, pod) {
			// -dump takes a session id: the corpus has to be listed first.
			out := kubectlExecAllowFail(t, pod, inject.SidecarName,
				inject.BinaryPath, "-corpus-dir", "/var/lib/waveoff/corpus", "-dump", session)
			for _, line := range strings.Split(out, "\n") {
				line = strings.TrimSpace(line)
				if !strings.HasPrefix(line, "{") {
					continue
				}
				var s cassette.Span
				if err := json.Unmarshal([]byte(line), &s); err == nil && s.Attributes != nil {
					spans = append(spans, s)
				}
			}
		}
		seen = len(spans)
		for _, s := range spans {
			if s.Attributes[cassette.AttrToolSuppressed] == true {
				return spans
			}
		}
		time.Sleep(5 * time.Second)
	}
	logs, _ := exec.Command("kubectl", "logs", pod, "-c", inject.SidecarName, "-n", namespace).CombinedOutput()
	t.Fatalf("no suppressed span appeared in the corpus after reading %d spans across %v\n"+
		"--- recorder ---\n%s", seen, corpusSessions(t, pod), truncate(string(logs)))
	return nil
}

// corpusSessions asks the recorder which cassettes it holds.
func corpusSessions(t *testing.T, pod string) []string {
	t.Helper()
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
	return sessions
}

// requireNetworkPolicyEnforcement establishes that this cluster's CNI enforces
// NetworkPolicy at all.
//
// Without this, every egress assertion in this file would pass on a cluster
// where NetworkPolicy is decorative — which is the exact failure this project
// keeps guarding against elsewhere: safe and useless looks identical to
// working. It fails rather than skips, because a shadow stage on a
// non-enforcing cluster is not safe, and a green suite should not say it is.
func requireNetworkPolicyEnforcement(t *testing.T) {
	t.Helper()
	if enforcementChecked {
		if !enforced {
			t.Fatal(enforcementReason)
		}
		return
	}
	enforcementChecked = true

	probe := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "np-probe", Namespace: namespace,
			Labels: map[string]string{"app": "np-probe"},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name: "probe", Image: "curlimages/curl:8.11.1",
				ImagePullPolicy: corev1.PullIfNotPresent,
				Command:         []string{"sh", "-c", "sleep 600"},
			}},
		},
	}
	_ = k8s.Delete(context.Background(), probe)
	if err := k8s.Create(ctx(t), probe); err != nil {
		t.Fatalf("creating the enforcement probe: %v", err)
	}
	t.Cleanup(func() { _ = k8s.Delete(context.Background(), probe) })
	waitForContainerReady(t, "np-probe", "probe")

	waitForDeployment(t, "fake-provider")
	ip := podIP(t, "fake-provider")
	if !canReach(t, "np-probe", ip, 8000) {
		enforcementReason = "the enforcement probe could not reach the fake provider even with no " +
			"policy applied, so nothing here can tell a working policy from a broken fixture"
		t.Fatal(enforcementReason)
	}

	deny := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "np-probe-deny", Namespace: namespace},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "np-probe"}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
		},
	}
	if err := k8s.Create(ctx(t), deny); err != nil {
		t.Fatalf("creating the probe policy: %v", err)
	}
	t.Cleanup(func() { _ = k8s.Delete(context.Background(), deny) })

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if !canReach(t, "np-probe", ip, 8000) {
			enforced = true
			return
		}
		time.Sleep(3 * time.Second)
	}
	enforcementReason = "this cluster's CNI does not enforce NetworkPolicy: a pod selected by a " +
		"default-deny egress policy still reached a Service. The egress confinement a shadow " +
		"deployment depends on would be decorative here, so these cases fail rather than pass " +
		"for the wrong reason. Run against a cluster with a policy-enforcing CNI."
	t.Fatal(enforcementReason)
}

var (
	enforcementChecked bool
	enforced           bool
	enforcementReason  string
)

// applyShadowEgressPolicy applies the shipped sample, retargeted at this
// namespace and this deployment.
//
// The sample is applied rather than a hand-written equivalent: what is being
// tested is the policy an operator would copy out of config/samples, not one
// written to make the test pass.
func applyShadowEgressPolicy(t *testing.T) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "samples", "shadow-egress.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	body := strings.NewReplacer(
		"namespace: prod", "namespace: "+namespace,
		"app: support-agent-shadow", "app: "+shadowAgentApp,
	).Replace(string(raw))

	cmd := exec.Command("kubectl", "apply", "-n", namespace, "-f", "-")
	cmd.Stdin = strings.NewReader(body)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("applying the sample egress policy: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("kubectl", "delete", "networkpolicy", "shadow-agent-egress",
			"-n", namespace, "--ignore-not-found").Run()
	})
}

// canReach reports whether a container can open a TCP connection.
//
// The status code is fenced in a marker rather than read off the tail of the
// output, because kubectl exec returns combined output: a failure carries
// curl's own message and "command terminated with exit code 28", and any
// substring search for a digit matches those. An earlier version of this helper
// did exactly that and reported every blocked connection as reachable.
//
// Any HTTP status at all means the connection was established — the MCP server
// answers a bare GET with an error, and an error from the server is still proof
// the packet arrived.
func canReach(t *testing.T, pod, host string, port int) bool {
	t.Helper()
	out := kubectlExecAllowFail(t, pod, containerFor(pod),
		"curl", "-sS", "-m", "5", "-o", "/dev/null", "-w", "<status>%{http_code}</status>",
		fmt.Sprintf("http://%s:%d/mcp", host, port))
	start := strings.Index(out, "<status>")
	end := strings.Index(out, "</status>")
	if start < 0 || end < start {
		return false
	}
	code := out[start+len("<status>") : end]
	return len(code) == 3 && code != "000"
}

func containerFor(pod string) string {
	if pod == "np-probe" {
		return "probe"
	}
	return "agent"
}

// podIP returns the address of a pod backing an app label.
//
// A pod address rather than a Service's cluster IP, on purpose. A cluster IP is
// rewritten by kube-proxy, and whether that rewrite happens before or after
// policy evaluation is a property of the CNI rather than of the policy — on
// kindnet, a connection to a cluster IP is not matched by an egress
// podSelector at all. Probing the pod directly tests what the policy says
// rather than what the service proxy happens to do with it.
func podIP(t *testing.T, app string) string {
	t.Helper()
	sel, _ := labels.Parse("app=" + app)
	pods := &corev1.PodList{}
	if err := k8s.List(ctx(t), pods, client.InNamespace(namespace),
		client.MatchingLabelsSelector{Selector: sel}); err != nil {
		t.Fatal(err)
	}
	for i := range pods.Items {
		if ip := pods.Items[i].Status.PodIP; ip != "" && pods.Items[i].DeletionTimestamp == nil {
			return ip
		}
	}
	t.Fatalf("no running pod with app=%s to probe", app)
	return ""
}
