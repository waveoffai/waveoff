// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

// Package inject adds the recorder sidecar to an agent's pod.
//
// This is the second mutating webhook in the project and the first one that was
// the right call. M0 deliberately has none, because rewriting a
// GitOps-managed object leaves Argo CD and Flux showing permanent drift. A Pod
// is different: it is created by a ReplicaSet, nobody reconciles it against
// git, and mutating it is how every sidecar in the ecosystem works.
//
// The agent needs no code change. Its base URLs are rewritten to the sidecar on
// loopback, and the sidecar proxies onward to the real provider.
package inject

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/waveoffai/waveoff/api/v1alpha1"
)

// Annotations and labels that drive injection.
const (
	// AnnotationInject opts a pod in. Absent or anything but "true" means the
	// pod is left exactly as it was.
	AnnotationInject = "waveoff.ai/inject"
	// AnnotationManifest names an AgentManifest in the same namespace. Its
	// digests are stamped into every cassette, which is what makes the corpus
	// attributable to a version rather than being a pile of traffic.
	AnnotationManifest = "waveoff.ai/manifest"
	// AnnotationContainer names the agent container, for pods with more than
	// one. Guessing here would silently record the wrong thing.
	AnnotationContainer = "waveoff.ai/container"
	// AnnotationModelUpstream overrides the detected provider endpoint.
	AnnotationModelUpstream = "waveoff.ai/model-upstream"
	// AnnotationToolUpstreams is a comma-separated list of <label>=<url>.
	AnnotationToolUpstreams = "waveoff.ai/tool-upstreams"
	// AnnotationShadow marks this pod as receiving mirrored production traffic.
	//
	// The sidecar then refuses every tool call classified as a write. Mirroring
	// alone stops nothing: without this the candidate files the ticket, sends
	// the email and charges the card exactly as it would in production, and a
	// shadow deployment becomes a second live one.
	AnnotationShadow = "waveoff.ai/shadow"

	// AnnotationEgressConfined attests that the pod cannot reach anything
	// except through the recorder's proxies.
	//
	// Write suppression is total only if every side effect flows through the
	// tool plane. An agent can also write over plain HTTP that never touches
	// MCP, through a database driver, an object-storage SDK, a message queue,
	// or the filesystem — and the suppressor sees none of it. Injecting a
	// shadow sidecar without confining egress produces a partial guarantee
	// presented as a complete one, and the failure mode is a shadow candidate
	// quietly writing to production.
	//
	// This annotation is an attestation, not a check: nothing here can verify
	// a NetworkPolicy actually exists and covers everything. It is a deliberate
	// obstacle, so that confining egress is a decision somebody made rather
	// than something nobody thought about. config/samples/shadow-egress.yaml
	// is the policy it is attesting to.
	AnnotationEgressConfined = "waveoff.ai/egress-confined"

	// AnnotationOTLPEndpoint sends spans on to a collector as well as into
	// cassettes. Absent means no export at all, which is the default: the
	// recorder must never dial somewhere nobody configured.
	AnnotationOTLPEndpoint = "waveoff.ai/otlp-endpoint"
	// AnnotationOTLPProtocol is grpc (default) or http.
	AnnotationOTLPProtocol = "waveoff.ai/otlp-protocol"

	// AnnotationSessionIdle overrides how long a session may be quiet before
	// its cassette is closed. A session has no explicit end — the agent just
	// stops calling — so quiet is the only signal, and how long to wait depends
	// on how the agent behaves.
	AnnotationSessionIdle = "waveoff.ai/session-idle"

	// AnnotationCorpusClaim mounts an existing PersistentVolumeClaim for the
	// corpus instead of an emptyDir. Without it, recordings live and die with
	// the pod.
	AnnotationCorpusClaim = "waveoff.ai/corpus-claim"

	// AnnotationInjected is stamped on the way out, so a second pass is a
	// no-op and an operator can see what happened.
	AnnotationInjected = "waveoff.ai/injected"

	// SidecarName is the container this webhook adds (§12).
	SidecarName = "waveoff-recorder"
	// ListenPort is where the sidecar listens on loopback.
	ListenPort = 8080
	// BinaryPath is where the recorder binary lives in its image. The
	// readiness probe execs it, so this is coupled to our own Dockerfile and
	// nothing else.
	BinaryPath = "/app"

	corpusVolume = "waveoff-corpus"
	corpusPath   = "/var/lib/waveoff/corpus"
	blobPath     = "/var/lib/waveoff/blobs"
)

