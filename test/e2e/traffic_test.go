// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/waveoffai/waveoff/internal/traffic"
)

// The traffic routers are covered by unit tests against a fake client. What a
// fake client cannot reproduce is the thing that actually decides whether a
// live canary can run: a real API server applying a real CRD's structural
// schema. Fields outside that schema are pruned silently — the write succeeds,
// the read comes back without them — and a router that reads its own writes
// through a fake client will never see it.
//
// One of these cases exists because that pruning bit us: Gateway API's
// sessionPersistence lives in the experimental channel only, and on a
// standard-channel install the field a live stage depends on disappears
// between the write and the read.

func trafficTarget() traffic.Target {
	return traffic.Target{
		Namespace: namespace, Name: "canary-route",
		Incumbent: "agent-incumbent", Candidate: "agent-candidate",
	}
}

// gatewayAPIChannel reports which Gateway API channel the cluster has
// installed, or "" when the CRDs are absent.
func gatewayAPIChannel(t *testing.T) string {
	t.Helper()
	crd := &apiextensionsv1.CustomResourceDefinition{}
	err := k8s.Get(ctx(t), types.NamespacedName{Name: "httproutes.gateway.networking.k8s.io"}, crd)
	if err != nil {
		return ""
	}
	return crd.Annotations["gateway.networking.k8s.io/channel"]
}

func requireGatewayAPI(t *testing.T) {
	t.Helper()
	if gatewayAPIChannel(t) == "" {
		t.Fatal("Gateway API CRDs are not installed. hack/e2e.sh installs them; if you are running " +
			"with USE_EXISTING=1, install them yourself rather than letting these cases pass vacuously")
	}
}

// httpRouteWithBackends creates a route carrying both arms as backends.
func httpRouteWithBackends(t *testing.T, name string, mutate func(*gatewayv1.HTTPRoute)) *gatewayv1.HTTPRoute {
	t.Helper()
	port := gatewayv1.PortNumber(8080)
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: gatewayv1.HTTPRouteSpec{
			Rules: []gatewayv1.HTTPRouteRule{{
				BackendRefs: []gatewayv1.HTTPBackendRef{
					{BackendRef: gatewayv1.BackendRef{
						BackendObjectReference: gatewayv1.BackendObjectReference{
							Name: gatewayv1.ObjectName("agent-incumbent"), Port: &port,
						},
						Weight: ptr.To(int32(100)),
					}},
					{BackendRef: gatewayv1.BackendRef{
						BackendObjectReference: gatewayv1.BackendObjectReference{
							Name: gatewayv1.ObjectName("agent-candidate"), Port: &port,
						},
						Weight: ptr.To(int32(0)),
					}},
				},
			}},
		},
	}
	if mutate != nil {
		mutate(route)
	}
	if err := k8s.Create(ctx(t), route); err != nil {
		t.Fatalf("creating HTTPRoute %s: %v", name, err)
	}
	t.Cleanup(func() { _ = k8s.Delete(context.Background(), route) })
	return route
}

// TestGatewayAPIWeightsSurviveARealAPIServer.
//
// Not "our reader agrees with our writer" — the unit tests already say that,
// and they would keep saying it if every weight were pruned on the way in. This
// re-reads the stored object independently and checks the numbers landed on the
// backends they were meant for.
func TestGatewayAPIWeightsSurviveARealAPIServer(t *testing.T) {
	requireGatewayAPI(t)
	httpRouteWithBackends(t, "canary-route", nil)

	router := &traffic.GatewayAPI{Client: k8s}
	if err := router.SetSplit(ctx(t), trafficTarget(), traffic.Split{Incumbent: 90, Candidate: 10}); err != nil {
		t.Fatal(err)
	}

	stored := &gatewayv1.HTTPRoute{}
	if err := k8s.Get(ctx(t), types.NamespacedName{Namespace: namespace, Name: "canary-route"}, stored); err != nil {
		t.Fatal(err)
	}
	got := map[string]int32{}
	for _, ref := range stored.Spec.Rules[0].BackendRefs {
		if ref.Weight == nil {
			t.Fatalf("backend %q came back with no weight; the field was pruned", ref.Name)
		}
		got[string(ref.Name)] = *ref.Weight
	}
	if got["agent-incumbent"] != 90 || got["agent-candidate"] != 10 {
		t.Errorf("weights in the cluster are %v, want incumbent 90 / candidate 10", got)
	}

	// And the router reads back what the cluster holds rather than what it
	// last asked for, which is what stops a controller reporting a candidate
	// withdrawn while it is still serving.
	split, err := router.Split(ctx(t), trafficTarget())
	if err != nil {
		t.Fatal(err)
	}
	if split.Incumbent != 90 || split.Candidate != 10 {
		t.Errorf("Split() = %+v", split)
	}
}

