// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"context"
	"math"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/waveoffai/waveoff/api/v1alpha1"
	"github.com/waveoffai/waveoff/internal/digest"
)

func h(n byte) string { return "sha256:" + strings.Repeat(string("0123456789abcdef"[n%16]), 64) }

// valid builds a manifest that admission should accept, with correct digests
// and a correctly derived name.
func valid(mutate ...func(*v1alpha1.AgentManifest)) *v1alpha1.AgentManifest {
	m := &v1alpha1.AgentManifest{
		ObjectMeta: metav1.ObjectMeta{Namespace: "prod"},
		Spec: v1alpha1.AgentManifestSpec{
			Agent: "support-agent",
			Code:  v1alpha1.CodeSpec{Image: "registry.internal/support-agent@" + h(1)},
			Model: v1alpha1.ModelSpec{Provider: "anthropic", ID: "claude-sonnet-4-6"},
			Tools: []v1alpha1.ToolRef{
				{Name: "docs.search", Server: "https://docs/mcp", ContractDigest: h(3), Effect: v1alpha1.EffectRead},
			},
		},
	}
	for _, f := range mutate {
		f(m)
	}
	reseal(m)
	return m
}

// reseal recomputes digests and name, the way `waveoff verify --write` does.
func reseal(m *v1alpha1.AgentManifest) {
	b, c, err := digest.Both(&m.Spec)
	if err != nil {
		return
	}
	m.Spec.BehaviorDigest, m.Spec.ContentDigest = b, c
	m.Name = digest.Name(m.Spec.Agent, b)
}

func create(t *testing.T, m *v1alpha1.AgentManifest) error {
	t.Helper()
	_, err := (&Validator{}).ValidateCreate(context.Background(), m)
	return err
}

func TestAcceptsAWellFormedManifest(t *testing.T) {
	if err := create(t, valid()); err != nil {
		t.Fatalf("a correct manifest was rejected: %v", err)
	}
}

// TestRejectsAbsentDigests: nothing computes them at admission, by design.
func TestRejectsAbsentDigests(t *testing.T) {
	m := valid()
	m.Spec.BehaviorDigest = ""
	m.Spec.ContentDigest = ""

	err := create(t, m)
	if err == nil {
		t.Fatal("a manifest with no digests was admitted")
	}
	for _, want := range []string{"behaviorDigest", "contentDigest", "waveoff verify --write"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("rejection should mention %q; got: %v", want, err)
		}
	}
}

// TestRejectionNamesBothValues: a digest mismatch is otherwise two
// indistinguishable hex strings and an operator cannot act on it.
func TestRejectionNamesBothValues(t *testing.T) {
	m := valid()
	stated := h(12)
	m.Spec.BehaviorDigest = stated

	err := create(t, m)
	if err == nil {
		t.Fatal("a manifest with a wrong behaviorDigest was admitted")
	}
	if !strings.Contains(err.Error(), stated) {
		t.Errorf("rejection should quote the stated value; got: %v", err)
	}
	if !strings.Contains(err.Error(), "computed") {
		t.Errorf("rejection should quote the computed value; got: %v", err)
	}
}

func TestRejectsTamperedContentDigest(t *testing.T) {
	m := valid()
	// Change a field classified ContentOnly, leaving behaviorDigest correct.
	m.Spec.Prompts = []v1alpha1.PromptRef{{Name: "system", Source: "git+ssh://x@1", Digest: h(2)}}
	reseal(m)
	m.Spec.Prompts[0].Source = "git+ssh://tampered@1"

	if err := create(t, m); err == nil {
		t.Fatal("provenance was edited after sealing and admission accepted it; " +
			"contentDigest exists precisely to catch this")
	}
}

// TestRejectsUnclassifiedEffect is the fail-closed rule.
func TestRejectsUnclassifiedEffect(t *testing.T) {
	m := valid(func(m *v1alpha1.AgentManifest) { m.Spec.Tools[0].Effect = "" })

	err := create(t, m)
	if err == nil {
		t.Fatal("a tool with no effect was admitted; it would be free to write during replay")
	}
	if !strings.Contains(err.Error(), "fails closed") && !strings.Contains(err.Error(), "untrusted") {
		t.Errorf("rejection should explain why effect is mandatory; got: %v", err)
	}
}

