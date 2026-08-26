// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

// Package v1alpha1 contains the admission webhook for AgentManifest.
//
// The webhook only ever validates. There is deliberately no defaulting webhook:
// a mutating webhook that rewrites the stored object — whether by sorting lists
// or by filling in digests — leaves Argo CD and Flux showing a permanent,
// un-reconcilable difference between desired and live state, and this is a
// GitOps-native product. Digests are authored by `waveoff pin` and repaired by
// `waveoff verify --write`, so the object the cluster holds is byte-identical to
// the one in git, and the digest an auditor reads was committed rather than
// minted at admission.
package v1alpha1

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/waveoffai/waveoff/api/v1alpha1"
	"github.com/waveoffai/waveoff/internal/digest"
)

// EvidencePrefix marks annotations that form part of the release record —
// approver identity, change-ticket references. contentDigest covers the spec
// but not metadata, so these are frozen separately: they may be added, never
// changed or removed.
//
// Unlike spec immutability, this freeze cannot be backed by a CEL rule on the
// CRD. Root-level x-kubernetes-validations expressions can reference only
// metadata.name and metadata.generateName; referencing metadata.annotations is
// a compile error and the API server refuses to install the CRD. So this check
// lives only here, and it holds only while the webhook is reachable — which is
// one more reason failurePolicy is Fail. See docs/digest.md, "What the digests
// do not cover".
const EvidencePrefix = "waveoff.ai/evidence."

// +kubebuilder:webhook:path=/validate-waveoff-ai-v1alpha1-agentmanifest,mutating=false,failurePolicy=fail,matchPolicy=Equivalent,sideEffects=None,groups=waveoff.ai,resources=agentmanifests,verbs=create;update,versions=v1alpha1,name=vagentmanifest.waveoff.ai,admissionReviewVersions=v1

// Validator implements admission validation for AgentManifest.
type Validator struct{}

var _ webhook.CustomValidator = &Validator{} //nolint:staticcheck

// SetupWithManager registers the webhook.
//
// controller-runtime 0.24 made the builder generic: the object moves into the
// constructor and `For` is gone. WithCustomValidator is the untyped seam, which
// is the one this validator implements — a typed admission.Validator[T] would
// let the casts below go, and is worth doing on its own rather than inside a
// dependency bump.
func SetupWithManager(mgr ctrl.Manager) error {
	// The suppression sits on the first line of the chain because that is where
	// staticcheck anchors a diagnostic about a method further down it.
	//nolint:staticcheck // WithCustomValidator is the untyped seam; see above.
	return ctrl.NewWebhookManagedBy(mgr, &v1alpha1.AgentManifest{}).
		WithCustomValidator(&Validator{}).
		Complete()
}

var gk = schema.GroupKind{Group: "waveoff.ai", Kind: "AgentManifest"}

func (v *Validator) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	m, ok := obj.(*v1alpha1.AgentManifest)
	if !ok {
		return nil, fmt.Errorf("expected an AgentManifest, got %T", obj)
	}
	if errs := Validate(m); len(errs) > 0 {
		return nil, apierrors.NewInvalid(gk, m.Name, errs)
	}
	return nil, nil
}

func (v *Validator) ValidateUpdate(_ context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	old, ok := oldObj.(*v1alpha1.AgentManifest)
	if !ok {
		return nil, fmt.Errorf("expected an AgentManifest, got %T", oldObj)
	}
	m, ok := newObj.(*v1alpha1.AgentManifest)
	if !ok {
		return nil, fmt.Errorf("expected an AgentManifest, got %T", newObj)
	}

	errs := Validate(m)
	errs = append(errs, validateEvidenceAnnotations(old, m)...)
	if len(errs) > 0 {
		return nil, apierrors.NewInvalid(gk, m.Name, errs)
	}
	return nil, nil
}

