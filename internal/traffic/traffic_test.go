// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package traffic_test

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/waveoffai/waveoff/internal/traffic"
)

func target() traffic.Target {
	return traffic.Target{
		Namespace: "prod", Name: "support-agent",
		Incumbent: "support-agent-incumbent", Candidate: "support-agent-candidate",
	}
}

func httpRoute() *gatewayv1.HTTPRoute {
	port := gatewayv1.PortNumber(8080)
	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "support-agent", Namespace: "prod"},
		Spec: gatewayv1.HTTPRouteSpec{
			Rules: []gatewayv1.HTTPRouteRule{{
				BackendRefs: []gatewayv1.HTTPBackendRef{
					{BackendRef: gatewayv1.BackendRef{
						BackendObjectReference: gatewayv1.BackendObjectReference{
							Name: "support-agent-incumbent", Port: &port,
						},
						Weight: ptr.To(int32(100)),
					}},
					{BackendRef: gatewayv1.BackendRef{
						BackendObjectReference: gatewayv1.BackendObjectReference{
							Name: "support-agent-candidate", Port: &port,
						},
						Weight: ptr.To(int32(0)),
					}},
				},
			}},
		},
	}
}

func gatewayRouter(t *testing.T, objs ...runtime.Object) (*traffic.GatewayAPI, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := gatewayv1.Install(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
	return &traffic.GatewayAPI{Client: c}, c
}

func TestGatewayAPISplitRoundTrip(t *testing.T) {
	r, _ := gatewayRouter(t, httpRoute())
	ctx := context.Background()

	if err := r.SetSplit(ctx, target(), traffic.Split{Incumbent: 95, Candidate: 5}); err != nil {
		t.Fatal(err)
	}
	got, err := r.Split(ctx, target())
	if err != nil {
		t.Fatal(err)
	}
	if got.Incumbent != 95 || got.Candidate != 5 {
		t.Errorf("split = %+v", got)
	}
}

// TestGatewayAPIPreservesBackendConfiguration: a rollout reweights, it does not
// reconfigure. Replacing the refs wholesale would drop the port, and routing
// would break for reasons nothing in the rollout explains.
func TestGatewayAPIPreservesBackendConfiguration(t *testing.T) {
	r, c := gatewayRouter(t, httpRoute())
	ctx := context.Background()

	if err := r.SetSplit(ctx, target(), traffic.Split{Incumbent: 50, Candidate: 50}); err != nil {
		t.Fatal(err)
	}

	var route gatewayv1.HTTPRoute
	if err := c.Get(ctx, client.ObjectKey{Namespace: "prod", Name: "support-agent"}, &route); err != nil {
		t.Fatal(err)
	}
	for _, ref := range route.Spec.Rules[0].BackendRefs {
		if ref.Port == nil {
			t.Errorf("backend %s lost its port", ref.Name)
		}
	}
}

// TestBothWeightsZeroIsRefused: that is not "no traffic to the candidate", it
// is no traffic at all — an outage produced by a rollout controller.
func TestBothWeightsZeroIsRefused(t *testing.T) {
	r, _ := gatewayRouter(t, httpRoute())
	err := r.SetSplit(context.Background(), target(), traffic.Split{})
	if err == nil {
		t.Fatal("a split routing nothing anywhere was accepted")
	}
	if !strings.Contains(err.Error(), "no traffic") {
		t.Errorf("err = %v", err)
	}
}

// TestUnknownBackendIsRefused: a rollout reweights backends that are already
// there rather than inventing them.
func TestUnknownBackendIsRefused(t *testing.T) {
	r, _ := gatewayRouter(t, httpRoute())
	tgt := target()
	tgt.Candidate = "never-configured"
	if err := r.SetSplit(context.Background(), tgt, traffic.Split{Incumbent: 90, Candidate: 10}); err == nil {
		t.Fatal("a rollout added a backend the route never had")
	}
}

func TestGatewayAPIMirror(t *testing.T) {
	r, c := gatewayRouter(t, httpRoute())
	ctx := context.Background()

	if err := r.Mirror(ctx, target(), 100); err != nil {
		t.Fatal(err)
	}
	var route gatewayv1.HTTPRoute
	if err := c.Get(ctx, client.ObjectKey{Namespace: "prod", Name: "support-agent"}, &route); err != nil {
		t.Fatal(err)
	}
	var mirrors int
	for _, f := range route.Spec.Rules[0].Filters {
		if f.Type == gatewayv1.HTTPRouteFilterRequestMirror {
			mirrors++
		}
	}
	if mirrors != 1 {
		t.Fatalf("mirror filters = %d, want 1", mirrors)
	}

	// Turning it off must remove it, or a shadow stage leaves production
	// permanently duplicating traffic to a candidate nobody is watching.
	if err := r.Mirror(ctx, target(), 0); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "prod", Name: "support-agent"}, &route); err != nil {
		t.Fatal(err)
	}
	for _, f := range route.Spec.Rules[0].Filters {
		if f.Type == gatewayv1.HTTPRouteFilterRequestMirror {
			t.Error("the mirror filter survived being disabled")
		}
	}
}

