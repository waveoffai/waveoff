// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

// Package e2e exercises Waveoff against a real cluster.
//
// envtest already covers the API server's behaviour, so this suite is not here
// to re-run those assertions. It is here for the things envtest structurally
// cannot reach:
//
//   - the webhook served by a real Deployment behind a real Service, with a
//     certificate issued by cert-manager and a CA bundle injected into the
//     ValidatingWebhookConfiguration — the wiring in config/default, which is
//     what actually breaks in an install;
//   - the CLI's own cluster path, which builds a client from a kubeconfig
//     rather than being handed one;
//   - the whole pin/verify/apply/diff loop as an operator runs it.
package e2e

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/waveoffai/waveoff/api/v1alpha1"
	"github.com/waveoffai/waveoff/internal/digest"
	"github.com/waveoffai/waveoff/internal/manifest"
)

var (
	k8s       client.Client
	namespace string
	cliPath   string
)

// scheme covers the waveoff types plus the built-ins this suite inspects to
// check the deployment is genuinely healthy.
var scheme = func() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(v1alpha1.AddToScheme(s))
	utilruntime.Must(corev1.AddToScheme(s))
	utilruntime.Must(appsv1.AddToScheme(s))
	utilruntime.Must(admissionv1.AddToScheme(s))
	utilruntime.Must(networkingv1.AddToScheme(s))
	// The traffic cases read CRD metadata to find out which Gateway API
	// channel is installed, because the answer changes what a live stage can
	// do — see traffic_test.go.
	utilruntime.Must(apiextensionsv1.AddToScheme(s))
	utilruntime.Must(gatewayv1.Install(s))
	return s
}()

func TestMain(m *testing.M) {
	cfg, err := config.GetConfig()
	if err != nil {
		fail("no cluster: %v\nrun: make test-e2e", err)
	}
	k8s, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		fail("could not build a client: %v", err)
	}

	cliPath = os.Getenv("WAVEOFF_BIN")
	if cliPath == "" {
		cliPath, _ = filepath.Abs(filepath.Join("..", "..", "bin", "waveoff"))
	}
	if _, err := os.Stat(cliPath); err != nil {
		fail("waveoff binary not found at %s: %v\nrun: make build", cliPath, err)
	}

	namespace = fmt.Sprintf("waveoff-e2e-%d", time.Now().UnixNano()%1e6)
	ctx := context.Background()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
	if err := k8s.Create(ctx, ns); err != nil {
		fail("could not create namespace %s: %v", namespace, err)
	}

	code := m.Run()

	if os.Getenv("KEEP_NAMESPACE") == "" {
		_ = k8s.Delete(ctx, ns)
	}
	os.Exit(code)
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return c
}

// waveoff runs the CLI the way an operator does, and returns its combined
// output and exit code.
func waveoff(t *testing.T, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(cliPath, args...)
	cmd.Env = append(os.Environ(), "NO_COLOR=1")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running %s %v: %v", cliPath, args, err)
	}
	return out.String(), code
}