func (v *Validator) ValidateDelete(context.Context, runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

// Validate runs every check the webhook applies. It is exported so that
// `waveoff verify` can enforce exactly what admission will, and a manifest that
// passes locally is one the cluster accepts.
func Validate(m *v1alpha1.AgentManifest) field.ErrorList {
	var errs field.ErrorList
	spec := field.NewPath("spec")

	errs = append(errs, validateImage(spec.Child("code", "image"), m.Spec.Code.Image)...)
	errs = append(errs, validateEffects(spec.Child("tools"), m.Spec.Tools)...)
	errs = append(errs, validateUniqueNames(spec, &m.Spec)...)

	// Recompute rather than trust. This is the entire security property of the
	// object, which is why the webhook runs with failurePolicy: Fail — with
	// Ignore, verification is bypassable by taking the webhook down.
	wantB, wantC, err := digest.Both(&m.Spec)
	if err != nil {
		errs = append(errs, field.Invalid(spec, "", err.Error()))
		return errs
	}

	if m.Spec.BehaviorDigest != wantB {
		errs = append(errs, field.Invalid(spec.Child("behaviorDigest"), m.Spec.BehaviorDigest,
			mismatch("behaviorDigest", m.Spec.BehaviorDigest, wantB)))
	}
	if m.Spec.ContentDigest != wantC {
		errs = append(errs, field.Invalid(spec.Child("contentDigest"), m.Spec.ContentDigest,
			mismatch("contentDigest", m.Spec.ContentDigest, wantC)))
	}
	errs = append(errs, validateName(m, wantB, wantC)...)
	return errs
}

// mismatch spells out both values. A digest rejection is otherwise two
// indistinguishable hex strings, and the fix is a copy-paste away.
func mismatch(which, stated, computed string) string {
	if stated == "" {
		return fmt.Sprintf("%s is required and is not computed for you: this is a GitOps-native "+
			"object, so nothing rewrites what you applied.\n  computed %s\nRun: waveoff verify --write <file>",
			which, computed)
	}
	return fmt.Sprintf("%s does not match the spec it claims to cover.\n  stated   %s\n  computed %s\n"+
		"Run: waveoff verify --write <file>", which, stated, computed)
}

func validateImage(p *field.Path, image string) field.ErrorList {
	if image == "" {
		return field.ErrorList{field.Required(p, "an agent manifest must pin the image that runs it")}
	}
	if !strings.Contains(image, "@sha256:") {
		return field.ErrorList{field.Invalid(p, image,
			"image must be digest-pinned (repository@sha256:...), not tagged. A tag is mutable, so a "+
				"manifest carrying one cannot say which bytes ran and is not a release artifact. "+
				"`waveoff pin` resolves the running digest for you.")}
	}
	return nil
}

// validateEffects enforces the fail-closed rule. An unclassified tool is
// refused during replay rather than passed through, so admitting one would let
// a tool nobody has classified write during an evaluation.
func validateEffects(p *field.Path, tools []v1alpha1.ToolRef) field.ErrorList {
	var errs field.ErrorList
	valid := map[v1alpha1.ToolEffect]bool{
		v1alpha1.EffectRead: true, v1alpha1.EffectIdempotentWrite: true, v1alpha1.EffectWrite: true,
	}
	for i, t := range tools {
		fp := p.Index(i).Child("effect")
		switch {
		case t.Effect == "":
			errs = append(errs, field.Required(fp, fmt.Sprintf(
				"tool %q has no effect. Effect is mandatory and must be asserted by an operator: "+
					"an unclassified tool fails closed during replay rather than being passed through. "+
					"Use read, idempotent-write or write. Server-advertised hints are not a substitute — "+
					"the server is the untrusted party here.", t.Name)))
		case !valid[t.Effect]:
			errs = append(errs, field.NotSupported(fp, t.Effect,
				[]string{string(v1alpha1.EffectRead), string(v1alpha1.EffectIdempotentWrite), string(v1alpha1.EffectWrite)}))
		}
	}
	return errs
}

func validateUniqueNames(p *field.Path, spec *v1alpha1.AgentManifestSpec) field.ErrorList {
	var errs field.ErrorList
	check := func(path *field.Path, names []string) {
		seen := map[string]int{}
		for i, n := range names {
			if first, dup := seen[n]; dup {
				errs = append(errs, field.Duplicate(path.Index(i).Child("name"),
					fmt.Sprintf("%q also appears at index %d; these are sets keyed by name", n, first)))
				continue
			}
			seen[n] = i
		}
	}
	check(p.Child("tools"), names(spec.Tools, func(t v1alpha1.ToolRef) string { return t.Name }))
	check(p.Child("prompts"), names(spec.Prompts, func(x v1alpha1.PromptRef) string { return x.Name }))
	check(p.Child("judges"), names(spec.Judges, func(j v1alpha1.JudgeSpec) string { return j.Name }))
	return errs
}

// validateName ties the object's name to its identity, so that `kubectl get
// agentmanifests` is a list of agent versions rather than a list of labels
// somebody chose.
func validateName(m *v1alpha1.AgentManifest, behavior, content string) field.ErrorList {
	p := field.NewPath("metadata", "name")
	canonical := digest.Name(m.Spec.Agent, behavior)
	if m.Name == canonical {
		return nil
	}
	// The second legal form exists for the registry-migration case: two
	// manifests that share a behaviorDigest are the same agent and must both be
	// able to exist, so the content digest disambiguates them.
	if m.Name == digest.DisambiguatedName(m.Spec.Agent, behavior, content) {
		return nil
	}
	return field.ErrorList{field.Invalid(p, m.Name, fmt.Sprintf(
		"name must be derived from spec.agent and behaviorDigest.\n  expected %s\n"+
			"  or       %s   (only when %s already exists with different content)\n"+
			"Run: waveoff verify --write <file>",
		canonical, digest.DisambiguatedName(m.Spec.Agent, behavior, content), canonical))}
}

// validateEvidenceAnnotations freezes the release record carried in metadata.
// contentDigest covers the spec and not metadata, so without this an approver
// or ticket reference could be rewritten after the fact.
func validateEvidenceAnnotations(old, new *v1alpha1.AgentManifest) field.ErrorList {
	var errs field.ErrorList
	p := field.NewPath("metadata", "annotations")
	for k, oldVal := range old.Annotations {
		if !strings.HasPrefix(k, EvidencePrefix) {
			continue
		}
		newVal, present := new.Annotations[k]
		switch {
		case !present:
			errs = append(errs, field.Forbidden(p.Key(k),
				"evidence annotations are part of the release record and cannot be removed"))
		case newVal != oldVal:
			errs = append(errs, field.Forbidden(p.Key(k), fmt.Sprintf(
				"evidence annotations are part of the release record and cannot be changed "+
					"(was %q, now %q). Add a new key instead.", oldVal, newVal)))
		}
	}
	return errs
}

func names[T any](in []T, key func(T) string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		out = append(out, key(v))
	}
	return out
}
