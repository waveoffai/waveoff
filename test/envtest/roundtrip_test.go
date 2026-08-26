// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package envtest

import (
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/waveoffai/waveoff/api/v1alpha1"
	"github.com/waveoffai/waveoff/internal/digest"
)

func sealed(t *testing.T, agent string) *v1alpha1.AgentManifest {
	t.Helper()
	s := spec(agent)
	b, c, err := digest.Both(&s)
	if err != nil {
		t.Fatal(err)
	}
	s.BehaviorDigest, s.ContentDigest = b, c
	return &v1alpha1.AgentManifest{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "AgentManifest"},
		ObjectMeta: metav1.ObjectMeta{Name: digest.Name(agent, b), Namespace: "default"},
		Spec:       s,
	}
}

// TestDigestSurvivesTheAPIServer is the test the whole canonicalisation design
// exists to pass. The API server decodes integral JSON numbers as int64 and
// everything else as float64, then re-encodes on the way out. If that round
// trip moved a digest, every manifest in the cluster would fail its own
// verification the moment it was read back.
func TestDigestSurvivesTheAPIServer(t *testing.T) {
	c := newClient(t)
	want := sealed(t, "roundtrip-agent")

	if err := c.Create(ctx(t), want.DeepCopy()); err != nil {
		t.Fatalf("create: %v", err)
	}

	got := &v1alpha1.AgentManifest{}
	if err := c.Get(ctx(t), key(want), got); err != nil {
		t.Fatalf("get: %v", err)
	}

	gotB, gotC, err := digest.Both(&got.Spec)
	if err != nil {
		t.Fatal(err)
	}
	if gotB != want.Spec.BehaviorDigest {
		t.Errorf("behaviorDigest changed across a store/load cycle:\n  before %s\n  after  %s",
			want.Spec.BehaviorDigest, gotB)
	}
	if gotC != want.Spec.ContentDigest {
		t.Errorf("contentDigest changed across a store/load cycle:\n  before %s\n  after  %s",
			want.Spec.ContentDigest, gotC)
	}
	if got.Spec.BehaviorDigest != gotB {
		t.Errorf("the stored digest no longer matches its own spec: stated %s, computed %s",
			got.Spec.BehaviorDigest, gotB)
	}
}

