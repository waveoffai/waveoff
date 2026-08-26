// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package inject

import (
	"context"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/waveoffai/waveoff/api/v1alpha1"
)

func pod(annotations map[string]string, containers ...corev1.Container) *corev1.Pod {
	if len(containers) == 0 {
		containers = []corev1.Container{{
			Name:  "agent",
			Image: "registry.internal/support-agent@sha256:abc",
			Env: []corev1.EnvVar{
				{Name: "ANTHROPIC_API_KEY", ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{Key: "k"}}},
			},
		}}
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "support-agent-abc", Namespace: "prod",
			Annotations: annotations,
			Labels:      map[string]string{"app": "support-agent"},
		},
		Spec: corev1.PodSpec{Containers: containers},
	}
}

func inject(t *testing.T, p *corev1.Pod, objs ...runtime.Object) *corev1.Pod {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
	i := &Injector{Client: c, Image: "ghcr.io/waveoffai/waveoff-recorder:v1"}
	if err := i.Default(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	return p
}

func sidecar(t *testing.T, p *corev1.Pod) *corev1.Container {
	t.Helper()
	for i := range p.Spec.InitContainers {
		if p.Spec.InitContainers[i].Name == SidecarName {
			return &p.Spec.InitContainers[i]
		}
	}
	t.Fatalf("no %s sidecar was injected:\ninit=%+v\ncontainers=%+v",
		SidecarName, p.Spec.InitContainers, p.Spec.Containers)
	return nil
}

// TestSidecarStartsBeforeTheAgent is the ordering guarantee.
//
// As an ordinary container the recorder starts alongside the agent, and the
// agent's first model calls fail against a proxy that is not listening yet. A
// native sidecar — an init container with restartPolicy: Always — is started
// first and kept running.
func TestSidecarStartsBeforeTheAgent(t *testing.T) {
	p := inject(t, pod(map[string]string{AnnotationInject: "true"}))

	sc := sidecar(t, p)
	if sc.RestartPolicy == nil || *sc.RestartPolicy != corev1.ContainerRestartPolicyAlways {
		t.Fatalf("restartPolicy = %v; without Always this is a one-shot init container "+
			"that must exit before the agent starts, so nothing would ever be recorded", sc.RestartPolicy)
	}
	for _, c := range p.Spec.Containers {
		if c.Name == SidecarName {
			t.Error("the recorder was added as an ordinary container; the agent would race it on startup")
		}
	}
}

func argValue(c *corev1.Container, flag string) string {
	for i, a := range c.Args {
		if a == flag && i+1 < len(c.Args) {
			return c.Args[i+1]
		}
	}
	return ""
}

// TestOptOutIsTheDefault: a webhook that injects into every pod in the cluster
// is an outage waiting to happen.
func TestOptOutIsTheDefault(t *testing.T) {
	p := inject(t, pod(nil))
	if len(p.Spec.Containers) != 1 || len(p.Spec.InitContainers) != 0 {
		t.Errorf("a pod with no opt-in was mutated: %+v / %+v", p.Spec.Containers, p.Spec.InitContainers)
	}
}

func TestInjectsSidecarAndRewritesBaseURL(t *testing.T) {
	p := inject(t, pod(map[string]string{AnnotationInject: "true"}))

	sc := sidecar(t, p)
	if got := argValue(sc, "-model-upstream"); got != "https://api.anthropic.com" {
		t.Errorf("-model-upstream = %q; it should be where traffic really goes", got)
	}

	// The whole zero-code-change trick: the agent keeps talking to what it
	// thinks is its provider.
	agent := &p.Spec.Containers[0]
	want := fmt.Sprintf("http://127.0.0.1:%d", ListenPort)
	var found bool
	for _, e := range agent.Env {
		if e.Name == "ANTHROPIC_BASE_URL" {
			found = true
			if e.Value != want {
				t.Errorf("ANTHROPIC_BASE_URL = %q, want %q", e.Value, want)
			}
		}
	}
	if !found {
		t.Error("the agent's base URL was not rewritten, so nothing would be recorded")
	}
	if p.Annotations[AnnotationInjected] == "" {
		t.Error("no injected marker was stamped")
	}
}

// TestExistingBaseURLIsPreservedAsUpstream: an agent already pointed at a
// gateway must keep reaching that gateway, not the public provider.
func TestExistingBaseURLIsPreservedAsUpstream(t *testing.T) {
	p := pod(map[string]string{AnnotationInject: "true"}, corev1.Container{
		Name: "agent",
		Env:  []corev1.EnvVar{{Name: "ANTHROPIC_BASE_URL", Value: "https://llm-gw.internal"}},
	})
	inject(t, p)

	if got := argValue(sidecar(t, p), "-model-upstream"); got != "https://llm-gw.internal" {
		t.Errorf("-model-upstream = %q; the existing gateway must be preserved", got)
	}
}

// TestManifestIdentityIsStamped: without it a corpus is traffic that cannot be
// attributed to a version.
func TestManifestIdentityIsStamped(t *testing.T) {
	behavior := "sha256:" + strings.Repeat("a", 64)
	content := "sha256:" + strings.Repeat("b", 64)
	m := &v1alpha1.AgentManifest{
		ObjectMeta: metav1.ObjectMeta{Name: "support-agent-aaaaaaaaaaaa", Namespace: "prod"},
		Spec: v1alpha1.AgentManifestSpec{
			Agent: "support-agent", BehaviorDigest: behavior, ContentDigest: content,
		},
	}
	p := inject(t, pod(map[string]string{
		AnnotationInject:   "true",
		AnnotationManifest: "support-agent-aaaaaaaaaaaa",
	}), m)

	sc := sidecar(t, p)
	if argValue(sc, "-behavior-digest") != behavior {
		t.Errorf("-behavior-digest = %q", argValue(sc, "-behavior-digest"))
	}
	if argValue(sc, "-content-digest") != content {
		t.Errorf("-content-digest = %q", argValue(sc, "-content-digest"))
	}
	if argValue(sc, "-agent") != "support-agent" {
		t.Errorf("-agent = %q", argValue(sc, "-agent"))
	}
}

// TestShadowPodsRefuseWrites is the wiring that makes a shadow deployment safe.
// Mirroring alone stops nothing: the candidate would file the ticket.
func TestShadowPodsRefuseWrites(t *testing.T) {
	m := &v1alpha1.AgentManifest{
		ObjectMeta: metav1.ObjectMeta{Name: "support-agent-aaaaaaaaaaaa", Namespace: "prod"},
		Spec: v1alpha1.AgentManifestSpec{
			Agent:          "support-agent",
			BehaviorDigest: "sha256:" + strings.Repeat("a", 64),
			Tools: []v1alpha1.ToolRef{
				{Name: "docs.search", Effect: v1alpha1.EffectRead},
				{Name: "jira.create_issue", Effect: v1alpha1.EffectWrite},
			},
		},
	}
	p := inject(t, pod(map[string]string{
		AnnotationInject:         "true",
		AnnotationManifest:       "support-agent-aaaaaaaaaaaa",
		AnnotationShadow:         "true",
		AnnotationEgressConfined: "true",
	}), m)

	args := strings.Join(sidecar(t, p).Args, " ")
	if !strings.Contains(args, "-shadow") {
		t.Error("a shadow pod was injected without write suppression")
	}
	// The classifications have to travel with it, or the sidecar refuses
	// everything and the shadow deployment observes nothing.
	for _, want := range []string{"docs.search=read", "jira.create_issue=write"} {
		if !strings.Contains(args, want) {
			t.Errorf("the sidecar was not told about %q: %v", want, sidecar(t, p).Args)
		}
	}
}

// TestANonShadowPodDoesNotSuppress: suppression is for mirrored traffic. A
// normal pod that refused its own writes would simply be broken.
func TestANonShadowPodDoesNotSuppress(t *testing.T) {
	p := inject(t, pod(map[string]string{AnnotationInject: "true"}))
	if strings.Contains(strings.Join(sidecar(t, p).Args, " "), "-shadow") {
		t.Error("an ordinary pod was given write suppression")
	}
}

// TestShadowWithoutClassificationsIsRefused: a shadow pod that refuses every
// tool call is safe and useless, and looks identical to one that is working.
func TestShadowWithoutClassificationsIsRefused(t *testing.T) {
	p := pod(map[string]string{
		AnnotationInject: "true",
		AnnotationShadow: "true",
		// No manifest annotation, so no effects can be read.
	})
	inject(t, p)

	if hasContainer(p, SidecarName) {
		t.Error("a shadow pod was injected with no tool classifications")
	}
	if p.Annotations["waveoff.ai/inject-skipped"] == "" {
		t.Error("the reason was not recorded")
	}
}

// TestShadowRequiresConfinedEgress.
//
// Suppression only covers the MCP proxy. Direct HTTP, database drivers, object
// storage, queues and the filesystem all bypass it, so a shadow pod that can
// reach any of those has a partial guarantee presented as a complete one.
func TestShadowRequiresConfinedEgress(t *testing.T) {
	m := &v1alpha1.AgentManifest{
		ObjectMeta: metav1.ObjectMeta{Name: "support-agent-aaaaaaaaaaaa", Namespace: "prod"},
		Spec: v1alpha1.AgentManifestSpec{
			Agent: "support-agent",
			Tools: []v1alpha1.ToolRef{{Name: "docs.search", Effect: v1alpha1.EffectRead}},
		},
	}
	p := pod(map[string]string{
		AnnotationInject:   "true",
		AnnotationManifest: "support-agent-aaaaaaaaaaaa",
		AnnotationShadow:   "true",
		// No egress attestation.
	})
	inject(t, p, m)

	if hasContainer(p, SidecarName) {
		t.Error("a shadow pod was injected without egress being confined")
	}
	skipped := p.Annotations["waveoff.ai/inject-skipped"]
	if !strings.Contains(skipped, "bypass") {
		t.Errorf("the refusal should name what suppression cannot see: %q", skipped)
	}
}

// TestMissingManifestStillInjects: recording without identity is degraded;
// refusing to start the pod would be worse.
func TestMissingManifestStillInjects(t *testing.T) {
	p := inject(t, pod(map[string]string{
		AnnotationInject:   "true",
		AnnotationManifest: "does-not-exist",
	}))
	sc := sidecar(t, p)
	if argValue(sc, "-behavior-digest") != "" {
		t.Error("a digest was invented for a manifest that does not exist")
	}
}

func TestToolUpstreams(t *testing.T) {
	p := inject(t, pod(map[string]string{
		AnnotationInject:        "true",
		AnnotationToolUpstreams: "jira=https://jira-gw.internal/mcp, docs=https://docs-gw.internal/mcp",
	}))
	sc := sidecar(t, p)
	joined := strings.Join(sc.Args, " ")
	for _, want := range []string{"jira=https://jira-gw.internal/mcp", "docs=https://docs-gw.internal/mcp"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q: %v", want, sc.Args)
		}
	}
	// The agent is told where to reach each server through the sidecar.
	agent := &p.Spec.Containers[0]
	var jira string
	for _, e := range agent.Env {
		if e.Name == "WAVEOFF_MCP_JIRA" {
			jira = e.Value
		}
	}
	if !strings.Contains(jira, "/mcp/jira") {
		t.Errorf("WAVEOFF_MCP_JIRA = %q", jira)
	}
}