// TestGatewayAPIRefusesPartialMirror rather than rounding it up.
//
// Gateway API mirrors a whole rule or none of it. Silently treating a request
// for 10% as 100% would multiply production load by a factor nobody asked for,
// caused by a feature meant only to observe.
// TestAMirrorCarriesTheCandidatePort.
//
// Gateway API requires a port on a Service reference and the API server rejects
// a mirror without one. That rule lives in CEL on the CRD, so the fake client
// this file uses will never enforce it — this test exists because the e2e suite
// caught a mirror that was written without a port and rejected in a real
// cluster, and something in the fast suite has to hold the line.
func TestAMirrorCarriesTheCandidatePort(t *testing.T) {
	r, c := gatewayRouter(t, httpRoute())
	ctx := context.Background()

	if err := r.Mirror(ctx, target(), 100); err != nil {
		t.Fatal(err)
	}
	var route gatewayv1.HTTPRoute
	if err := c.Get(ctx, client.ObjectKey{Namespace: "prod", Name: "support-agent"}, &route); err != nil {
		t.Fatal(err)
	}
	for _, f := range route.Spec.Rules[0].Filters {
		if f.Type != gatewayv1.HTTPRouteFilterRequestMirror || f.RequestMirror == nil {
			continue
		}
		if f.RequestMirror.BackendRef.Port == nil {
			t.Fatal("the mirror names no port, which a real API server rejects outright")
		}
		if *f.RequestMirror.BackendRef.Port != gatewayv1.PortNumber(8080) {
			t.Errorf("mirror port = %d, want the port the rule already routes to", *f.RequestMirror.BackendRef.Port)
		}
		return
	}
	t.Fatal("no mirror filter was written")
}

// TestAMirrorToABackendTheRouteDoesNotHaveIsRefused.
//
// A shadow stage mirrors to a backend that is already there. Inventing one
// means guessing a port, and a mirror to a port the candidate is not listening
// on measures a candidate that never answered rather than one that did badly.
func TestAMirrorToABackendTheRouteDoesNotHaveIsRefused(t *testing.T) {
	route := httpRoute()
	route.Spec.Rules[0].BackendRefs = route.Spec.Rules[0].BackendRefs[:1]
	r, _ := gatewayRouter(t, route)

	err := r.Mirror(context.Background(), target(), 100)
	if err == nil {
		t.Fatal("mirrored to a backend the route does not have")
	}
	if !strings.Contains(err.Error(), "support-agent-candidate") {
		t.Errorf("the refusal does not name the missing backend: %v", err)
	}
}

func TestGatewayAPIRefusesPartialMirror(t *testing.T) {
	r, _ := gatewayRouter(t, httpRoute())
	err := r.Mirror(context.Background(), target(), 10)
	if err == nil {
		t.Fatal("a partial mirror was silently accepted")
	}
	if !strings.Contains(err.Error(), "0 or 100") {
		t.Errorf("the refusal should say what is possible: %v", err)
	}
}

// --- Istio ---

