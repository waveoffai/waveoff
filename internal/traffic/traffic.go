// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

// Package traffic moves requests between an incumbent and a candidate.
//
// The controller talks to this interface and never to a mesh. That is a
// deliberate constraint: service meshes are the least portable part of a
// cluster, teams change theirs, and a rollout controller welded to one is a
// rollout controller most people cannot run. Everything the rollout logic needs
// is here — a weight, a mirror, and the ability to read back what is actually
// configured.
package traffic

import (
	"context"
	"fmt"
)

// Target identifies the route a rollout controls.
type Target struct {
	Namespace string
	// Name is the route object: an HTTPRoute, a VirtualService, whatever the
	// implementation understands.
	Name string
	// Incumbent and Candidate are the backend services traffic is split
	// between.
	Incumbent string
	Candidate string
}

// Split is how traffic is divided, in percent.
type Split struct {
	Incumbent int
	Candidate int
}

// Valid reports whether a split is usable.
func (s Split) Valid() error {
	if s.Incumbent < 0 || s.Candidate < 0 {
		return fmt.Errorf("negative weight: %d/%d", s.Incumbent, s.Candidate)
	}
	if s.Incumbent+s.Candidate == 0 {
		// Both at zero is not "no traffic to the candidate", it is no traffic
		// at all — an outage produced by a rollout controller.
		return fmt.Errorf("both weights are zero, which would route no traffic anywhere")
	}
	return nil
}

// Stickiness says whether a router keeps one session on one arm.
type Stickiness string

const (
	// StickyBySession: every request in a session reaches the same arm.
	StickyBySession Stickiness = "by-session"
	// StickyNone: routing is decided per request.
	StickyNone Stickiness = "none"
)

// Router controls how traffic reaches the two arms.
//
// Implementations must be idempotent: the controller reconciles, so it will set
// the same weights repeatedly and must not thrash the route object doing it.
type Router interface {
	// SetSplit routes the given percentages to each arm.
	SetSplit(ctx context.Context, target Target, split Split) error

	// Split reads back what is actually configured.
	//
	// Read-back matters more than it looks. A rollback is a weight flip, and a
	// controller that assumes its last write took effect will report a
	// candidate as withdrawn while it is still serving.
	Split(ctx context.Context, target Target) (Split, error)

	// Mirror sends a copy of traffic to the candidate without returning its
	// responses to the caller. Zero disables mirroring.
	//
	// This is what makes a shadow stage possible: the candidate sees real
	// production requests and nothing it produces reaches a user.
	Mirror(ctx context.Context, target Target, percent int) error

	// Stickiness reports whether one session stays on one arm.
	//
	// This is a hard requirement of a live canary rather than a deployment
	// nicety. Weights are evaluated per request, so a multi-turn agent session
	// routed by weight alone lands on both arms across its turns. That corrupts
	// the measurement — an observation attributed to one arm was partly
	// produced by the other — and breaks the agent outright if either arm holds
	// session state.
	//
	// It also has to key on the agent's own session identifier. Keying on a
	// cookie or a source address routes by whatever the network happens to
	// share, which for a fleet of agent pods behind one address is everything.
	Stickiness(ctx context.Context, target Target) (Stickiness, error)

	// Name identifies the implementation in logs and status.
	Name() string
}

// SessionHeader is the header a router must hash on to keep a session on one
// arm.
//
// The agent's own session identifier, which is the same thing the recorder
// derives a session from — so the unit the router keeps together is the unit
// the analysis counts.
//
// Anything correlated with tenant, region or time of day would confound the
// comparison, and no amount of correct interval arithmetic recovers from that:
// the two arms would be serving different populations.
const SessionHeader = "X-Waveoff-Session"

// FullyIncumbent is the split a rollback returns to.
var FullyIncumbent = Split{Incumbent: 100, Candidate: 0}

// ErrUnsupported reports that a router cannot do something the stage asked for.
//
// Returned rather than silently ignored. A shadow stage against a router that
// cannot mirror is not a shadow stage that passed; it is one that never ran,
// and the difference decides whether a candidate has been tested.
var ErrUnsupported = fmt.Errorf("unsupported by this traffic router")
