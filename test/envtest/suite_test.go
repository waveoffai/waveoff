// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

// Package envtest runs the checks that only a real API server can answer.
//
// Everything here exists because of one hazard: values do not go from a YAML
// file straight into the digest function. They round-trip through the API
// server, which re-decodes and re-encodes every number, and through
// server-side apply, which owns fields. A digest that is stable in a unit test
// and unstable through etcd would be worse than no digest at all.
package envtest

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/waveoffai/waveoff/api/v1alpha1"
	"github.com/waveoffai/waveoff/internal/manifest"
)

var (
	cfg *rest.Config
	// webhookOpts carries the local serving details envtest rewrote into the
	// ValidatingWebhookConfiguration, so the webhook tests can start the real
	// server against them.
	webhookOpts *envtest.WebhookInstallOptions
)

func TestMain(m *testing.M) {
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		// Install the real ValidatingWebhookConfiguration, so the tests
		// exercise the wiring — path, rules, failurePolicy — and not just the
		// validator function.
		WebhookInstallOptions: envtest.WebhookInstallOptions{
			Paths: []string{filepath.Join("..", "..", "config", "webhook", "manifests.yaml")},
		},
	}
	var err error
	cfg, err = env.Start()
	if err != nil {
		// A plain `go test ./...` on a fresh checkout has no API server
		// binaries, and failing the whole suite for that is unhelpful. But if
		// KUBEBUILDER_ASSETS is set, somebody meant to run these — CI does —
		// and a silent skip there would hide exactly the failures this package
		// exists to catch.
		if os.Getenv("KUBEBUILDER_ASSETS") != "" {
			os.Stderr.WriteString("envtest was configured but did not start: " + err.Error() + "\n")
			os.Exit(1)
		}
		os.Stderr.WriteString("SKIP: envtest binaries not found; run `make test-envtest`\n")
		os.Exit(0)
	}
	webhookOpts = &env.WebhookInstallOptions

	// The ValidatingWebhookConfiguration is installed for the whole
	// environment and runs failurePolicy: Fail, so the server has to be up
	// before any test creates anything. That is also how a real cluster
	// behaves, which makes every test in this package an end-to-end one.
	stop, err := startWebhookServer()
	if err != nil {
		os.Stderr.WriteString("could not start the webhook server: " + err.Error() + "\n")
		_ = env.Stop()
		os.Exit(1)
	}

	code := m.Run()
	stop()
	_ = env.Stop()
	os.Exit(code)
}

func newClient(t *testing.T) client.Client {
	t.Helper()
	c, err := client.New(cfg, client.Options{Scheme: manifest.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func f(v float64) *float64 { return &v }
func i(v int64) *int64     { return &v }

func hash(c byte) string {
	b := make([]byte, 64)
	for i := range b {
		b[i] = c
	}
	return "sha256:" + string(b)
}

func spec(agent string) v1alpha1.AgentManifestSpec {
	return v1alpha1.AgentManifestSpec{
		Agent: agent,
		Code:  v1alpha1.CodeSpec{Image: "registry.internal/" + agent + "@" + hash('a')},
		Model: v1alpha1.ModelSpec{
			Provider: "anthropic", ID: "claude-sonnet-4-6", Pin: "2026-05-01",
			Params: v1alpha1.ModelParams{
				// The awkward values on purpose: a float that is exactly
				// integral (the API server decodes it as int64), one that is
				// not, a very small one, and a very large one.
				Temperature: f(1.0), TopP: f(0.95), TopK: i(40), MaxTokens: i(4096),
			},
		},
		Prompts: []v1alpha1.PromptRef{{Name: "system", Source: "git+ssh://x/p@a1b2c3d", Digest: hash('b')}},
		Tools: []v1alpha1.ToolRef{
			{Name: "docs.search", Server: "https://docs/mcp", ContractDigest: hash('c'),
				Effect: v1alpha1.EffectRead, ReplayPolicy: v1alpha1.ReplaySnapshot},
			{Name: "jira.create", Server: "https://jira/mcp", ContractDigest: hash('d'),
				Effect: v1alpha1.EffectWrite, ReplayPolicy: v1alpha1.ReplayNoOp},
		},
		Retrieval: &v1alpha1.RetrievalSpec{IndexSnapshot: "snap-2026-08-19T04:00Z", EmbeddingModel: "voyage-3"},
		Policy:    &v1alpha1.PolicySpec{BundleDigest: hash('e')},
		Judges: []v1alpha1.JudgeSpec{{Name: "task-completion", Model: "claude-opus-5", RubricDigest: hash('f'),
			Calibration: &v1alpha1.JudgeCalibration{
				Kappa:         f(0.71),
				GoldSetDigest: hash('0'),
				MeasuredAt:    ptrTime(metav1.NewTime(time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC))),
			}}},
	}
}

func ptrTime(t metav1.Time) *metav1.Time { return &t }

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return c
}

func key(m *v1alpha1.AgentManifest) types.NamespacedName {
	return types.NamespacedName{Namespace: m.Namespace, Name: m.Name}
}

func key2(ns, name string) types.NamespacedName {
	return types.NamespacedName{Namespace: ns, Name: name}
}
