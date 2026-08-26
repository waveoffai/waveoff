// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/waveoffai/waveoff/api/v1alpha1"
)

// TestSchemeCoversEverythingTheCLIReads guards a failure that is invisible to
// any test using a hand-built fake scheme, and total in production: a client
// built from a scheme missing a kind fails at the first Get with "no kind is
// registered". `waveoff pin` reads Deployments, Pods and ConfigMaps, so all of
// them have to be here.
func TestSchemeCoversEverythingTheCLIReads(t *testing.T) {
	for _, obj := range []runtime.Object{
		&v1alpha1.AgentManifest{},
		&v1alpha1.AgentManifestList{},
		&appsv1.Deployment{},
		&corev1.Pod{},
		&corev1.PodList{},
		&corev1.ConfigMap{},
		&corev1.Secret{},
		&corev1.Namespace{},
	} {
		gvks, _, err := Scheme.ObjectKinds(obj)
		if err != nil {
			t.Errorf("%T is not registered in manifest.Scheme: %v", obj, err)
			continue
		}
		if len(gvks) == 0 {
			t.Errorf("%T resolved to no GroupVersionKind", obj)
		}
	}
}
