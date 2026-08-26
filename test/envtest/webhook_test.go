// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package envtest

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/waveoffai/waveoff/api/v1alpha1"
	"github.com/waveoffai/waveoff/internal/digest"
	webhookv1alpha1 "github.com/waveoffai/waveoff/internal/webhook/v1alpha1"
)

func admit(t *testing.T, m *v1alpha1.AgentManifest) error {
	t.Helper()
	return newClient(t).Create(ctx(t), m)
}

func manifestFor(t *testing.T, agent string, mutate func(*v1alpha1.AgentManifest)) *v1alpha1.AgentManifest {
	t.Helper()
	m := &v1alpha1.AgentManifest{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "AgentManifest"},
		ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
		Spec:       spec(agent),
	}
	if mutate != nil {
		mutate(m)
	}
	b, c, err := digest.Both(&m.Spec)
	if err == nil && m.Spec.BehaviorDigest == "" {
		m.Spec.BehaviorDigest, m.Spec.ContentDigest = b, c
	}
	if m.Name == "" {
		m.Name = digest.Name(m.Spec.Agent, m.Spec.BehaviorDigest)
	}
	return m
}

func TestWebhookAdmitsASealedManifest(t *testing.T) {
	if err := admit(t, manifestFor(t, "wh-ok-agent", nil)); err != nil {
		t.Fatalf("a correctly sealed manifest was rejected: %v", err)
	}
}

// TestWebhookRejectsAbsentDigests: nothing computes them at admission, by
// design, so an unsealed manifest must not slip through.
func TestWebhookRejectsAbsentDigests(t *testing.T) {
	m := manifestFor(t, "wh-unsealed-agent", nil)
	m.Spec.BehaviorDigest = ""
	m.Spec.ContentDigest = ""

	err := admit(t, m)
	if err == nil {
		t.Fatal("an unsealed manifest was admitted")
	}
	// Schema validation runs before validating webhooks, so this rejection
	// comes from the CRD rather than from the webhook. That ordering is
	// correct — it holds even where the webhook was never deployed — which is
	// why the CRD carries a written-out message instead of a bare pattern.
	if !strings.Contains(err.Error(), "waveoff verify --write") {
		t.Errorf("the rejection should tell the operator how to fix it: %v", err)
	}
}

func TestWebhookRejectsAWrongDigest(t *testing.T) {
	m := manifestFor(t, "wh-wrong-agent", nil)
	stated := "sha256:" + strings.Repeat("9", 64)
	m.Spec.BehaviorDigest = stated

	err := admit(t, m)
	if err == nil {
		t.Fatal("a manifest whose digest does not cover its spec was admitted")
	}
	if !strings.Contains(err.Error(), "computed") {
		t.Errorf("the rejection should name the computed value: %v", err)
	}
}

// TestWebhookRejectsUnclassifiedEffect is the fail-closed rule reaching the
// cluster: an unclassified tool must never be storable.
func TestWebhookRejectsUnclassifiedEffect(t *testing.T) {
	m := manifestFor(t, "wh-effect-agent", func(m *v1alpha1.AgentManifest) {
		m.Spec.Tools[0].Effect = ""
	})
	if err := admit(t, m); err == nil {
		t.Fatal("a tool with no effect was admitted")
	}
}

func TestWebhookRejectsATaggedImage(t *testing.T) {
	m := manifestFor(t, "wh-tag-agent", func(m *v1alpha1.AgentManifest) {
		m.Spec.Code.Image = "registry.internal/agent:v4"
	})
	err := admit(t, m)
	if err == nil {
		t.Fatal("a tag-pinned image was admitted")
	}
	if !strings.Contains(err.Error(), "digest-pinned") {
		t.Errorf("unexpected rejection: %v", err)
	}
}

func TestWebhookRejectsAnArbitraryName(t *testing.T) {
	m := manifestFor(t, "wh-name-agent", nil)
	m.Name = "whatever-i-like"
	if err := admit(t, m); err == nil {
		t.Fatal("an arbitrary name was admitted; a manifest's name is its identity")
	}
}

// TestWebhookFreezesEvidenceAnnotations covers the one guarantee CEL cannot
// provide: root-level validation rules cannot reference metadata.annotations,
// so this holds only while the webhook is reachable.
func TestWebhookFreezesEvidenceAnnotations(t *testing.T) {
	c := newClient(t)

	m := manifestFor(t, "wh-evidence-agent", nil)
	m.Annotations = map[string]string{
		webhookv1alpha1.EvidencePrefix + "approver": "sre-oncall",
		"waveoff.ai/note": "operational",
	}
	if err := c.Create(ctx(t), m); err != nil {
		t.Fatal(err)
	}

	reload := func() *v1alpha1.AgentManifest {
		got := &v1alpha1.AgentManifest{}
		if err := c.Get(ctx(t), key(m), got); err != nil {
			t.Fatal(err)
		}
		return got
	}

	// Adding a second evidence key is allowed: a CAB flow accretes.
	add := reload()
	add.Annotations[webhookv1alpha1.EvidencePrefix+"ticket"] = "CHG-1234"
	if err := c.Update(ctx(t), add); err != nil {
		t.Errorf("adding an evidence key must be allowed: %v", err)
	}

	rewrite := reload()
	rewrite.Annotations[webhookv1alpha1.EvidencePrefix+"approver"] = "someone-else"
	if err := c.Update(ctx(t), rewrite); err == nil {
		t.Error("an approver was rewritten after the fact; contentDigest does not cover metadata, " +
			"so nothing else would have caught it")
	}

	remove := reload()
	delete(remove.Annotations, webhookv1alpha1.EvidencePrefix+"approver")
	if err := c.Update(ctx(t), remove); err == nil {
		t.Error("an evidence annotation was deleted")
	}

	free := reload()
	free.Annotations["waveoff.ai/note"] = "changed freely"
	if err := c.Update(ctx(t), free); err != nil {
		t.Errorf("non-evidence annotations must stay mutable: %v", err)
	}
}

// TestWebhookAdmitsTheDisambiguatedName covers the registry migration end to
// end: two manifests share a behaviorDigest, are the same agent, and both must
// be storable.
func TestWebhookAdmitsTheDisambiguatedName(t *testing.T) {
	c := newClient(t)

	original := manifestFor(t, "wh-migration-agent", nil)
	if err := c.Create(ctx(t), original.DeepCopy()); err != nil {
		t.Fatal(err)
	}

	moved := manifestFor(t, "wh-migration-agent", func(m *v1alpha1.AgentManifest) {
		m.Spec.Code.Image = "mirror.example.com/team/wh-migration-agent@" + hash('a')
	})
	if moved.Spec.BehaviorDigest != original.Spec.BehaviorDigest {
		t.Fatalf("fixture is wrong: the registry move should not change behaviorDigest")
	}
	if moved.Spec.ContentDigest == original.Spec.ContentDigest {
		t.Fatal("fixture is wrong: the registry move should change contentDigest")
	}

	// The canonical name is taken, so the second legal form must be accepted.
	moved.Name = digest.DisambiguatedName(moved.Spec.Agent, moved.Spec.BehaviorDigest, moved.Spec.ContentDigest)
	if err := c.Create(ctx(t), moved); err != nil {
		t.Fatalf("the disambiguated name was rejected: %v", err)
	}

	list := &v1alpha1.AgentManifestList{}
	if err := c.List(ctx(t), list, client.InNamespace("default")); err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, item := range list.Items {
		if item.Spec.Agent == "wh-migration-agent" {
			found++
		}
	}
	if found != 2 {
		t.Errorf("expected both manifests to coexist, found %d", found)
	}
}
