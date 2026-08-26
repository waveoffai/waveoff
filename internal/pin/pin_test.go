// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package pin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/waveoffai/waveoff/internal/mcp"
)

func scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := appsv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func deployment() *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "support-agent", Namespace: "prod"},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "support-agent"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "support-agent"}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name: "agent",
						// A mutable tag, which is the normal case and the reason
						// pin looks at what is running instead.
						Image: "registry.internal/support-agent:v4",
						Env: []corev1.EnvVar{
							{Name: "ANTHROPIC_MODEL", Value: "claude-sonnet-4-6"},
							{Name: "LLM_TEMPERATURE", Value: "0.2"},
							{Name: "MAX_TOKENS", Value: "4096"},
						},
						VolumeMounts: []corev1.VolumeMount{{Name: "prompts", MountPath: "/etc/prompts"}},
					}},
					Volumes: []corev1.Volume{{
						Name: "prompts",
						VolumeSource: corev1.VolumeSource{
							ConfigMap: &corev1.ConfigMapVolumeSource{
								LocalObjectReference: corev1.LocalObjectReference{Name: "agent-prompts"}}},
					}},
				},
			},
		},
	}
}

func runningPod(ready bool, imageID string) *corev1.Pod {
	cond := corev1.ConditionFalse
	if ready {
		cond = corev1.ConditionTrue
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "support-agent-abc", Namespace: "prod",
			Labels: map[string]string{"app": "support-agent"}},
		Status: corev1.PodStatus{
			Conditions:        []corev1.PodCondition{{Type: corev1.PodReady, Status: cond}},
			ContainerStatuses: []corev1.ContainerStatus{{Name: "agent", ImageID: imageID}},
		},
	}
}

func configMap() *corev1.ConfigMap {
	cfg, _ := json.Marshal(map[string]any{
		"mcpServers": map[string]any{"jira": map[string]any{"url": "https://jira-gw.internal/mcp"}},
	})
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-prompts", Namespace: "prod"},
		Data: map[string]string{
			"system.md": "You are a support agent.\nBe concise.\n",
			"mcp.json":  string(cfg),
			"notes.bin": "not a prompt",
		},
	}
}

func fakeTools(tools ...mcp.Tool) ToolLister {
	return func(context.Context, string) ([]mcp.Tool, error) { return tools, nil }
}