// providerEnv maps a base-URL environment variable to the public endpoint it
// defaults to when unset. These are conventions rather than a standard, so an
// unrecognised setup is left alone rather than guessed at.
var providerEnv = []struct{ name, defaultURL string }{
	{"ANTHROPIC_BASE_URL", "https://api.anthropic.com"},
	{"OPENAI_BASE_URL", "https://api.openai.com"},
}

// The injector reads AgentManifests so a cassette can name the version it was
// recorded against. Read-only, and nothing else: this webhook runs on every pod
// creation in the cluster, so its blast radius should be as small as the job
// allows.
// +kubebuilder:rbac:groups=waveoff.ai,resources=agentmanifests,verbs=get;list;watch

// +kubebuilder:webhook:path=/mutate-v1-pod,mutating=true,failurePolicy=ignore,matchPolicy=Equivalent,sideEffects=None,groups="",resources=pods,verbs=create,versions=v1,name=inject.waveoff.ai,admissionReviewVersions=v1

// Injector implements the pod mutating webhook.
type Injector struct {
	Client client.Reader
	// Image is the recorder image to inject.
	Image string
}

// SetupWithManager registers the webhook.
func SetupWithManager(mgr ctrl.Manager, image string) error {
	mgr.GetWebhookServer().Register("/mutate-v1-pod", &admission.Webhook{
		Handler: admission.WithCustomDefaulter(mgr.GetScheme(), &corev1.Pod{},
			&Injector{Client: mgr.GetClient(), Image: image}),
	})
	return nil
}

// The untyped seam. admission.Defaulter[T] is the replacement and would be an
// improvement — this injector casts to *corev1.Pod on entry — but migrating it
// inside a dependency bump makes both changes harder to review.
var _ admission.CustomDefaulter = &Injector{} //nolint:staticcheck

// Default injects the sidecar.
//
// Every failure path here leaves the pod untouched and returns nil. This
// webhook runs failurePolicy: Ignore for the same reason: a recorder that
// cannot be configured is a missing recording, while a webhook that blocks pod
// creation is an outage. Observability must never be able to take down the
// thing it observes.
func (i *Injector) Default(ctx context.Context, obj runtime.Object) error {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return nil
	}
	if !wants(pod) {
		return nil
	}
	if pod.Annotations[AnnotationInjected] != "" {
		return nil
	}
	if hasContainer(pod, SidecarName) {
		return nil
	}

	idx, err := agentContainer(pod)
	if err != nil {
		return admissionWarning(pod, err)
	}
	agent := &pod.Spec.Containers[idx]

	upstream, envName := modelUpstream(pod, agent)
	tools, err := toolUpstreams(pod)
	if err != nil {
		return admissionWarning(pod, err)
	}
	if upstream == "" && len(tools) == 0 {
		return admissionWarning(pod, fmt.Errorf(
			"no model or tool upstream could be determined; set %s or %s",
			AnnotationModelUpstream, AnnotationToolUpstreams))
	}

	behavior, content, agentName, effects := i.manifestIdentity(ctx, pod)

	shadow := pod.Annotations[AnnotationShadow] == "true"
	if shadow && pod.Annotations[AnnotationEgressConfined] != "true" {
		// The same reasoning as refusing a shadow pod with no classifications:
		// a partial guarantee is indistinguishable from a complete one until
		// the day it is not, and here the stakes are writes reaching
		// production.
		return admissionWarning(pod, fmt.Errorf(
			"%s is set but %s is not. Suppressing writes only covers tools that go through the "+
				"MCP proxy; direct HTTP, database drivers, object storage, queues and the filesystem "+
				"all bypass it. Confine the pod's egress to the sidecar (see "+
				"config/samples/shadow-egress.yaml) and set %s=true to attest to it",
			AnnotationShadow, AnnotationEgressConfined, AnnotationEgressConfined))
	}
	if shadow && len(effects) == 0 {
		// Refusing every tool call is safe and useless: the candidate cannot
		// complete a session, so the shadow deployment observes nothing.
		return admissionWarning(pod, fmt.Errorf(
			"%s is set but no tool effects could be read from %s; a shadow pod with no "+
				"classifications would refuse every tool call and observe nothing",
			AnnotationShadow, AnnotationManifest))
	}

	// Point the agent at the sidecar. This is the whole zero-code-change trick:
	// the agent keeps talking to what it thinks is its provider.
	if envName != "" {
		setEnv(agent, envName, fmt.Sprintf("http://127.0.0.1:%d", ListenPort))
	}
	for label := range tools {
		// Tool endpoints are exposed per label so one sidecar can front
		// several MCP servers.
		setEnv(agent, "WAVEOFF_MCP_"+strings.ToUpper(sanitise(label)),
			fmt.Sprintf("http://127.0.0.1:%d/mcp/%s", ListenPort, label))
	}

	i.addVolume(pod)

	// A native sidecar, not an ordinary container.
	//
	// An init container with restartPolicy: Always is started before the app
	// containers and kept running for the pod's life. That ordering is the
	// whole point: as an ordinary container the recorder starts alongside the
	// agent, and the agent's first model calls fail with connection refused
	// against a proxy that is not listening yet. Observability that breaks the
	// first request of every pod does not survive contact with production.
	always := corev1.ContainerRestartPolicyAlways
	sc := i.sidecar(pod, upstream, tools, agentName, behavior, content, shadow, effects)
	sc.RestartPolicy = &always
	pod.Spec.InitContainers = append(pod.Spec.InitContainers, sc)

	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[AnnotationInjected] = i.Image
	return nil
}

