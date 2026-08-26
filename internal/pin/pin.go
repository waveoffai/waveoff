// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

// Package pin builds a best-effort AgentManifest from a running Deployment.
//
// Adoption friction is the whole ballgame: nobody hand-writes this YAML, and a
// manifest nobody writes pins nothing. So pin infers what it safely can, and
// marks what it cannot with a TODO rather than guessing.
package pin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/waveoffai/waveoff/api/v1alpha1"
	"github.com/waveoffai/waveoff/internal/mcp"
)

// Options control what pin looks at.
type Options struct {
	Namespace  string
	Deployment string
	// Container names which container in the pod is the agent. Empty means the
	// first, which is right for the overwhelmingly common single-container pod.
	Container string
	// Agent overrides the logical agent name. Defaults to the Deployment name.
	Agent string
	// MCPServers maps a label to an endpoint URL, from --mcp-server.
	MCPServers map[string]string
	// AllowSecrets permits reading prompt bodies out of mounted Secrets.
	// Off by default: a prompt is usually not a credential, but a Secret is,
	// and hashing one into a git-committed artifact should be a deliberate act.
	AllowSecrets bool
	// Set carries --set overrides for fields that could not be inferred.
	Set map[string]string
}

// ToolContract pairs a discovered tool with the server that advertised it.
type ToolContract struct {
	Server string
	Tool   mcp.Tool
}

// Result is a manifest plus everything the operator still has to decide.
type Result struct {
	Manifest *v1alpha1.AgentManifest
	// Contracts holds the live tool contracts, so the emitter can render the
	// server's advisory hints as comments next to each unresolved effect.
	Contracts map[string]ToolContract
	// Notes are things the operator should read. They go to stderr, never into
	// the manifest.
	Notes []string
	// TODOs counts fields pin refused to guess. A non-zero count means the
	// manifest is not yet appliable, which is the honest outcome.
	TODOs int
}

// Reader is the subset of the controller-runtime client that pin needs.
type Reader interface {
	Get(ctx context.Context, key types.NamespacedName, obj client.Object, opts ...client.GetOption) error
	List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error
}

// ToolLister introspects one MCP server. Injected so tests do not need a live
// server, and so a future transport can be swapped in without touching pin.
type ToolLister func(ctx context.Context, endpoint string) ([]mcp.Tool, error)

// LiveTools is the default ToolLister.
func LiveTools(ctx context.Context, endpoint string) ([]mcp.Tool, error) {
	return mcp.New(endpoint).ListTools(ctx)
}

// Pin builds the manifest.
func Pin(ctx context.Context, r Reader, list ToolLister, opts Options) (*Result, error) {
	dep := &appsv1.Deployment{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: opts.Namespace, Name: opts.Deployment}, dep); err != nil {
		return nil, fmt.Errorf("get deployment %s/%s: %w", opts.Namespace, opts.Deployment, err)
	}

	agent := opts.Agent
	if agent == "" {
		agent = dep.Name
	}

	res := &Result{Contracts: map[string]ToolContract{}}
	spec := &v1alpha1.AgentManifestSpec{Agent: agent}

	ctr, err := pickContainer(dep, opts.Container)
	if err != nil {
		return nil, err
	}

	spec.Code.Image = resolveImage(ctx, r, dep, ctr.Name, res)
	spec.Model = inferModel(ctr, res)
	spec.Prompts = inferPrompts(ctx, r, dep, ctr, opts, res)
	spec.Tools = introspectTools(ctx, r, dep, ctr, list, opts, res)

	applyOverrides(spec, opts.Set, res)
	noteMissing(spec, res)

	res.Manifest = &v1alpha1.AgentManifest{
		TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "AgentManifest"},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: opts.Namespace,
			Labels:    map[string]string{"waveoff.ai/agent": agent},
		},
		Spec: *spec,
	}
	return res, nil
}

func pickContainer(dep *appsv1.Deployment, name string) (*corev1.Container, error) {
	ctrs := dep.Spec.Template.Spec.Containers
	if len(ctrs) == 0 {
		return nil, fmt.Errorf("deployment %s has no containers", dep.Name)
	}
	if name == "" {
		if len(ctrs) > 1 {
			names := make([]string, 0, len(ctrs))
			for _, c := range ctrs {
				names = append(names, c.Name)
			}
			return nil, fmt.Errorf("deployment %s has %d containers (%s); pick one with --container",
				dep.Name, len(ctrs), strings.Join(names, ", "))
		}
		return &ctrs[0], nil
	}
	for i := range ctrs {
		if ctrs[i].Name == name {
			return &ctrs[i], nil
		}
	}
	return nil, fmt.Errorf("deployment %s has no container %q", dep.Name, name)
}

