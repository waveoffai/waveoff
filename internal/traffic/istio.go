// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package traffic

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// istioVirtualService is the type this router manipulates.
var istioVirtualService = schema.GroupVersionKind{
	Group: "networking.istio.io", Version: "v1beta1", Kind: "VirtualService",
}

// Istio splits traffic with a VirtualService.
//
// The secondary implementation, for clusters that have not moved to Gateway API
// yet. It works through unstructured objects rather than Istio's own Go types
// on purpose: importing istio.io/client-go would pull a very large dependency
// tree into a module whose whole job is to stay easy to run, and this needs
// four fields.
type Istio struct {
	Client client.Client
}

var _ Router = (*Istio)(nil)

// Name implements Router.
func (i *Istio) Name() string { return "istio" }

// SetSplit writes weights onto the first HTTP route's destinations.
func (i *Istio) SetSplit(ctx context.Context, target Target, split Split) error {
	if err := split.Valid(); err != nil {
		return err
	}
	vs, err := i.virtualService(ctx, target)
	if err != nil {
		return err
	}

	routes, err := httpRoutes(vs)
	if err != nil {
		return err
	}
	destinations, err := destinationsOf(routes[0])
	if err != nil {
		return err
	}

	// Weights are set on destinations that already exist. Adding one would
	// mean inventing a subset or host the operator never configured.
	seen := map[string]bool{}
	for _, d := range destinations {
		name, err := destinationName(d)
		if err != nil {
			return err
		}
		switch name {
		case target.Incumbent:
			d["weight"] = int64(split.Incumbent)
			seen[name] = true
		case target.Candidate:
			d["weight"] = int64(split.Candidate)
			seen[name] = true
		}
	}
	for _, want := range []string{target.Incumbent, target.Candidate} {
		if !seen[want] {
			return fmt.Errorf("VirtualService %s/%s has no destination for %q; a rollout reweights "+
				"destinations that are already there rather than adding them",
				target.Namespace, target.Name, want)
		}
	}

	return i.Client.Update(ctx, vs)
}

// Split reads back the configured weights.
func (i *Istio) Split(ctx context.Context, target Target) (Split, error) {
	vs, err := i.virtualService(ctx, target)
	if err != nil {
		return Split{}, err
	}
	routes, err := httpRoutes(vs)
	if err != nil {
		return Split{}, err
	}
	destinations, err := destinationsOf(routes[0])
	if err != nil {
		return Split{}, err
	}

	var split Split
	for _, d := range destinations {
		name, err := destinationName(d)
		if err != nil {
			return Split{}, err
		}
		weight, _ := d["weight"].(int64)
		switch name {
		case target.Incumbent:
			split.Incumbent = int(weight)
		case target.Candidate:
			split.Candidate = int(weight)
		}
	}
	return split, nil
}

// Mirror sends a percentage of traffic to the candidate as a copy.
//
// Istio does support a percentage here, which Gateway API does not — so a
// shadow stage can be run at a fraction of production load on Istio and only at
// all-or-nothing elsewhere.
func (i *Istio) Mirror(ctx context.Context, target Target, percent int) error {
	if percent < 0 || percent > 100 {
		return fmt.Errorf("mirror percentage %d is out of range", percent)
	}
	vs, err := i.virtualService(ctx, target)
	if err != nil {
		return err
	}
	routes, err := httpRoutes(vs)
	if err != nil {
		return err
	}
	route, ok := routes[0].(map[string]any)
	if !ok {
		return fmt.Errorf("VirtualService %s/%s: malformed http route", target.Namespace, target.Name)
	}

	if percent == 0 {
		delete(route, "mirror")
		delete(route, "mirrorPercentage")
	} else {
		route["mirror"] = map[string]any{"host": target.Candidate}
		route["mirrorPercentage"] = map[string]any{"value": float64(percent)}
	}
	return i.Client.Update(ctx, vs)
}

// Stickiness reports whether the VirtualService keeps a session on one arm.
//
// Istio expresses this as a consistent hash on a header, configured on a
// DestinationRule rather than the VirtualService. Reading it would mean a
// second object and a second set of permissions, so this looks for the
// operator's explicit assertion on the VirtualService instead — which is
// honest about the fact that it is an assertion rather than a verification.
func (i *Istio) Stickiness(ctx context.Context, target Target) (Stickiness, error) {
	vs, err := i.virtualService(ctx, target)
	if err != nil {
		return StickyNone, err
	}
	if vs.GetAnnotations()["waveoff.ai/session-affinity"] == SessionHeader {
		return StickyBySession, nil
	}
	return StickyNone, nil
}

func (i *Istio) virtualService(ctx context.Context, target Target) (*unstructured.Unstructured, error) {
	vs := &unstructured.Unstructured{}
	vs.SetGroupVersionKind(istioVirtualService)
	key := types.NamespacedName{Namespace: target.Namespace, Name: target.Name}
	if err := i.Client.Get(ctx, key, vs); err != nil {
		return nil, fmt.Errorf("VirtualService %s: %w", key, err)
	}
	return vs, nil
}

func httpRoutes(vs *unstructured.Unstructured) ([]any, error) {
	routes, found, err := unstructured.NestedSlice(vs.Object, "spec", "http")
	if err != nil {
		return nil, err
	}
	if !found || len(routes) == 0 {
		return nil, fmt.Errorf("VirtualService %s/%s has no http routes to weight",
			vs.GetNamespace(), vs.GetName())
	}
	// NestedSlice deep-copies, so the caller's edits would be lost. Reach for
	// the live value instead.
	spec, _ := vs.Object["spec"].(map[string]any)
	live, _ := spec["http"].([]any)
	if len(live) == 0 {
		return nil, fmt.Errorf("VirtualService %s/%s has no http routes", vs.GetNamespace(), vs.GetName())
	}
	return live, nil
}

func destinationsOf(route any) ([]map[string]any, error) {
	r, ok := route.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("malformed http route")
	}
	raw, ok := r["route"].([]any)
	if !ok || len(raw) == 0 {
		return nil, fmt.Errorf("http route has no destinations")
	}
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		d, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("malformed destination")
		}
		out = append(out, d)
	}
	return out, nil
}

// destinationName is the host or subset a destination points at, which is what
// a Target names.
func destinationName(d map[string]any) (string, error) {
	dest, ok := d["destination"].(map[string]any)
	if !ok {
		return "", fmt.Errorf("destination has no destination block")
	}
	if subset, ok := dest["subset"].(string); ok && subset != "" {
		return subset, nil
	}
	host, _ := dest["host"].(string)
	return host, nil
}