func wants(pod *corev1.Pod) bool {
	return pod.Annotations[AnnotationInject] == "true" || pod.Labels[AnnotationInject] == "true"
}

func hasContainer(pod *corev1.Pod, name string) bool {
	for _, c := range pod.Spec.Containers {
		if c.Name == name {
			return true
		}
	}
	// The recorder is injected as a native sidecar, so it lives among the init
	// containers rather than the ordinary ones.
	for _, c := range pod.Spec.InitContainers {
		if c.Name == name {
			return true
		}
	}
	return false
}

// agentContainer picks which container is the agent. With several and no
// annotation this refuses rather than guessing: recording the wrong container
// produces a corpus that looks fine and describes nothing.
func agentContainer(pod *corev1.Pod) (int, error) {
	name := pod.Annotations[AnnotationContainer]
	if name == "" {
		if len(pod.Spec.Containers) == 1 {
			return 0, nil
		}
		return 0, fmt.Errorf("pod has %d containers; name the agent with %s",
			len(pod.Spec.Containers), AnnotationContainer)
	}
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == name {
			return i, nil
		}
	}
	return 0, fmt.Errorf("no container named %q", name)
}

// modelUpstream works out where model traffic really goes, and which
// environment variable to rewrite.
func modelUpstream(pod *corev1.Pod, agent *corev1.Container) (upstream, envName string) {
	if v := pod.Annotations[AnnotationModelUpstream]; v != "" {
		// Still needs a variable to rewrite; prefer one the container already
		// sets, otherwise fall back to the Anthropic convention.
		for _, p := range providerEnv {
			if getEnv(agent, p.name) != "" {
				return v, p.name
			}
		}
		return v, providerEnv[0].name
	}
	for _, p := range providerEnv {
		if v := getEnv(agent, p.name); v != "" {
			return v, p.name
		}
	}
	// No explicit base URL: infer the provider from whichever API key is set,
	// and rewrite the corresponding variable so the agent starts using it.
	for _, p := range providerEnv {
		keyVar := strings.TrimSuffix(p.name, "_BASE_URL") + "_API_KEY"
		if getEnv(agent, keyVar) != "" || hasEnvFrom(agent, keyVar) {
			return p.defaultURL, p.name
		}
	}
	return "", ""
}

func toolUpstreams(pod *corev1.Pod) (map[string]string, error) {
	raw := pod.Annotations[AnnotationToolUpstreams]
	if raw == "" {
		return nil, nil
	}
	out := map[string]string{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		label, endpoint, ok := strings.Cut(part, "=")
		if !ok || label == "" || endpoint == "" {
			return nil, fmt.Errorf("%s: %q is not <label>=<url>", AnnotationToolUpstreams, part)
		}
		out[strings.TrimSpace(label)] = strings.TrimSpace(endpoint)
	}
	return out, nil
}

// manifestIdentity resolves the AgentManifest named on the pod, so cassettes
// carry the version they were recorded against.
func (i *Injector) manifestIdentity(ctx context.Context, pod *corev1.Pod) (
	behavior, content, agent string, effects map[string]v1alpha1.ToolEffect) {

	name := pod.Annotations[AnnotationManifest]
	if name == "" || i.Client == nil {
		return "", "", pod.Labels["app"], nil
	}
	var m v1alpha1.AgentManifest
	key := types.NamespacedName{Namespace: pod.Namespace, Name: name}
	if err := i.Client.Get(ctx, key, &m); err != nil {
		// The manifest may not exist yet, or RBAC may not allow the read.
		// Recording without identity is degraded; refusing to start the pod
		// would be worse.
		return "", "", pod.Labels["app"], nil
	}

	// The manifest is where tool effects are asserted, so it is also where a
	// shadow pod learns what it must not run.
	effects = make(map[string]v1alpha1.ToolEffect, len(m.Spec.Tools))
	for _, t := range m.Spec.Tools {
		effects[t.Name] = t.Effect
	}
	return m.Spec.BehaviorDigest, m.Spec.ContentDigest, m.Spec.Agent, effects
}