// TestMultipleContainersRefuseToGuess: recording the wrong container produces a
// corpus that looks fine and describes nothing.
func TestMultipleContainersRefuseToGuess(t *testing.T) {
	p := pod(map[string]string{AnnotationInject: "true"},
		corev1.Container{Name: "agent"}, corev1.Container{Name: "envoy"})
	inject(t, p)

	if hasContainer(p, SidecarName) {
		t.Error("a sidecar was injected without knowing which container is the agent")
	}
	if p.Annotations["waveoff.ai/inject-skipped"] == "" {
		t.Error("the reason for skipping was not recorded")
	}
	// And the pod must still be admitted: observability may not take down the
	// thing it observes.
	if len(p.Spec.Containers) != 2 {
		t.Error("the pod was altered despite injection being skipped")
	}
}

func TestNamedContainerIsUsed(t *testing.T) {
	p := pod(map[string]string{AnnotationInject: "true", AnnotationContainer: "agent"},
		corev1.Container{Name: "envoy"},
		corev1.Container{Name: "agent", Env: []corev1.EnvVar{{Name: "OPENAI_BASE_URL", Value: "https://api.openai.com"}}},
	)
	inject(t, p)
	sidecar(t, p)

	for _, e := range p.Spec.Containers[1].Env {
		if e.Name == "OPENAI_BASE_URL" && !strings.Contains(e.Value, "127.0.0.1") {
			t.Errorf("the named container's base URL was not rewritten: %q", e.Value)
		}
	}
	if len(p.Spec.Containers[0].Env) != 0 {
		t.Error("the wrong container was modified")
	}
}