// resolveImage prefers the digest of what is actually running over what the
// Deployment asks for. A pod template routinely carries a mutable tag, and a
// manifest that cannot say which bytes ran is not a release artifact.
func resolveImage(ctx context.Context, r Reader, dep *appsv1.Deployment, container string, res *Result) string {
	specImage := ""
	for _, c := range dep.Spec.Template.Spec.Containers {
		if c.Name == container {
			specImage = c.Image
		}
	}

	sel, err := metav1.LabelSelectorAsSelector(dep.Spec.Selector)
	if err == nil {
		pods := &corev1.PodList{}
		if err := r.List(ctx, pods, client.InNamespace(dep.Namespace), client.MatchingLabelsSelector{Selector: sel}); err == nil {
			if id := runningImageID(pods, container); id != "" {
				// Keep the repository the Deployment names, but take the digest
				// from what is running: the imageID may name a mirror or a
				// pull-through cache that means nothing to a reader.
				return repoOf(specImage) + "@" + id
			}
		}
	}

	if strings.Contains(specImage, "@sha256:") {
		return specImage
	}
	res.TODOs++
	res.Notes = append(res.Notes, fmt.Sprintf(
		"could not resolve a running image digest for container %q; the manifest carries the "+
			"Deployment's %q, which is a mutable tag. Apply will be rejected until it is digest-pinned.",
		container, specImage))
	return specImage
}

func runningImageID(pods *corev1.PodList, container string) string {
	// Prefer a Ready pod: a crash-looping one may be running the image that is
	// being rolled back from.
	best := ""
	for _, p := range pods.Items {
		ready := false
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				ready = true
			}
		}
		for _, cs := range p.Status.ContainerStatuses {
			if cs.Name != container {
				continue
			}
			if id := normaliseImageID(cs.ImageID); id != "" {
				if ready {
					return id
				}
				best = id
			}
		}
	}
	return best
}

// normaliseImageID extracts the sha256 from the many shapes a runtime reports.
func normaliseImageID(id string) string {
	if i := strings.Index(id, "@"); i >= 0 {
		id = id[i+1:]
	}
	if !strings.HasPrefix(id, "sha256:") {
		return ""
	}
	return id
}

func repoOf(image string) string {
	if i := strings.Index(image, "@"); i >= 0 {
		return image[:i]
	}
	// Strip a tag, but not a registry port: the colon that matters is the one
	// after the last slash.
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		return image[:i]
	}
	return image
}

// modelEnv maps environment variables onto manifest fields, most specific
// first. These are conventions, not a standard, so anything not matched is left
// for --set rather than guessed at.
var modelEnv = struct {
	id       []string
	temp     []string
	topP     []string
	maxToken []string
}{
	id:       []string{"ANTHROPIC_MODEL", "OPENAI_MODEL", "LLM_MODEL", "MODEL_ID", "MODEL"},
	temp:     []string{"LLM_TEMPERATURE", "MODEL_TEMPERATURE", "TEMPERATURE"},
	topP:     []string{"LLM_TOP_P", "MODEL_TOP_P", "TOP_P"},
	maxToken: []string{"LLM_MAX_TOKENS", "MODEL_MAX_TOKENS", "MAX_TOKENS"},
}

func inferModel(c *corev1.Container, res *Result) v1alpha1.ModelSpec {
	env := map[string]string{}
	for _, e := range c.Env {
		if e.ValueFrom == nil {
			env[e.Name] = e.Value
		}
	}

	m := v1alpha1.ModelSpec{}
	m.ID = firstOf(env, modelEnv.id)
	switch {
	case env["ANTHROPIC_MODEL"] != "" || env["ANTHROPIC_BASE_URL"] != "" || env["ANTHROPIC_API_KEY"] != "":
		m.Provider = "anthropic"
	case env["OPENAI_MODEL"] != "" || env["OPENAI_BASE_URL"] != "" || env["OPENAI_API_KEY"] != "":
		m.Provider = "openai"
	case strings.HasPrefix(m.ID, "claude-"):
		m.Provider = "anthropic"
	case strings.HasPrefix(m.ID, "gpt-") || strings.HasPrefix(m.ID, "o1") || strings.HasPrefix(m.ID, "o3"):
		m.Provider = "openai"
	}

	if v := firstOf(env, modelEnv.temp); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			m.Params.Temperature = &f
		}
	}
	if v := firstOf(env, modelEnv.topP); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			m.Params.TopP = &f
		}
	}
	if v := firstOf(env, modelEnv.maxToken); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			m.Params.MaxTokens = &n
		}
	}

	for _, e := range c.Env {
		if e.ValueFrom != nil && contains(modelEnv.id, e.Name) {
			res.Notes = append(res.Notes, fmt.Sprintf(
				"%s comes from a %s reference, so its value cannot be read from the Deployment; "+
					"set it with --set model.id=<value>", e.Name, sourceKind(e.ValueFrom)))
		}
	}
	return m
}

