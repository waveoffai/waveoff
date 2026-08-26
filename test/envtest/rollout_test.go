// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package envtest

import (
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/waveoffai/waveoff/api/v1alpha1"
)

func rolloutFor(name string, mutate func(*v1alpha1.AgentRollout)) *v1alpha1.AgentRollout {
	margin := -0.02
	alpha := 0.05
	r := &v1alpha1.AgentRollout{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "AgentRollout"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: v1alpha1.AgentRolloutSpec{
			IncumbentRef: "support-agent-aaaaaaaaaaaa",
			CandidateRef: "support-agent-bbbbbbbbbbbb",
			Stages: []v1alpha1.Stage{{
				Name: "replay", Mode: v1alpha1.StageOfflineReplay,
				Corpus: v1alpha1.CorpusSelector{Ref: "/corpus"},
				Scorer: v1alpha1.ScorerSpec{
					Exec: &v1alpha1.ExecScorerSpec{Command: "python", Args: []string{"score.py"}},
				},
				Gate: v1alpha1.Gate{
					Primary: v1alpha1.GateMetric{
						Metric: "task-completion", Test: v1alpha1.GatePairedBootstrap,
						Margin: &margin, Alpha: &alpha, Direction: v1alpha1.HigherIsBetter,
					},
				},
			}},
		},
	}
	if mutate != nil {
		mutate(r)
	}
	return r
}

func TestRolloutAccepted(t *testing.T) {
	if err := newClient(t).Create(ctx(t), rolloutFor("accepted", nil)); err != nil {
		t.Fatalf("a well-formed rollout was rejected: %v", err)
	}
}

// TestComparingAManifestWithItselfIsRejected: a rollout where both arms are the
// same manifest measures nothing, and would report a confident promotion for a
// change that does not exist.
func TestComparingAManifestWithItselfIsRejected(t *testing.T) {
	r := rolloutFor("self-compare", func(r *v1alpha1.AgentRollout) {
		r.Spec.CandidateRef = r.Spec.IncumbentRef
	})
	err := newClient(t).Create(ctx(t), r)
	if err == nil {
		t.Fatal("a rollout comparing a manifest with itself was admitted")
	}
	if !strings.Contains(err.Error(), "nothing to compare") {
		t.Errorf("the rejection should say why: %v", err)
	}
}

// TestUnimplementedStageModeIsRejected: a mode that does not exist must be
// refused at admission rather than accepted and silently skipped, which would
// look like a stage that passed.
func TestUnimplementedStageModeIsRejected(t *testing.T) {
	r := rolloutFor("bad-mode", func(r *v1alpha1.AgentRollout) {
		r.Spec.Stages[0].Mode = "live"
	})
	if err := newClient(t).Create(ctx(t), r); err == nil {
		t.Fatal("a stage mode that is not implemented was admitted")
	}
}

func TestUnknownTestIsRejected(t *testing.T) {
	r := rolloutFor("bad-test", func(r *v1alpha1.AgentRollout) {
		r.Spec.Stages[0].Gate.Primary.Test = "t-test"
	})
	if err := newClient(t).Create(ctx(t), r); err == nil {
		t.Fatal("an unimplemented statistical test was admitted")
	}
}

// TestARolloutNeedsAStage: an empty rollout would report success having
// measured nothing.
func TestARolloutNeedsAStage(t *testing.T) {
	r := rolloutFor("no-stages", func(r *v1alpha1.AgentRollout) {
		r.Spec.Stages = nil
	})
	err := newClient(t).Create(ctx(t), r)
	if err == nil {
		t.Fatal("a rollout with no stages was admitted")
	}
	if !apierrors.IsInvalid(err) {
		t.Errorf("expected Invalid, got %T: %v", err, err)
	}
}

// TestRolloutStatusIsASubresource: the controller writes status, and a spec
// update must not be able to smuggle a verdict in with it.
func TestRolloutStatusIsASubresource(t *testing.T) {
	c := newClient(t)
	r := rolloutFor("status-sub", nil)
	if err := c.Create(ctx(t), r); err != nil {
		t.Fatal(err)
	}

	r.Status.Phase = v1alpha1.PhasePromoted
	r.Status.Reason = "written through the main resource"
	if err := c.Update(ctx(t), r); err != nil {
		t.Fatal(err)
	}

	got := &v1alpha1.AgentRollout{}
	if err := c.Get(ctx(t), key2("default", "status-sub"), got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase == v1alpha1.PhasePromoted {
		t.Error("status was written through the main resource; it should be a subresource")
	}

	// And through the subresource it sticks.
	got.Status.Phase = v1alpha1.PhaseWavedOff
	got.Status.Reason = "task-completion is worse than the margin allows"
	if err := c.Status().Update(ctx(t), got); err != nil {
		t.Fatal(err)
	}
	final := &v1alpha1.AgentRollout{}
	if err := c.Get(ctx(t), key2("default", "status-sub"), final); err != nil {
		t.Fatal(err)
	}
	if final.Status.Phase != v1alpha1.PhaseWavedOff {
		t.Errorf("phase = %q", final.Status.Phase)
	}
}
