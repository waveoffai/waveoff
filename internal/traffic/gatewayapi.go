// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package traffic

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"k8s.io/utils/ptr"
)

// GatewayAPI splits traffic with an HTTPRoute.
//
// The first-choice implementation: Gateway API is the standard the ecosystem is
// converging on, it is implemented by every mesh worth naming, and a cluster
// that has it needs nothing mesh-specific from us.
type GatewayAPI struct {
	Client client.Client
}

var _ Router = (*GatewayAPI)(nil)

// Name implements Router.
func (g *GatewayAPI) Name() string { return "gateway-api" }

// SetSplit writes backend weights onto the route's first rule.
func (g *GatewayAPI) SetSplit(ctx context.Context, target Target, split Split) error {
	if err := split.Valid(); err != nil {
		return err
	}
	route, err := g.route(ctx, target)
	if err != nil {
		return err
	}
	if len(route.Spec.Rules) == 0 {
		return fmt.Errorf("HTTPRoute %s/%s has no rules to weight", target.Namespace, target.Name)
	}

	rule := &route.Spec.Rules[0]
	refs, err := backendRefs(rule, target, split)
	if err != nil {
		return err
	}
	rule.BackendRefs = refs

	return g.Client.Update(ctx, route)
}

// backendRefs builds the weighted backends, preserving whatever the operator
// configured about each one beyond its weight.
//
// Replacing the refs wholesale would drop ports, filters and namespaces the
// route already had, so a rollout would silently reconfigure routing it was
// only supposed to reweight.
func backendRefs(rule *gatewayv1.HTTPRouteRule, target Target, split Split) ([]gatewayv1.HTTPBackendRef, error) {
	existing := map[string]gatewayv1.HTTPBackendRef{}
	for _, ref := range rule.BackendRefs {
		existing[string(ref.Name)] = ref
	}

	build := func(name string, weight int32) (gatewayv1.HTTPBackendRef, error) {
		ref, ok := existing[name]
		if !ok {
			return gatewayv1.HTTPBackendRef{}, fmt.Errorf(
				"HTTPRoute %s/%s does not route to %q; a rollout reweights backends that are "+
					"already there rather than adding them", target.Namespace, target.Name, name)
		}
		ref.Weight = ptr.To(weight)
		return ref, nil
	}

	incumbent, err := build(target.Incumbent, int32(split.Incumbent))
	if err != nil {
		return nil, err
	}
	candidate, err := build(target.Candidate, int32(split.Candidate))
	if err != nil {
		return nil, err
	}
	return []gatewayv1.HTTPBackendRef{incumbent, candidate}, nil
}

// Split reads back the configured weights.
func (g *GatewayAPI) Split(ctx context.Context, target Target) (Split, error) {
	route, err := g.route(ctx, target)
	if err != nil {
		return Split{}, err
	}
	if len(route.Spec.Rules) == 0 {
		return Split{}, fmt.Errorf("HTTPRoute %s/%s has no rules", target.Namespace, target.Name)
	}

	var split Split
	for _, ref := range route.Spec.Rules[0].BackendRefs {
		// An absent weight means 1 in Gateway API, not 0.
		weight := 1
		if ref.Weight != nil {
			weight = int(*ref.Weight)
		}
		switch string(ref.Name) {
		case target.Incumbent:
			split.Incumbent = weight
		case target.Candidate:
			split.Candidate = weight
		}
	}
	return split, nil
}