func sourceKind(s *corev1.EnvVarSource) string {
	switch {
	case s.ConfigMapKeyRef != nil:
		return "ConfigMap"
	case s.SecretKeyRef != nil:
		return "Secret"
	case s.FieldRef != nil:
		return "field"
	}
	return "external"
}

// promptExts are the file extensions treated as prompt bodies when a ConfigMap
// is mounted into the agent.
var promptExts = map[string]bool{".md": true, ".txt": true, ".prompt": true, ".tmpl": true, ".jinja": true}

func inferPrompts(ctx context.Context, r Reader, dep *appsv1.Deployment, c *corev1.Container, opts Options, res *Result) []v1alpha1.PromptRef {
	var out []v1alpha1.PromptRef
	seen := map[string]bool{}

	for _, vol := range mountedVolumes(dep, c) {
		switch {
		case vol.ConfigMap != nil:
			cm := &corev1.ConfigMap{}
			if err := r.Get(ctx, types.NamespacedName{Namespace: dep.Namespace, Name: vol.ConfigMap.Name}, cm); err != nil {
				res.Notes = append(res.Notes, fmt.Sprintf("could not read ConfigMap %q: %v", vol.ConfigMap.Name, err))
				continue
			}
			for _, key := range sortedKeys(cm.Data) {
				if !promptExts[strings.ToLower(path.Ext(key))] {
					continue
				}
				name := strings.TrimSuffix(key, path.Ext(key))
				if seen[name] {
					continue
				}
				seen[name] = true
				out = append(out, v1alpha1.PromptRef{
					Name:   name,
					Source: fmt.Sprintf("configmap://%s/%s#%s", dep.Namespace, cm.Name, key),
					Digest: sha256Of(cm.Data[key]),
				})
			}
		case vol.Secret != nil:
			// A prompt living in a Secret is unusual and a Secret being hashed
			// into a git-committed artifact is a decision, not a default.
			if !opts.AllowSecrets {
				res.Notes = append(res.Notes, fmt.Sprintf(
					"Secret %q is mounted and may hold prompts; pin did not read it. "+
						"Pass --allow-secrets if its contents really are prompt bodies.", vol.Secret.SecretName))
			}
		}
	}
	return out
}

func mountedVolumes(dep *appsv1.Deployment, c *corev1.Container) []corev1.Volume {
	mounted := map[string]bool{}
	for _, m := range c.VolumeMounts {
		mounted[m.Name] = true
	}
	var out []corev1.Volume
	for _, v := range dep.Spec.Template.Spec.Volumes {
		if mounted[v.Name] {
			out = append(out, v)
		}
	}
	return out
}

// mcpConfig is the shape of the widely-used mcp.json / .mcp.json file.
type mcpConfig struct {
	MCPServers map[string]struct {
		URL     string `json:"url"`
		Type    string `json:"type,omitempty"`
		Command string `json:"command,omitempty"`
	} `json:"mcpServers"`
}