func virtualService() *unstructured.Unstructured {
	vs := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "networking.istio.io/v1beta1",
		"kind":       "VirtualService",
		"metadata":   map[string]any{"name": "support-agent", "namespace": "prod"},
		"spec": map[string]any{
			"hosts": []any{"support-agent"},
			"http": []any{map[string]any{
				"route": []any{
					map[string]any{
						"destination": map[string]any{"host": "support-agent", "subset": "support-agent-incumbent"},
						"weight":      int64(100),
					},
					map[string]any{
						"destination": map[string]any{"host": "support-agent", "subset": "support-agent-candidate"},
						"weight":      int64(0),
					},
				},
			}},
		},
	}}
	return vs
}

func istioRouter(t *testing.T, objs ...runtime.Object) (*traffic.Istio, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
	return &traffic.Istio{Client: c}, c
}

func TestIstioSplitRoundTrip(t *testing.T) {
	r, _ := istioRouter(t, virtualService())
	ctx := context.Background()

	if err := r.SetSplit(ctx, target(), traffic.Split{Incumbent: 90, Candidate: 10}); err != nil {
		t.Fatal(err)
	}
	got, err := r.Split(ctx, target())
	if err != nil {
		t.Fatal(err)
	}
	if got.Incumbent != 90 || got.Candidate != 10 {
		t.Errorf("split = %+v", got)
	}
}

// TestIstioSupportsPartialMirror, which Gateway API does not — so a shadow
// stage can run at a fraction of production load here.
func TestIstioSupportsPartialMirror(t *testing.T) {
	r, c := istioRouter(t, virtualService())
	ctx := context.Background()

	if err := r.Mirror(ctx, target(), 25); err != nil {
		t.Fatal(err)
	}
	vs := &unstructured.Unstructured{}
	vs.SetGroupVersionKind(virtualService().GroupVersionKind())
	if err := c.Get(ctx, client.ObjectKey{Namespace: "prod", Name: "support-agent"}, vs); err != nil {
		t.Fatal(err)
	}
	route := vs.Object["spec"].(map[string]any)["http"].([]any)[0].(map[string]any)
	if route["mirror"] == nil {
		t.Fatal("no mirror was configured")
	}
	// Unstructured round-trips numbers as either float64 or int64 depending on
	// how they were written, so compare the value rather than the Go type.
	pct := numeric(t, route["mirrorPercentage"].(map[string]any)["value"])
	if pct != 25 {
		t.Errorf("mirrorPercentage = %v, want 25", pct)
	}

	if err := r.Mirror(ctx, target(), 0); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "prod", Name: "support-agent"}, vs); err != nil {
		t.Fatal(err)
	}
	route = vs.Object["spec"].(map[string]any)["http"].([]any)[0].(map[string]any)
	if route["mirror"] != nil {
		t.Error("the mirror survived being disabled")
	}
}

func TestIstioUnknownDestinationIsRefused(t *testing.T) {
	r, _ := istioRouter(t, virtualService())
	tgt := target()
	tgt.Candidate = "never-configured"
	if err := r.SetSplit(context.Background(), tgt, traffic.Split{Incumbent: 90, Candidate: 10}); err == nil {
		t.Fatal("a rollout added a destination the VirtualService never had")
	}
}

// TestBothRoutersHonourTheSameContract: the controller talks to the interface,
// so the two must not disagree about what a split means.
func TestBothRoutersHonourTheSameContract(t *testing.T) {
	gw, _ := gatewayRouter(t, httpRoute())
	istio, _ := istioRouter(t, virtualService())
	ctx := context.Background()

	for _, r := range []traffic.Router{gw, istio} {
		if err := r.SetSplit(ctx, target(), traffic.Split{Incumbent: 80, Candidate: 20}); err != nil {
			t.Fatalf("%s: %v", r.Name(), err)
		}
		got, err := r.Split(ctx, target())
		if err != nil {
			t.Fatalf("%s: %v", r.Name(), err)
		}
		if got.Incumbent != 80 || got.Candidate != 20 {
			t.Errorf("%s read back %+v", r.Name(), got)
		}
		// And a rollback returns everything to the incumbent.
		if err := r.SetSplit(ctx, target(), traffic.FullyIncumbent); err != nil {
			t.Fatalf("%s: %v", r.Name(), err)
		}
		got, _ = r.Split(ctx, target())
		if got.Candidate != 0 {
			t.Errorf("%s: a rollback left %d%% on the candidate", r.Name(), got.Candidate)
		}
	}
}