func run(t *testing.T, opts Options, list ToolLister, objs ...runtime.Object) *Result {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithRuntimeObjects(objs...).Build()
	res, err := Pin(context.Background(), c, list, opts)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// TestResolvesRunningImageDigest is the detail most tooling gets wrong: the pod
// template usually carries a tag, so pinning it pins nothing.
func TestResolvesRunningImageDigest(t *testing.T) {
	const id = "sha256:" + "ab12cd34" + "00000000000000000000000000000000000000000000000000000000"
	res := run(t, Options{Namespace: "prod", Deployment: "support-agent"}, fakeTools(),
		deployment(), runningPod(true, "docker-pullable://mirror.io/support-agent@"+id[:71]), configMap())

	got := res.Manifest.Spec.Code.Image
	if !strings.HasPrefix(got, "registry.internal/support-agent@sha256:") {
		t.Errorf("image = %q; want the Deployment's repository with the running digest", got)
	}
	if strings.Contains(got, ":v4") {
		t.Error("pinned the mutable tag from the pod template instead of the running digest")
	}
}

// TestPrefersReadyPod: a crash-looping pod may still be running the image being
// rolled back from, so its digest is the wrong answer.
func TestPrefersReadyPod(t *testing.T) {
	old := "sha256:" + strings.Repeat("a", 64)
	cur := "sha256:" + strings.Repeat("b", 64)

	notReady := runningPod(false, "registry.internal/support-agent@"+old)
	notReady.Name = "old-pod"
	ready := runningPod(true, "registry.internal/support-agent@"+cur)

	res := run(t, Options{Namespace: "prod", Deployment: "support-agent"}, fakeTools(),
		deployment(), notReady, ready, configMap())

	if !strings.HasSuffix(res.Manifest.Spec.Code.Image, cur) {
		t.Errorf("image = %q; want the digest from the Ready pod", res.Manifest.Spec.Code.Image)
	}
}

func TestUnpinnableImageIsATODO(t *testing.T) {
	res := run(t, Options{Namespace: "prod", Deployment: "support-agent"}, fakeTools(), deployment(), configMap())
	if res.TODOs == 0 {
		t.Error("a Deployment with no running pod and a tagged image must produce a TODO, not a tag-pinned manifest")
	}
	if !hasNote(res, "mutable tag") {
		t.Errorf("expected a note about the mutable tag, got %v", res.Notes)
	}
}

func TestInfersModelFromEnvironment(t *testing.T) {
	res := run(t, Options{Namespace: "prod", Deployment: "support-agent"}, fakeTools(),
		deployment(), runningPod(true, "registry.internal/support-agent@sha256:"+strings.Repeat("c", 64)), configMap())

	m := res.Manifest.Spec.Model
	if m.Provider != "anthropic" || m.ID != "claude-sonnet-4-6" {
		t.Errorf("model = %+v", m)
	}
	if m.Params.Temperature == nil || *m.Params.Temperature != 0.2 {
		t.Errorf("temperature = %v, want 0.2", m.Params.Temperature)
	}
	if m.Params.MaxTokens == nil || *m.Params.MaxTokens != 4096 {
		t.Errorf("maxTokens = %v, want 4096", m.Params.MaxTokens)
	}
}

func TestHashesPromptsFromConfigMap(t *testing.T) {
	res := run(t, Options{Namespace: "prod", Deployment: "support-agent"}, fakeTools(),
		deployment(), runningPod(true, "registry.internal/support-agent@sha256:"+strings.Repeat("c", 64)), configMap())

	if len(res.Manifest.Spec.Prompts) != 1 {
		t.Fatalf("prompts = %+v; want exactly system.md (notes.bin is not a prompt)", res.Manifest.Spec.Prompts)
	}
	p := res.Manifest.Spec.Prompts[0]
	if p.Name != "system" || !strings.HasPrefix(p.Digest, "sha256:") {
		t.Errorf("prompt = %+v", p)
	}
	if !strings.HasPrefix(p.Source, "configmap://prod/agent-prompts#system.md") {
		t.Errorf("source = %q; provenance must name where the body came from", p.Source)
	}
}

// TestEffectIsNeverInferredFromServerHints is the security property. The server
// is the untrusted party in the tool-poisoning threat model, so its claims about
// itself must never become an effect classification.
func TestEffectIsNeverInferredFromServerHints(t *testing.T) {
	yes := true
	res := run(t, Options{Namespace: "prod", Deployment: "support-agent"},
		fakeTools(
			mcp.Tool{Name: "docs.search", Description: "Search", Annotations: &mcp.Annotations{ReadOnlyHint: &yes}},
			mcp.Tool{Name: "jira.create", Description: "Create", Annotations: &mcp.Annotations{DestructiveHint: &yes}},
		),
		deployment(), runningPod(true, "registry.internal/support-agent@sha256:"+strings.Repeat("c", 64)), configMap())

	if len(res.Manifest.Spec.Tools) != 2 {
		t.Fatalf("tools = %+v", res.Manifest.Spec.Tools)
	}
	for _, tool := range res.Manifest.Spec.Tools {
		if tool.Effect != "" {
			t.Errorf("tool %q was assigned effect %q from a server hint; effect must be operator-asserted",
				tool.Name, tool.Effect)
		}
		if !strings.HasPrefix(tool.ContractDigest, "sha256:") {
			t.Errorf("tool %q has no contract digest", tool.Name)
		}
	}

	// The hint must still reach the operator, as a comment.
	var out strings.Builder
	if err := res.Emit(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "suggesting: write") {
		t.Errorf("the server's destructive hint must be surfaced as a comment:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "untrusted") {
		t.Error("the comment must say why the hint was not adopted")
	}
}

// TestUnreachableServerOmitsToolsRatherThanInventingThem: a placeholder digest
// would produce a manifest that looks pinned and is not.
func TestUnreachableServerOmitsToolsRatherThanInventingThem(t *testing.T) {
	failing := func(context.Context, string) ([]mcp.Tool, error) {
		return nil, context.DeadlineExceeded
	}
	res := run(t, Options{Namespace: "prod", Deployment: "support-agent"}, failing,
		deployment(), runningPod(true, "registry.internal/support-agent@sha256:"+strings.Repeat("c", 64)), configMap())

	if len(res.Manifest.Spec.Tools) != 0 {
		t.Errorf("tools were invented for an unreachable server: %+v", res.Manifest.Spec.Tools)
	}
	if !hasNote(res, "no contract digest is better than a made-up one") {
		t.Errorf("notes = %v", res.Notes)
	}
}

func TestDiscoversMCPServersFromMountedConfig(t *testing.T) {
	var seen string
	list := func(_ context.Context, endpoint string) ([]mcp.Tool, error) {
		seen = endpoint
		return nil, nil
	}
	run(t, Options{Namespace: "prod", Deployment: "support-agent"}, list,
		deployment(), runningPod(true, "registry.internal/support-agent@sha256:"+strings.Repeat("c", 64)), configMap())

	if seen != "https://jira-gw.internal/mcp" {
		t.Errorf("endpoint discovered from mcp.json = %q", seen)
	}
}

func TestExplicitServerOverridesDiscovered(t *testing.T) {
	var seen []string
	list := func(_ context.Context, endpoint string) ([]mcp.Tool, error) {
		seen = append(seen, endpoint)
		return nil, nil
	}
	run(t, Options{Namespace: "prod", Deployment: "support-agent",
		MCPServers: map[string]string{"jira": "https://jira-prod.internal/mcp"}}, list,
		deployment(), runningPod(true, "registry.internal/support-agent@sha256:"+strings.Repeat("c", 64)), configMap())

	if len(seen) != 1 || seen[0] != "https://jira-prod.internal/mcp" {
		t.Errorf("endpoints = %v; --mcp-server must win over a mounted config", seen)
	}
}

// TestCompleteManifestGetsDigests: when nothing is left undecided, pin emits a
// manifest that applies as-is.
func TestCompleteManifestGetsDigests(t *testing.T) {
	res := run(t, Options{Namespace: "prod", Deployment: "support-agent"}, fakeTools(),
		deployment(), runningPod(true, "registry.internal/support-agent@sha256:"+strings.Repeat("c", 64)), configMap())
	res.TODOs = 0 // no tools to classify in this fixture

	var out strings.Builder
	if err := res.Emit(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "behaviorDigest: sha256:") {
		t.Errorf("a complete manifest must carry digests:\n%s", out.String())
	}
	if !strings.HasPrefix(res.Manifest.Name, "support-agent-") {
		t.Errorf("name = %q", res.Manifest.Name)
	}
}

func TestMultiContainerRequiresExplicitChoice(t *testing.T) {
	d := deployment()
	d.Spec.Template.Spec.Containers = append(d.Spec.Template.Spec.Containers,
		corev1.Container{Name: "sidecar", Image: "busybox:latest"})

	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithRuntimeObjects(d).Build()
	_, err := Pin(context.Background(), c, fakeTools(), Options{Namespace: "prod", Deployment: "support-agent"})
	if err == nil || !strings.Contains(err.Error(), "--container") {
		t.Errorf("expected a clear error naming --container, got %v", err)
	}
}

func hasNote(r *Result, substr string) bool {
	for _, n := range r.Notes {
		if strings.Contains(n, substr) {
			return true
		}
	}
	return false
}