func TestRejectsUnknownEffect(t *testing.T) {
	m := valid(func(m *v1alpha1.AgentManifest) { m.Spec.Tools[0].Effect = "maybe-write" })
	if err := create(t, m); err == nil {
		t.Fatal("an unknown effect was admitted")
	}
}

func TestRejectsTaggedImage(t *testing.T) {
	m := valid(func(m *v1alpha1.AgentManifest) { m.Spec.Code.Image = "registry.internal/support-agent:v4" })

	err := create(t, m)
	if err == nil {
		t.Fatal("a tag-pinned image was admitted; a tag is mutable and pins nothing")
	}
	if !strings.Contains(err.Error(), "digest-pinned") {
		t.Errorf("rejection should say what is wrong; got: %v", err)
	}
}

func TestRejectsDuplicateNames(t *testing.T) {
	m := valid(func(m *v1alpha1.AgentManifest) {
		m.Spec.Tools = append(m.Spec.Tools, v1alpha1.ToolRef{
			Name: "docs.search", ContractDigest: h(4), Effect: v1alpha1.EffectRead})
	})
	if err := create(t, m); err == nil {
		t.Fatal("two tools with the same name were admitted; these are sets keyed by name")
	}
}

func TestRejectsNonFiniteNumerics(t *testing.T) {
	inf := math.Inf(1)
	m := valid(func(m *v1alpha1.AgentManifest) { m.Spec.Model.Params.Temperature = &inf })
	if err := create(t, m); err == nil {
		t.Fatal("a non-finite temperature was admitted; JSON cannot represent it")
	}
}

func TestRejectsAnArbitraryName(t *testing.T) {
	m := valid()
	m.Name = "my-favourite-agent"

	err := create(t, m)
	if err == nil {
		t.Fatal("an arbitrary name was admitted; a manifest's name is its identity")
	}
	if !strings.Contains(err.Error(), digest.Name(m.Spec.Agent, m.Spec.BehaviorDigest)) {
		t.Errorf("rejection should name the expected value; got: %v", err)
	}
}

// TestAcceptsDisambiguatedName covers the registry-migration case: two
// manifests sharing a behaviorDigest are the same agent and must both exist.
func TestAcceptsDisambiguatedName(t *testing.T) {
	m := valid()
	m.Name = digest.DisambiguatedName(m.Spec.Agent, m.Spec.BehaviorDigest, m.Spec.ContentDigest)
	if err := create(t, m); err != nil {
		t.Fatalf("the disambiguated name form was rejected: %v", err)
	}
}

func TestEvidenceAnnotationsAreFrozen(t *testing.T) {
	old := valid()
	old.Annotations = map[string]string{
		EvidencePrefix + "approver": "sre-oncall",
		"waveoff.ai/note":           "not evidence",
	}

	update := func(mutate func(map[string]string)) error {
		next := valid()
		next.Annotations = map[string]string{}
		for k, v := range old.Annotations {
			next.Annotations[k] = v
		}
		mutate(next.Annotations)
		_, err := (&Validator{}).ValidateUpdate(context.Background(), old, next)
		return err
	}

	if err := update(func(a map[string]string) { a[EvidencePrefix+"ticket"] = "CHG-1234" }); err != nil {
		t.Errorf("adding a new evidence key must be allowed: %v", err)
	}
	if err := update(func(a map[string]string) { a[EvidencePrefix+"approver"] = "someone-else" }); err == nil {
		t.Error("rewriting an approver was allowed; contentDigest does not cover metadata, " +
			"so nothing else would catch it")
	}
	if err := update(func(a map[string]string) { delete(a, EvidencePrefix+"approver") }); err == nil {
		t.Error("deleting an evidence annotation was allowed")
	}
	if err := update(func(a map[string]string) { a["waveoff.ai/note"] = "changed freely" }); err != nil {
		t.Errorf("non-evidence annotations must stay mutable for operations: %v", err)
	}
}

// TestValidateMatchesVerify: `waveoff verify` promises that a manifest passing
// locally is one the cluster accepts, which only holds if both run the same
// checks. Validate is exported for exactly this reason.
func TestValidateIsExportedForTheCLI(t *testing.T) {
	if errs := Validate(valid()); len(errs) != 0 {
		t.Fatalf("Validate rejected a valid manifest: %v", errs)
	}
	broken := valid()
	broken.Spec.Code.Image = "nope:latest"
	if errs := Validate(broken); len(errs) == 0 {
		t.Fatal("Validate accepted a tagged image")
	}
}