// TestIntegralFloatSurvives isolates the specific hazard: temperature 1.0 is
// stored as the JSON number 1, decoded as an int64, and handed back. RFC 8785
// renders 1.0 and 1 identically, which is what saves us — but only if the
// canonicaliser is actually in the path.
func TestIntegralFloatSurvives(t *testing.T) {
	c := newClient(t)
	m := sealed(t, "integral-float-agent")
	if *m.Spec.Model.Params.Temperature != 1.0 {
		t.Fatal("fixture should use an exactly-integral temperature")
	}
	if err := c.Create(ctx(t), m.DeepCopy()); err != nil {
		t.Fatalf("create: %v", err)
	}
	got := &v1alpha1.AgentManifest{}
	if err := c.Get(ctx(t), key(m), got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.Model.Params.Temperature == nil || *got.Spec.Model.Params.Temperature != 1.0 {
		t.Fatalf("temperature came back as %v", got.Spec.Model.Params.Temperature)
	}
	b, _, err := digest.Both(&got.Spec)
	if err != nil {
		t.Fatal(err)
	}
	if b != m.Spec.BehaviorDigest {
		t.Errorf("an integral float moved the digest: %s → %s", m.Spec.BehaviorDigest, b)
	}
}

// TestZeroTemperatureIsPreserved: the API server must not prune a zero-valued
// pointer field. If it did, "temperature: 0" (greedy decoding) would come back
// as absent (provider default) — a real behavioural change, silently applied.
func TestZeroTemperatureIsPreserved(t *testing.T) {
	c := newClient(t)
	m := sealed(t, "zero-temp-agent")
	zero := 0.0
	m.Spec.Model.Params.Temperature = &zero
	b, cd, err := digest.Both(&m.Spec)
	if err != nil {
		t.Fatal(err)
	}
	m.Spec.BehaviorDigest, m.Spec.ContentDigest = b, cd
	m.Name = digest.Name(m.Spec.Agent, b)

	if err := c.Create(ctx(t), m.DeepCopy()); err != nil {
		t.Fatalf("create: %v", err)
	}
	got := &v1alpha1.AgentManifest{}
	if err := c.Get(ctx(t), key(m), got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.Model.Params.Temperature == nil {
		t.Fatal("temperature 0 was pruned to absent; greedy decoding became the provider default")
	}
	if *got.Spec.Model.Params.Temperature != 0 {
		t.Fatalf("temperature = %v, want 0", *got.Spec.Model.Params.Temperature)
	}
}

// TestSpecIsImmutable exercises the CEL rule rather than the webhook, which is
// the point: immutability must survive the webhook being unavailable.
func TestSpecIsImmutable(t *testing.T) {
	c := newClient(t)
	m := sealed(t, "immutable-agent")
	if err := c.Create(ctx(t), m); err != nil {
		t.Fatalf("create: %v", err)
	}

	m.Spec.Model.ID = "claude-opus-5"
	err := c.Update(ctx(t), m)
	if err == nil {
		t.Fatal("the spec was edited in place; a manifest must be replaced, not amended")
	}
	if !apierrors.IsInvalid(err) {
		t.Errorf("expected an Invalid error from CEL validation, got %T: %v", err, err)
	}
}

// TestLabelsRemainMutable: immutability is about the release artifact, not
// about operations. Relabelling must keep working.
func TestLabelsRemainMutable(t *testing.T) {
	c := newClient(t)
	m := sealed(t, "labels-agent")
	if err := c.Create(ctx(t), m); err != nil {
		t.Fatal(err)
	}
	m.Labels = map[string]string{"waveoff.ai/rollout": "candidate"}
	if err := c.Update(ctx(t), m); err != nil {
		t.Errorf("labels must stay mutable: %v", err)
	}
}

// TestServerSideApplyIsIdempotent guards the failure mode that only appears on
// the second apply: defaulting plus field ownership producing an apply loop
// that GitOps never converges out of. There is no defaulting webhook precisely
// so this holds; the test proves it stayed that way.
func TestServerSideApplyIsIdempotent(t *testing.T) {
	c := newClient(t)
	m := sealed(t, "ssa-agent")
	opts := []client.PatchOption{client.FieldOwner("waveoff-test"), client.ForceOwnership}

	if err := c.Patch(ctx(t), m.DeepCopy(), client.Apply, opts...); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	first := &v1alpha1.AgentManifest{}
	if err := c.Get(ctx(t), key(m), first); err != nil {
		t.Fatal(err)
	}

	if err := c.Patch(ctx(t), m.DeepCopy(), client.Apply, opts...); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	second := &v1alpha1.AgentManifest{}
	if err := c.Get(ctx(t), key(m), second); err != nil {
		t.Fatal(err)
	}

	if first.ResourceVersion != second.ResourceVersion {
		t.Errorf("re-applying an unchanged manifest wrote to the object "+
			"(resourceVersion %s → %s). GitOps would loop on this.",
			first.ResourceVersion, second.ResourceVersion)
	}
	// And the object must still be byte-identical to what was applied: nothing
	// reorders the lists, nothing fills anything in.
	if len(second.Spec.Tools) != len(m.Spec.Tools) || second.Spec.Tools[0].Name != m.Spec.Tools[0].Name {
		t.Errorf("the stored tools list differs from what was applied: %+v", second.Spec.Tools)
	}
	b, _, err := digest.Both(&second.Spec)
	if err != nil {
		t.Fatal(err)
	}
	if b != m.Spec.BehaviorDigest {
		t.Errorf("the stored spec no longer hashes to what was applied")
	}
}

// TestListOrderIsPreserved: the projection sorts for hashing, but the stored
// object must keep the author's order. A webhook that reordered it would leave
// Argo CD and Flux showing permanent drift.
func TestListOrderIsPreserved(t *testing.T) {
	c := newClient(t)
	m := sealed(t, "order-agent")
	// jira.create sorts before docs.search alphabetically? No: docs < jira. So
	// apply them in reverse of sorted order and check nothing re-sorts them.
	m.Spec.Tools[0], m.Spec.Tools[1] = m.Spec.Tools[1], m.Spec.Tools[0]
	b, cd, err := digest.Both(&m.Spec)
	if err != nil {
		t.Fatal(err)
	}
	m.Spec.BehaviorDigest, m.Spec.ContentDigest = b, cd
	m.Name = digest.Name(m.Spec.Agent, b)

	if err := c.Create(ctx(t), m.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	got := &v1alpha1.AgentManifest{}
	if err := c.Get(ctx(t), key(m), got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.Tools[0].Name != "jira.create" {
		t.Errorf("the stored list was reordered: %s came back first", got.Spec.Tools[0].Name)
	}
}

// TestReorderingDoesNotChangeIdentity is the other half: an author who
// reorders their YAML must get the same manifest, not a new one.
func TestReorderingDoesNotChangeIdentity(t *testing.T) {
	a := spec("reorder-agent")
	b := spec("reorder-agent")
	b.Tools[0], b.Tools[1] = b.Tools[1], b.Tools[0]

	da, _, err := digest.Both(&a)
	if err != nil {
		t.Fatal(err)
	}
	db, _, err := digest.Both(&b)
	if err != nil {
		t.Fatal(err)
	}
	if da != db {
		t.Errorf("reordering tools minted a new identity:\n  %s\n  %s", da, db)
	}
}