// TestInjectionIsIdempotent: a pod that passes through admission twice must not
// grow two sidecars.
func TestInjectionIsIdempotent(t *testing.T) {
	p := inject(t, pod(map[string]string{AnnotationInject: "true"}))
	before := len(p.Spec.InitContainers)
	inject(t, p)
	if len(p.Spec.InitContainers) != before {
		t.Errorf("a second pass added another sidecar: %d -> %d", before, len(p.Spec.InitContainers))
	}
}

func TestCorpusClaimIsMountedWhenGiven(t *testing.T) {
	// Without a claim the corpus dies with the pod, which is the right default
	// for trying it out and the wrong one for building an asset.
	p := inject(t, pod(map[string]string{AnnotationInject: "true"}))
	if p.Spec.Volumes[0].EmptyDir == nil {
		t.Error("expected an emptyDir by default")
	}

	p2 := inject(t, pod(map[string]string{AnnotationInject: "true", AnnotationCorpusClaim: "waveoff-corpus"}))
	if p2.Spec.Volumes[0].PersistentVolumeClaim == nil {
		t.Fatal("the corpus claim was not mounted")
	}
	if got := p2.Spec.Volumes[0].PersistentVolumeClaim.ClaimName; got != "waveoff-corpus" {
		t.Errorf("claim = %q", got)
	}
}