// TestSessionPersistenceNeedsTheExperimentalChannel.
//
// This is the case that justifies the whole file. `sessionPersistence` is not
// in Gateway API's standard channel, so on a standard-channel install the API
// server prunes it: the apply succeeds, kubectl reports no error, and the field
// is simply gone. A live stage then holds forever, telling the operator to
// configure session affinity they already did configure.
//
// The assertion is conditional on the installed channel on purpose. Either
// answer is correct behaviour; what would not be correct is the router
// reporting sticky on a cluster that cannot store the field.
func TestSessionPersistenceNeedsTheExperimentalChannel(t *testing.T) {
	requireGatewayAPI(t)
	channel := gatewayAPIChannel(t)

	httpRouteWithBackends(t, "sticky-route", func(r *gatewayv1.HTTPRoute) {
		r.Spec.Rules[0].SessionPersistence = &gatewayv1.SessionPersistence{
			Type:        ptr.To(gatewayv1.HeaderBasedSessionPersistence),
			SessionName: ptr.To(traffic.SessionHeader),
		}
	})

	stored := &gatewayv1.HTTPRoute{}
	if err := k8s.Get(ctx(t), types.NamespacedName{Namespace: namespace, Name: "sticky-route"}, stored); err != nil {
		t.Fatal(err)
	}
	persisted := stored.Spec.Rules[0].SessionPersistence != nil

	router := &traffic.GatewayAPI{Client: k8s}
	target := trafficTarget()
	target.Name = "sticky-route"
	got, err := router.Stickiness(ctx(t), target)
	if err != nil {
		t.Fatal(err)
	}

	switch channel {
	case "experimental":
		if !persisted {
			t.Fatal("the experimental channel dropped sessionPersistence, which it must not")
		}
		if got != traffic.StickyBySession {
			t.Errorf("Stickiness = %q on a route configured for it", got)
		}
	default:
		if persisted {
			t.Fatalf("channel %q stored sessionPersistence; this test's premise is wrong", channel)
		}
		if got != traffic.StickyBySession {
			// Correct: nothing was stored, so nothing can be read.
			t.Logf("channel %q prunes sessionPersistence, so a live stage cannot run here", channel)
		} else {
			t.Error("reported sticky on a cluster that cannot store the field")
		}
	}
}

// TestAPlainWeightedRouteIsNotSticky.
//
// The route most people will write. Weights are evaluated per request, so a
// multi-turn session lands on both arms across its turns — and the controller
// holds the stage rather than measuring something that attributes one arm's
// work to the other.
func TestAPlainWeightedRouteIsNotSticky(t *testing.T) {
	requireGatewayAPI(t)
	httpRouteWithBackends(t, "plain-route", nil)

	router := &traffic.GatewayAPI{Client: k8s}
	target := trafficTarget()
	target.Name = "plain-route"

	got, err := router.Stickiness(ctx(t), target)
	if err != nil {
		t.Fatal(err)
	}
	if got != traffic.StickyNone {
		t.Errorf("Stickiness = %q on a route with weights and nothing else", got)
	}
}