func numeric(t *testing.T, v any) float64 {
	t.Helper()
	switch n := v.(type) {
	case float64:
		return n
	case int64:
		return float64(n)
	case int:
		return float64(n)
	}
	t.Fatalf("%v is not a number (%T)", v, v)
	return 0
}

// TestWeightsAloneAreNotSticky.
//
// The default route carries weights and nothing else, which is what most people
// will write. Weights are evaluated per request, so a multi-turn session lands
// on both arms across its turns: the comparison then attributes to one arm work
// the other partly produced. Routers have to say so rather than let a live
// stage run on the assumption.
func TestWeightsAloneAreNotSticky(t *testing.T) {
	r, _ := gatewayRouter(t, httpRoute())

	got, err := r.Stickiness(context.Background(), target())
	if err != nil {
		t.Fatal(err)
	}
	if got != traffic.StickyNone {
		t.Errorf("a weighted route with no session persistence reported %q", got)
	}
}

func TestGatewayAPIStickinessOnTheSessionHeader(t *testing.T) {
	route := httpRoute()
	route.Spec.Rules[0].SessionPersistence = &gatewayv1.SessionPersistence{
		Type:        ptr.To(gatewayv1.HeaderBasedSessionPersistence),
		SessionName: ptr.To(traffic.SessionHeader),
	}
	r, _ := gatewayRouter(t, route)

	got, err := r.Stickiness(context.Background(), target())
	if err != nil {
		t.Fatal(err)
	}
	if got != traffic.StickyBySession {
		t.Errorf("Stickiness = %q, want %q", got, traffic.StickyBySession)
	}
}

// TestCookieStickinessIsNotSessionStickiness.
//
// Cookie persistence is the Gateway API default and it identifies a browser.
// There is no browser here: agent traffic arrives from a fleet of pods, so a
// cookie either does not exist or is shared by everything behind one client.
// Keeping "something" together is not the same as keeping a session together,
// and only one of those makes the comparison valid.
func TestCookieStickinessIsNotSessionStickiness(t *testing.T) {
	route := httpRoute()
	route.Spec.Rules[0].SessionPersistence = &gatewayv1.SessionPersistence{
		Type: ptr.To(gatewayv1.CookieBasedSessionPersistence),
	}
	r, _ := gatewayRouter(t, route)

	got, err := r.Stickiness(context.Background(), target())
	if err != nil {
		t.Fatal(err)
	}
	if got != traffic.StickyNone {
		t.Errorf("cookie persistence reported %q", got)
	}
}

// TestIstioStickinessIsAnAssertion.
//
// Istio configures a consistent hash on a DestinationRule, not on the
// VirtualService this router owns. Reading it would mean a second object and a
// second set of permissions, so the router looks for the operator's explicit
// assertion instead — and both this test and the annotation name are honest
// about it being an assertion.
func TestIstioStickinessIsAnAssertion(t *testing.T) {
	vs := virtualService()
	r, _ := istioRouter(t, vs)
	ctx := context.Background()

	got, err := r.Stickiness(ctx, target())
	if err != nil {
		t.Fatal(err)
	}
	if got != traffic.StickyNone {
		t.Errorf("an unannotated VirtualService reported %q", got)
	}

	vs.SetAnnotations(map[string]string{"waveoff.ai/session-affinity": traffic.SessionHeader})
	r, _ = istioRouter(t, vs)
	got, err = r.Stickiness(ctx, target())
	if err != nil {
		t.Fatal(err)
	}
	if got != traffic.StickyBySession {
		t.Errorf("Stickiness = %q, want %q", got, traffic.StickyBySession)
	}
}