const template = `apiVersion: waveoff.ai/v1alpha1
kind: AgentManifest
metadata:
  name: PLACEHOLDER
  namespace: NAMESPACE
spec:
  agent: AGENT
  behaviorDigest: ""
  contentDigest: ""
  code:
    image: registry.internal/AGENT@sha256:1111111111111111111111111111111111111111111111111111111111111111
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

// manifestFile writes an unsealed manifest and returns its path.
func manifestFile(t *testing.T, agent string, edits ...func(string) string) string {
	t.Helper()
	body := strings.NewReplacer("AGENT", agent, "NAMESPACE", namespace).Replace(template)
	for _, e := range edits {
		body = e(body)
	}
	path := filepath.Join(t.TempDir(), agent+".yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// seal runs `waveoff verify --write`, which is the only supported way to
// produce a digest. Nothing computes one at admission.
func seal(t *testing.T, path string) string {
	t.Helper()
	if out, code := waveoff(t, "verify", "--write", path); code != 0 {
		t.Fatalf("verify --write failed (%d): %s", code, out)
	}
	if out, code := waveoff(t, "verify", path); code != 0 {
		t.Fatalf("a freshly sealed manifest failed verification (%d): %s", code, out)
	}
	return nameIn(t, path)
}

func nameIn(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "  name: ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "  name:"))
		}
	}
	t.Fatalf("no metadata.name in %s", path)
	return ""
}

func apply(t *testing.T, path string) (string, error) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	docs, err := manifest.ReadAll(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 {
		t.Fatalf("expected one manifest in %s, got %d", path, len(docs))
	}
	m := docs[0]
	m.Namespace = namespace
	return m.Name, k8s.Create(ctx(t), m)
}

// TestDeploymentIsHealthy checks the thing an install actually gets wrong.
// Everything below depends on the webhook being reachable over TLS, so if the
// certificate or the CA injection is wrong this is the failure worth reading.
func TestDeploymentIsHealthy(t *testing.T) {
	dep := &appsv1.Deployment{}
	err := k8s.Get(ctx(t), types.NamespacedName{Namespace: "waveoff-system", Name: "waveoff-manager"}, dep)
	if err != nil {
		t.Fatalf("the manager Deployment is not installed: %v", err)
	}
	if dep.Status.ReadyReplicas < 1 {
		t.Fatalf("no ready replicas: %d/%d", dep.Status.ReadyReplicas, dep.Status.Replicas)
	}

	// The CA bundle is injected by cert-manager. Without it the API server
	// cannot verify the webhook's certificate, and under failurePolicy: Fail
	// every AgentManifest in the cluster is rejected.
	whc := &admissionv1.ValidatingWebhookConfiguration{}
	if err := k8s.Get(ctx(t), types.NamespacedName{Name: "validating-webhook-configuration"}, whc); err != nil {
		t.Fatalf("the ValidatingWebhookConfiguration is not installed: %v", err)
	}
	if len(whc.Webhooks) == 0 {
		t.Fatal("no webhooks configured")
	}
	w := whc.Webhooks[0]
	if len(w.ClientConfig.CABundle) == 0 {
		t.Error("no caBundle was injected; check the cert-manager.io/inject-ca-from annotation " +
			"and that it names a Certificate in the namespace the manager was deployed to")
	}
	if w.FailurePolicy == nil || string(*w.FailurePolicy) != "Fail" {
		t.Errorf("failurePolicy = %v, want Fail; digest verification must not be bypassable "+
			"by taking the webhook down", w.FailurePolicy)
	}
}

// TestSealedManifestApplies is the happy path an operator walks.
func TestSealedManifestApplies(t *testing.T) {
	path := manifestFile(t, "e2e-happy")
	name := seal(t, path)

	got, err := apply(t, path)
	if err != nil {
		t.Fatalf("a sealed manifest was rejected by the live webhook: %v", err)
	}
	if got != name {
		t.Errorf("applied as %q, expected %q", got, name)
	}

	stored := &v1alpha1.AgentManifest{}
	if err := k8s.Get(ctx(t), types.NamespacedName{Namespace: namespace, Name: name}, stored); err != nil {
		t.Fatal(err)
	}
	// And it must still verify after a round trip through etcd.
	b, c, err := digest.Both(&stored.Spec)
	if err != nil {
		t.Fatal(err)
	}
	if b != stored.Spec.BehaviorDigest || c != stored.Spec.ContentDigest {
		t.Error("the stored manifest no longer hashes to its own digests")
	}
}

// TestUnsealedManifestIsRejected: the CRD's CEL rule fires before the webhook,
// so this checks the operator gets the message that tells them what to run.
func TestUnsealedManifestIsRejected(t *testing.T) {
	path := manifestFile(t, "e2e-unsealed")
	// Deliberately not sealed. Give it a plausible name so the failure is
	// about the digests rather than the name.
	replaceInFile(t, path, "name: PLACEHOLDER", "name: e2e-unsealed-000000000000")

	_, err := apply(t, path)
	if err == nil {
		t.Fatal("an unsealed manifest was admitted")
	}
	if !strings.Contains(err.Error(), "waveoff verify --write") {
		t.Errorf("the rejection should tell the operator how to fix it: %v", err)
	}
}

func TestLiveWebhookRejections(t *testing.T) {
	cases := []struct {
		name string
		edit func(string) string
		want string
	}{
		{
			name: "tagged-image",
			edit: func(s string) string {
				return strings.Replace(s,
					"registry.internal/e2e-reject@sha256:1111111111111111111111111111111111111111111111111111111111111111",
					"registry.internal/e2e-reject:v4", 1)
			},
			want: "digest-pinned",
		},
		{
			name: "unclassified-effect",
			edit: func(s string) string { return strings.Replace(s, "effect: read", `effect: ""`, 1) },
			want: "effect",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := manifestFile(t, "e2e-reject", tc.edit)
			// Seal it so the digests are right: the rejection must come from
			// the rule under test, not from a stale hash.
			if out, code := waveoff(t, "verify", "--write", path); code != 0 {
				t.Fatalf("verify --write failed (%d): %s", code, out)
			}
			_, err := apply(t, path)
			if err == nil {
				t.Fatalf("%s was admitted", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("rejection should mention %q: %v", tc.want, err)
			}
		})
	}
}

func TestTamperedManifestIsRejected(t *testing.T) {
	path := manifestFile(t, "e2e-tampered")
	seal(t, path)
	// Edit a value without resealing, exactly as a hand-edit in review would.
	replaceInFile(t, path, "temperature: 0.2", "temperature: 0.9")

	if _, err := apply(t, path); err == nil {
		t.Fatal("a manifest edited after sealing was admitted")
	}
	if out, code := waveoff(t, "verify", path); code == 0 {
		t.Errorf("waveoff verify should have caught this before the cluster did:\n%s", out)
	}
}

// TestSpecIsImmutableInCluster exercises the CEL rule against a real API
// server, which is the guarantee that survives the webhook being down.
func TestSpecIsImmutableInCluster(t *testing.T) {
	path := manifestFile(t, "e2e-immutable")
	name := seal(t, path)
	if _, err := apply(t, path); err != nil {
		t.Fatal(err)
	}

	stored := &v1alpha1.AgentManifest{}
	if err := k8s.Get(ctx(t), types.NamespacedName{Namespace: namespace, Name: name}, stored); err != nil {
		t.Fatal(err)
	}
	stored.Spec.Model.ID = "claude-opus-5"
	err := k8s.Update(ctx(t), stored)
	if err == nil {
		t.Fatal("the spec was edited in place")
	}
	if !apierrors.IsInvalid(err) {
		t.Errorf("expected Invalid from CEL, got %T: %v", err, err)
	}
}

// TestEvidenceAnnotationsAreFrozenInCluster covers the guarantee CEL cannot
// provide, so it holds only while the webhook is reachable — which makes it
// worth checking against a real deployment rather than only in envtest.
func TestEvidenceAnnotationsAreFrozenInCluster(t *testing.T) {
	path := manifestFile(t, "e2e-evidence")
	name := seal(t, path)
	if _, err := apply(t, path); err != nil {
		t.Fatal(err)
	}
	key := types.NamespacedName{Namespace: namespace, Name: name}

	reload := func() *v1alpha1.AgentManifest {
		got := &v1alpha1.AgentManifest{}
		if err := k8s.Get(ctx(t), key, got); err != nil {
			t.Fatal(err)
		}
		if got.Annotations == nil {
			got.Annotations = map[string]string{}
		}
		return got
	}

	first := reload()
	first.Annotations["waveoff.ai/evidence.approver"] = "sre-oncall"
	if err := k8s.Update(ctx(t), first); err != nil {
		t.Fatalf("setting an evidence annotation failed: %v", err)
	}

	add := reload()
	add.Annotations["waveoff.ai/evidence.ticket"] = "CHG-1234"
	if err := k8s.Update(ctx(t), add); err != nil {
		t.Errorf("adding a second evidence key must be allowed: %v", err)
	}

	rewrite := reload()
	rewrite.Annotations["waveoff.ai/evidence.approver"] = "someone-else"
	if err := k8s.Update(ctx(t), rewrite); err == nil {
		t.Error("an approver was rewritten after the fact")
	}
}

// TestRegistryMigrationCoexists is the case the dual digest exists to serve,
// end to end: same bytes from a different registry, same agent, both stored.
func TestRegistryMigrationCoexists(t *testing.T) {
	original := manifestFile(t, "e2e-migration")
	seal(t, original)
	if _, err := apply(t, original); err != nil {
		t.Fatal(err)
	}

	moved := manifestFile(t, "e2e-migration", func(s string) string {
		return strings.Replace(s, "registry.internal/e2e-migration@", "mirror.example.com/team/e2e-migration@", 1)
	})
	seal(t, moved)

	// The canonical name now collides, so the disambiguated form is required.
	movedDocs := readSpec(t, moved)
	disambiguated := digest.DisambiguatedName(movedDocs.Agent, movedDocs.BehaviorDigest, movedDocs.ContentDigest)
	replaceInFile(t, moved, "name: "+nameIn(t, moved), "name: "+disambiguated)

	if _, err := apply(t, moved); err != nil {
		t.Fatalf("the disambiguated name was rejected: %v", err)
	}

	// And the CLI must call this promotable without a canary.
	out, code := waveoff(t, "diff", original, moved)
	if code != 1 {
		t.Errorf("a registry migration should exit 1 (provenance only), got %d:\n%s", code, out)
	}
	if !strings.Contains(out, "promotes without a canary") {
		t.Errorf("expected the provenance verdict:\n%s", out)
	}
}

// TestDiffAgainstLiveCluster exercises the CLI's own cluster path, which builds
// a client from a kubeconfig. envtest never runs that code.
func TestDiffAgainstLiveCluster(t *testing.T) {
	a := manifestFile(t, "e2e-diff")
	nameA := seal(t, a)
	if _, err := apply(t, a); err != nil {
		t.Fatal(err)
	}

	b := manifestFile(t, "e2e-diff", func(s string) string {
		return strings.Replace(s, "temperature: 0.2", "temperature: 0.7", 1)
	})
	nameB := seal(t, b)
	if _, err := apply(t, b); err != nil {
		t.Fatal(err)
	}

	// Both sides resolved from the cluster by name.
	out, code := waveoff(t, "diff", "-n", namespace, nameA, nameB)
	if code != 2 {
		t.Fatalf("expected exit 2 (behavioural), got %d:\n%s", code, out)
	}
	for _, want := range []string{"temperature", "0.2 → 0.7", "behavioural change"} {
		if !strings.Contains(out, want) {
			t.Errorf("output should contain %q:\n%s", want, out)
		}
	}

	// And a mixed file-versus-cluster comparison, which is how a reviewer
	// checks a candidate against what is deployed.
	mixed, code := waveoff(t, "diff", "-n", namespace, nameA, b)
	if code != 2 {
		t.Errorf("file-versus-cluster diff exited %d:\n%s", code, mixed)
	}
}

func TestServerSideApplyIsIdempotentInCluster(t *testing.T) {
	path := manifestFile(t, "e2e-ssa")
	name := seal(t, path)

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	docs, err := manifest.ReadAll(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	m := docs[0]
	m.Namespace = namespace

	opts := []client.PatchOption{client.FieldOwner("waveoff-e2e"), client.ForceOwnership}
	if err := k8s.Patch(ctx(t), m.DeepCopy(), client.Apply, opts...); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	key := types.NamespacedName{Namespace: namespace, Name: name}
	first := &v1alpha1.AgentManifest{}
	if err := k8s.Get(ctx(t), key, first); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Patch(ctx(t), m.DeepCopy(), client.Apply, opts...); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	second := &v1alpha1.AgentManifest{}
	if err := k8s.Get(ctx(t), key, second); err != nil {
		t.Fatal(err)
	}
	if first.ResourceVersion != second.ResourceVersion {
		t.Errorf("re-applying an unchanged manifest wrote to the object (%s → %s); GitOps would loop",
			first.ResourceVersion, second.ResourceVersion)
	}
}

func readSpec(t *testing.T, path string) *v1alpha1.AgentManifestSpec {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	docs, err := manifest.ReadAll(bytes.NewReader(raw))
	if err != nil || len(docs) != 1 {
		t.Fatalf("reading %s: %v", path, err)
	}
	return &docs[0].Spec
}

func replaceInFile(t *testing.T, path, old, new string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := strings.Replace(string(raw), old, new, 1)
	if out == string(raw) {
		t.Fatalf("%q not found in %s", old, path)
	}
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
		t.Fatal(err)
	}
}

func typesName(ns, name string) types.NamespacedName {
	return types.NamespacedName{Namespace: ns, Name: name}
}