// Mirror sends a copy of traffic to the candidate.
//
// Gateway API's RequestMirror filter mirrors all traffic on a rule or none of
// it; there is no percentage. Rather than silently rounding a request for 10%
// up to everything, anything but 0 or 100 is refused — a shadow stage that
// mirrored ten times the traffic it was asked to would be a capacity incident
// caused by an observability feature.
func (g *GatewayAPI) Mirror(ctx context.Context, target Target, percent int) error {
	if percent != 0 && percent != 100 {
		return fmt.Errorf("%w: Gateway API mirrors all traffic on a rule or none, so %d%% cannot be "+
			"honoured; use 0 or 100", ErrUnsupported, percent)
	}
	route, err := g.route(ctx, target)
	if err != nil {
		return err
	}
	if len(route.Spec.Rules) == 0 {
		return fmt.Errorf("HTTPRoute %s/%s has no rules", target.Namespace, target.Name)
	}
	rule := &route.Spec.Rules[0]

	// Drop any mirror we previously added, leaving the operator's own filters
	// alone.
	var filters []gatewayv1.HTTPRouteFilter
	for _, f := range rule.Filters {
		if f.Type == gatewayv1.HTTPRouteFilterRequestMirror &&
			f.RequestMirror != nil &&
			string(f.RequestMirror.BackendRef.Name) == target.Candidate {
			continue
		}
		filters = append(filters, f)
	}

	if percent == 100 {
		// The port has to come along. Gateway API requires one on a Service
		// reference and the API server rejects a mirror without it — a CEL
		// rule on the CRD, so no fake client will ever tell you. Take it from
		// the candidate backend the operator already configured rather than
		// guessing a number.
		port, err := candidatePort(rule, target)
		if err != nil {
			return err
		}
		filters = append(filters, gatewayv1.HTTPRouteFilter{
			Type: gatewayv1.HTTPRouteFilterRequestMirror,
			RequestMirror: &gatewayv1.HTTPRequestMirrorFilter{
				BackendRef: gatewayv1.BackendObjectReference{
					Name: gatewayv1.ObjectName(target.Candidate),
					Port: port,
				},
			},
		})
	}
	rule.Filters = filters
	return g.Client.Update(ctx, route)
}

// candidatePort finds the port the candidate backend is already served on.
//
// A mirror to a different port than the one the rule routes to would send
// shadow traffic somewhere the candidate is not listening, and the stage would
// report a candidate that never answered rather than one that did badly.
func candidatePort(rule *gatewayv1.HTTPRouteRule, target Target) (*gatewayv1.PortNumber, error) {
	for _, ref := range rule.BackendRefs {
		if string(ref.Name) != target.Candidate {
			continue
		}
		if ref.Port == nil {
			return nil, fmt.Errorf("HTTPRoute %s/%s routes to %q without a port, so a mirror to it "+
				"cannot name one either", target.Namespace, target.Name, target.Candidate)
		}
		return ref.Port, nil
	}
	return nil, fmt.Errorf("HTTPRoute %s/%s has no backend for %q; a shadow stage mirrors to a "+
		"backend that is already there rather than adding one",
		target.Namespace, target.Name, target.Candidate)
}

// Stickiness reports whether the route keeps a session on one arm.
//
// Gateway API's weights alone do not: they are evaluated per request. The rule
// has to carry an explicit sessionPersistence configuration, and it has to key
// on the agent's session header rather than a cookie — a cookie identifies a
// browser, and there is no browser here.
func (g *GatewayAPI) Stickiness(ctx context.Context, target Target) (Stickiness, error) {
	route, err := g.route(ctx, target)
	if err != nil {
		return StickyNone, err
	}
	if len(route.Spec.Rules) == 0 {
		return StickyNone, nil
	}
	sp := route.Spec.Rules[0].SessionPersistence
	if sp == nil {
		return StickyNone, nil
	}
	if sp.Type != nil && *sp.Type == gatewayv1.HeaderBasedSessionPersistence {
		if sp.SessionName != nil && *sp.SessionName == SessionHeader {
			return StickyBySession, nil
		}
		// Header-based, but on some other header. That keeps *something*
		// together; whether it is a session is not ours to assume.
		return StickyNone, nil
	}
	return StickyNone, nil
}

func (g *GatewayAPI) route(ctx context.Context, target Target) (*gatewayv1.HTTPRoute, error) {
	var route gatewayv1.HTTPRoute
	key := types.NamespacedName{Namespace: target.Namespace, Name: target.Name}
	if err := g.Client.Get(ctx, key, &route); err != nil {
		return nil, fmt.Errorf("HTTPRoute %s: %w", key, err)
	}
	return &route, nil
}