// TestGatewayAPIMirrorSurvivesARealAPIServer.
//
// A shadow stage is a RequestMirror filter, and a filter is a discriminated
// union the API server validates: get the discriminator wrong and the write is
// rejected rather than pruned. Only a real API server says so.
func TestGatewayAPIMirrorSurvivesARealAPIServer(t *testing.T) {
	requireGatewayAPI(t)
	httpRouteWithBackends(t, "mirror-route", nil)

	router := &traffic.GatewayAPI{Client: k8s}
	target := trafficTarget()
	target.Name = "mirror-route"
	if err := router.Mirror(ctx(t), target, 100); err != nil {
		t.Fatal(err)
	}

	stored := &gatewayv1.HTTPRoute{}
	if err := k8s.Get(ctx(t), types.NamespacedName{Namespace: namespace, Name: "mirror-route"}, stored); err != nil {
		t.Fatal(err)
	}
	var mirrored string
	for _, f := range stored.Spec.Rules[0].Filters {
		if f.Type == gatewayv1.HTTPRouteFilterRequestMirror && f.RequestMirror != nil {
			mirrored = string(f.RequestMirror.BackendRef.Name)
		}
	}
	if mirrored != "agent-candidate" {
		t.Errorf("no RequestMirror to the candidate survived; filters are %+v", stored.Spec.Rules[0].Filters)
	}

	// Withdrawing has to remove it again. A mirror left behind keeps sending
	// production traffic to a candidate nobody is watching any more.
	if err := router.Mirror(ctx(t), target, 0); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Get(ctx(t), types.NamespacedName{Namespace: namespace, Name: "mirror-route"}, stored); err != nil {
		t.Fatal(err)
	}
	for _, f := range stored.Spec.Rules[0].Filters {
		if f.Type == gatewayv1.HTTPRouteFilterRequestMirror {
			t.Error("the mirror survived withdrawal, so a withdrawn candidate is still receiving traffic")
		}
	}
}

var virtualServiceGVK = schema.GroupVersionKind{
	Group: "networking.istio.io", Version: "v1beta1", Kind: "VirtualService",
}

func requireIstioCRDs(t *testing.T) {
	t.Helper()
	crd := &apiextensionsv1.CustomResourceDefinition{}
	err := k8s.Get(ctx(t), types.NamespacedName{Name: "virtualservices.networking.istio.io"}, crd)
	if err != nil {
		t.Fatal("Istio CRDs are not installed. hack/e2e.sh installs them; if you are running with " +
			"USE_EXISTING=1, install them yourself rather than letting this case pass vacuously")
	}
}

// TestIstioSplitSurvivesARealCRD.
//
// The Istio router works through unstructured objects rather than Istio's own
// Go types, to keep a very large dependency tree out of a module whose job is
// to stay easy to run. The cost of that choice is that nothing checks the field
// names at compile time — a typo is a silently pruned field, not a build
// failure — so it has to be checked against the real CRD.
func TestIstioSplitSurvivesARealCRD(t *testing.T) {
	requireIstioCRDs(t)

	vs := &unstructured.Unstructured{}
	vs.SetGroupVersionKind(virtualServiceGVK)
	vs.SetName("canary-vs")
	vs.SetNamespace(namespace)
	if err := unstructured.SetNestedMap(vs.Object, map[string]any{
		"hosts": []any{"agent.internal"},
		"http": []any{map[string]any{
			"route": []any{
				map[string]any{
					"destination": map[string]any{"host": "agent-incumbent"},
					"weight":      int64(100),
				},
				map[string]any{
					"destination": map[string]any{"host": "agent-candidate"},
					"weight":      int64(0),
				},
			},
		}},
	}, "spec"); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Create(ctx(t), vs); err != nil {
		t.Fatalf("creating the VirtualService: %v", err)
	}
	t.Cleanup(func() { _ = k8s.Delete(context.Background(), vs) })

	router := &traffic.Istio{Client: k8s}
	target := trafficTarget()
	target.Name = "canary-vs"
	if err := router.SetSplit(ctx(t), target, traffic.Split{Incumbent: 80, Candidate: 20}); err != nil {
		t.Fatal(err)
	}

	// Re-read independently and walk the object by hand, so a pruned weight
	// cannot hide behind the same helper that wrote it.
	stored := &unstructured.Unstructured{}
	stored.SetGroupVersionKind(virtualServiceGVK)
	if err := k8s.Get(ctx(t), types.NamespacedName{Namespace: namespace, Name: "canary-vs"}, stored); err != nil {
		t.Fatal(err)
	}
	routes, found, err := unstructured.NestedSlice(stored.Object, "spec", "http")
	if err != nil || !found {
		t.Fatalf("spec.http did not survive: %v", err)
	}
	dests, _, _ := unstructured.NestedSlice(routes[0].(map[string]any), "route")
	got := map[string]int64{}
	for _, d := range dests {
		m := d.(map[string]any)
		host, _, _ := unstructured.NestedString(m, "destination", "host")
		w, ok, _ := unstructured.NestedInt64(m, "weight")
		if !ok {
			t.Errorf("destination %q came back with no weight; the field was pruned", host)
		}
		got[host] = w
	}
	if got["agent-incumbent"] != 80 || got["agent-candidate"] != 20 {
		t.Errorf("weights in the cluster are %v, want incumbent 80 / candidate 20", got)
	}
}