func introspectTools(ctx context.Context, r Reader, dep *appsv1.Deployment, c *corev1.Container,
	list ToolLister, opts Options, res *Result) []v1alpha1.ToolRef {

	endpoints := map[string]string{}
	for k, v := range discoverMCPFromConfig(ctx, r, dep, c, res) {
		endpoints[k] = v
	}
	// Explicit flags win over discovery: the operator knows which endpoint the
	// agent actually reaches, and a mounted config may describe a dev setup.
	for k, v := range opts.MCPServers {
		endpoints[k] = v
	}
	if len(endpoints) == 0 {
		res.Notes = append(res.Notes, "no MCP servers found or given, so tools[] is empty. "+
			"Point pin at them with --mcp-server <label>=<url> so tool contracts can be pinned.")
		return nil
	}

	var out []v1alpha1.ToolRef
	for _, label := range sortedKeys(endpoints) {
		endpoint := endpoints[label]
		tools, err := list(ctx, endpoint)
		if err != nil {
			// A contract that cannot be read must not be invented. Emitting a
			// placeholder digest would produce a manifest that looks pinned and
			// is not.
			res.TODOs++
			res.Notes = append(res.Notes, fmt.Sprintf(
				"MCP server %q (%s) could not be introspected: %v. Its tools are absent from the "+
					"manifest — no contract digest is better than a made-up one.", label, endpoint, err))
			continue
		}
		for _, t := range tools {
			cd, err := t.ContractDigest()
			if err != nil {
				res.Notes = append(res.Notes, fmt.Sprintf("tool %q from %s: %v", t.Name, label, err))
				continue
			}
			out = append(out, v1alpha1.ToolRef{
				Name:           t.Name,
				Server:         endpoint,
				ContractDigest: cd,
				// Effect is deliberately left empty. See docs/digest.md: the
				// server is the untrusted party in the tool-poisoning threat
				// model, so its hints are rendered as a comment for a human to
				// accept, never promoted to a value.
				Effect: "",
			})
			res.Contracts[t.Name] = ToolContract{Server: endpoint, Tool: t}
			res.TODOs++
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func discoverMCPFromConfig(ctx context.Context, r Reader, dep *appsv1.Deployment, c *corev1.Container, res *Result) map[string]string {
	found := map[string]string{}
	for _, vol := range mountedVolumes(dep, c) {
		if vol.ConfigMap == nil {
			continue
		}
		cm := &corev1.ConfigMap{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: dep.Namespace, Name: vol.ConfigMap.Name}, cm); err != nil {
			continue
		}
		for _, key := range sortedKeys(cm.Data) {
			if key != "mcp.json" && key != ".mcp.json" {
				continue
			}
			var cfg mcpConfig
			if err := json.Unmarshal([]byte(cm.Data[key]), &cfg); err != nil {
				res.Notes = append(res.Notes, fmt.Sprintf("%s in ConfigMap %q is not valid JSON: %v", key, cm.Name, err))
				continue
			}
			for name, s := range cfg.MCPServers {
				if s.URL != "" {
					found[name] = s.URL
					continue
				}
				// stdio servers have no endpoint to introspect over HTTP.
				res.Notes = append(res.Notes, fmt.Sprintf(
					"MCP server %q in %s is a stdio server, which pin cannot introspect remotely; "+
						"its tools must be added by hand or captured by the recorder.", name, key))
			}
		}
	}
	return found
}

// applyOverrides handles --set for the handful of fields worth overriding from
// the command line. Anything broader belongs in the emitted YAML, which the
// operator is going to edit anyway.
func applyOverrides(spec *v1alpha1.AgentManifestSpec, set map[string]string, res *Result) {
	for _, k := range sortedKeys(set) {
		v := set[k]
		switch k {
		case "agent":
			spec.Agent = v
		case "model.id":
			spec.Model.ID = v
		case "model.provider":
			spec.Model.Provider = v
		case "model.pin":
			spec.Model.Pin = v
		case "code.image":
			spec.Code.Image = v
		case "model.params.temperature":
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				spec.Model.Params.Temperature = &f
			}
		case "model.params.maxTokens":
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				spec.Model.Params.MaxTokens = &n
			}
		default:
			res.Notes = append(res.Notes, fmt.Sprintf("--set %s is not a settable field; edit the emitted YAML instead", k))
		}
	}
}

// noteMissing records the fields pin has no way to discover. Retrieval, policy
// and judges live outside the Deployment entirely, so an empty section is
// expected rather than a failure — but it is not silently fine either, because
// an unpinned judge means gating on a yardstick nothing versions.
func noteMissing(spec *v1alpha1.AgentManifestSpec, res *Result) {
	if spec.Model.Provider == "" || spec.Model.ID == "" {
		res.TODOs++
		res.Notes = append(res.Notes, "the model could not be read from the Deployment's environment; "+
			"set it with --set model.provider=... --set model.id=...")
	}
	if len(spec.Prompts) == 0 {
		res.Notes = append(res.Notes, "no prompts found: pin reads prompt bodies from mounted ConfigMap keys "+
			"ending in .md/.txt/.prompt/.tmpl. If prompts are baked into the image, add them by hand.")
	}
	if spec.Retrieval == nil {
		res.Notes = append(res.Notes, "retrieval is unset. If this agent retrieves, pin the index snapshot by hand — "+
			"an unpinned index means yesterday's evaluation ran against a different corpus.")
	}
	if spec.Policy == nil {
		res.Notes = append(res.Notes, "policy is unset. If a policy bundle governs this agent's actions, pin its digest.")
	}
	if len(spec.Judges) == 0 {
		res.Notes = append(res.Notes, "judges is empty. If you gate on a judge, pin it here — "+
			"a silent judge model update is a release-blocking change disguised as a no-op.")
	}
}

func firstOf(env map[string]string, keys []string) string {
	for _, k := range keys {
		if v := env[k]; v != "" {
			return v
		}
	}
	return ""
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sha256Of(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}