func TestSidecarRunsLocked(t *testing.T) {
	p := inject(t, pod(map[string]string{AnnotationInject: "true"}))
	sc := sidecar(t, p)
	if sc.SecurityContext == nil {
		t.Fatal("no security context")
	}
	if sc.SecurityContext.ReadOnlyRootFilesystem == nil || !*sc.SecurityContext.ReadOnlyRootFilesystem {
		t.Error("the sidecar should run with a read-only root filesystem")
	}
	if sc.SecurityContext.RunAsNonRoot == nil || !*sc.SecurityContext.RunAsNonRoot {
		t.Error("the sidecar should run as non-root")
	}
	// It listens on loopback only, so it must not advertise a port.
	if len(sc.Ports) != 0 {
		t.Error("the sidecar should not expose a container port; it serves loopback only")
	}
}

// TestReadinessProbeCanReachALoopbackListener: a Kubernetes HTTP probe is
// dialled from the kubelet's network namespace and cannot reach a pod's
// loopback, so it fails with connection refused however healthy the process
// is. Only an exec probe runs from inside the container.
func TestReadinessProbeIsExec(t *testing.T) {
	p := inject(t, pod(map[string]string{AnnotationInject: "true"}))
	probe := sidecar(t, p).ReadinessProbe
	if probe == nil {
		t.Fatal("no readiness probe")
	}
	if probe.HTTPGet != nil {
		t.Error("an HTTP probe cannot reach a loopback-only listener from the kubelet")
	}
	if probe.Exec == nil || len(probe.Exec.Command) == 0 {
		t.Fatal("expected an exec probe")
	}
	if !strings.Contains(strings.Join(probe.Exec.Command, " "), "-healthcheck") {
		t.Errorf("probe command = %v", probe.Exec.Command)
	}
}

// TestUnrecognisedSetupIsLeftAlone: if we cannot tell where model traffic goes,
// guessing would send it somewhere wrong.
func TestUnrecognisedSetupIsLeftAlone(t *testing.T) {
	p := pod(map[string]string{AnnotationInject: "true"},
		corev1.Container{Name: "agent"}) // no provider env at all
	inject(t, p)

	if hasContainer(p, SidecarName) {
		t.Error("a sidecar was injected with no upstream to proxy to")
	}
	if p.Annotations["waveoff.ai/inject-skipped"] == "" {
		t.Error("the reason was not recorded")
	}
}