// TestIstioStickinessIsReadFromTheStoredObject.
//
// Istio configures a consistent hash on a DestinationRule, which this router
// does not own. It looks for the operator's explicit assertion on the
// VirtualService instead — and an annotation is the one thing a CRD cannot
// prune, which is part of why the assertion lives there.
func TestIstioStickinessIsReadFromTheStoredObject(t *testing.T) {
	requireIstioCRDs(t)

	vs := &unstructured.Unstructured{}
	vs.SetGroupVersionKind(virtualServiceGVK)
	vs.SetName("sticky-vs")
	vs.SetNamespace(namespace)
	vs.SetAnnotations(map[string]string{"waveoff.ai/session-affinity": traffic.SessionHeader})
	if err := unstructured.SetNestedMap(vs.Object, map[string]any{
		"hosts": []any{"agent.internal"},
		"http": []any{map[string]any{
			"route": []any{map[string]any{
				"destination": map[string]any{"host": "agent-incumbent"},
				"weight":      int64(100),
			}},
		}},
	}, "spec"); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Create(ctx(t), vs); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8s.Delete(context.Background(), vs) })

	router := &traffic.Istio{Client: k8s}
	target := trafficTarget()
	target.Name = "sticky-vs"
	got, err := router.Stickiness(ctx(t), target)
	if err != nil {
		t.Fatal(err)
	}
	if got != traffic.StickyBySession {
		t.Errorf("Stickiness = %q on an annotated VirtualService", got)
	}
}

// TestAnUnknownDestinationIsRefusedByARealCluster.
//
// A rollout reweights destinations that are already there rather than adding
// them: inventing a subset or host the operator never configured is how a
// canary sends traffic somewhere nobody meant it to go.
func TestAnUnknownDestinationIsRefusedByARealCluster(t *testing.T) {
	requireIstioCRDs(t)

	vs := &unstructured.Unstructured{}
	vs.SetGroupVersionKind(virtualServiceGVK)
	vs.SetName("partial-vs")
	vs.SetNamespace(namespace)
	if err := unstructured.SetNestedMap(vs.Object, map[string]any{
		"hosts": []any{"agent.internal"},
		"http": []any{map[string]any{
			"route": []any{map[string]any{
				"destination": map[string]any{"host": "agent-incumbent"},
				"weight":      int64(100),
			}},
		}},
	}, "spec"); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Create(ctx(t), vs); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8s.Delete(context.Background(), vs) })

	router := &traffic.Istio{Client: k8s}
	target := trafficTarget()
	target.Name = "partial-vs"
	err := router.SetSplit(ctx(t), target, traffic.Split{Incumbent: 90, Candidate: 10})
	if err == nil {
		t.Fatal("a split was written to a VirtualService with no candidate destination")
	}
	if !strings.Contains(err.Error(), "agent-candidate") {
		t.Errorf("the refusal does not name the missing destination: %v", err)
	}
}