func (i *Injector) addVolume(pod *corev1.Pod) {
	for _, v := range pod.Spec.Volumes {
		if v.Name == corpusVolume {
			return
		}
	}
	vol := corev1.Volume{Name: corpusVolume}
	if claim := pod.Annotations[AnnotationCorpusClaim]; claim != "" {
		vol.PersistentVolumeClaim = &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim}
	} else {
		// Without a claim, recordings live and die with the pod. That is the
		// right default for trying the recorder out and the wrong one for
		// building a corpus, which is why the annotation exists.
		vol.EmptyDir = &corev1.EmptyDirVolumeSource{}
	}
	pod.Spec.Volumes = append(pod.Spec.Volumes, vol)
}

func (i *Injector) sidecar(pod *corev1.Pod, upstream string, tools map[string]string,
	agent, behavior, content string, shadow bool, effects map[string]v1alpha1.ToolEffect) corev1.Container {
	args := []string{
		"-listen", fmt.Sprintf("127.0.0.1:%d", ListenPort),
		"-corpus-dir", corpusPath,
		"-blob-dir", blobPath,
	}
	if upstream != "" {
		args = append(args, "-model-upstream", upstream)
	}
	for _, label := range sortedKeys(tools) {
		args = append(args, "-tool-upstream", label+"="+tools[label])
	}
	if agent != "" {
		args = append(args, "-agent", agent)
	}
	if behavior != "" {
		args = append(args, "-behavior-digest", behavior)
	}
	if content != "" {
		args = append(args, "-content-digest", content)
	}
	if shadow {
		args = append(args, "-shadow")
		for _, name := range sortedEffects(effects) {
			args = append(args, "-tool-effect", name+"="+string(effects[name]))
		}
	}
	if idle := pod.Annotations[AnnotationSessionIdle]; idle != "" {
		args = append(args, "-session-idle", idle)
	}
	if ep := pod.Annotations[AnnotationOTLPEndpoint]; ep != "" {
		args = append(args, "-otlp-endpoint", ep)
		if proto := pod.Annotations[AnnotationOTLPProtocol]; proto != "" {
			args = append(args, "-otlp-protocol", proto)
		}
	}

	yes := true
	no := false
	return corev1.Container{
		Name:            SidecarName,
		Image:           i.Image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Args:            args,
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: &no,
			ReadOnlyRootFilesystem:   &yes,
			RunAsNonRoot:             &yes,
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
		Resources: corev1.ResourceRequirements{},
		VolumeMounts: []corev1.VolumeMount{
			{Name: corpusVolume, MountPath: "/var/lib/waveoff"},
		},
		// An exec probe, not an HTTP one. The recorder binds loopback only, and
		// a Kubernetes HTTP probe is dialled from the kubelet's network
		// namespace, which cannot reach a pod's loopback — it fails with
		// connection refused however healthy the process is. Running the
		// binary's own healthcheck inside the container is the only vantage
		// point that works without giving up the loopback bind.
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				Exec: &corev1.ExecAction{Command: []string{
					BinaryPath, "-listen", fmt.Sprintf("127.0.0.1:%d", ListenPort), "-healthcheck",
				}},
			},
			InitialDelaySeconds: 1,
			PeriodSeconds:       5,
		},
	}
}

func getEnv(c *corev1.Container, name string) string {
	for _, e := range c.Env {
		if e.Name == name && e.ValueFrom == nil {
			return e.Value
		}
	}
	return ""
}

func hasEnvFrom(c *corev1.Container, name string) bool {
	for _, e := range c.Env {
		if e.Name == name {
			return true
		}
	}
	// envFrom pulls a whole ConfigMap or Secret in, so the variable may be
	// present without appearing here. Treat that as "possible".
	return len(c.EnvFrom) > 0
}

func setEnv(c *corev1.Container, name, value string) {
	for i := range c.Env {
		if c.Env[i].Name == name {
			c.Env[i] = corev1.EnvVar{Name: name, Value: value}
			return
		}
	}
	c.Env = append(c.Env, corev1.EnvVar{Name: name, Value: value})
}

// admissionWarning records why injection was skipped and lets the pod through.
func admissionWarning(pod *corev1.Pod, err error) error {
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations["waveoff.ai/inject-skipped"] = err.Error()
	return nil
}

func sanitise(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

func sortedEffects(m map[string]v1alpha1.ToolEffect) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
