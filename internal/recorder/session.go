// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package recorder

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// SessionHeader lets an agent name its own session explicitly. It is the escape
// hatch for a runtime that does not propagate trace context.
const SessionHeader = "X-Waveoff-Session"

// Origin records how a session boundary was established, so the quality of a
// corpus is visible rather than assumed.
type Origin string

const (
	// OriginTrace means the boundary came from W3C trace context. This is the
	// good case: the agent's own instrumentation defines the session, so a
	// multi-step agent loop is recorded as one session.
	OriginTrace Origin = "traceparent"
	// OriginHeader means the agent named the session itself.
	OriginHeader Origin = "header"
	// OriginSynthetic means neither was present and each request became its own
	// session. Steps cannot be correlated across a session boundary that does
	// not exist, so a corpus recorded this way supports payload-level replay
	// but not divergence detection.
	OriginSynthetic Origin = "synthetic"
)

// Sessions tracks in-flight sessions and hands out step indices.
//
// Step ordering is part of the replay matching key, so a step counter that
// resets or races produces cassettes that cannot be matched against. The
// counter is per session and monotonic.
type Sessions struct {
	mu        sync.Mutex
	byID      map[string]*sessionState
	ttl       time.Duration
	now       func() time.Time
	synthetic atomic.Int64
}

type sessionState struct {
	step atomic.Int64
	seen time.Time
}

// NewSessions builds a registry. Entries idle for longer than ttl are dropped,
// because a sidecar runs for the life of a pod and an unbounded map keyed by
// trace ID is a slow memory leak.
func NewSessions(ttl time.Duration) *Sessions {
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	return &Sessions{byID: map[string]*sessionState{}, ttl: ttl, now: time.Now}
}

// Identify returns the session for a request, how it was determined, and the
// next step index within it.
func (s *Sessions) Identify(r *http.Request) (id string, origin Origin, step int) {
	id, origin = sessionID(r)
	if id == "" {
		id, origin = s.newSyntheticID(), OriginSynthetic
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictLocked()

	st, ok := s.byID[id]
	if !ok {
		st = &sessionState{}
		s.byID[id] = st
	}
	st.seen = s.now()
	return id, origin, int(st.step.Add(1) - 1)
}

// sessionID prefers the agent's own trace context. Deriving the session from
// the trace means a multi-step agent loop is recorded as one session with no
// code change, which is the whole reason the recorder sits in the pod rather
// than in a gateway.
func sessionID(r *http.Request) (string, Origin) {
	if v := strings.TrimSpace(r.Header.Get(SessionHeader)); v != "" {
		return v, OriginHeader
	}
	// traceparent is "version-traceid-spanid-flags"; the trace ID is the
	// session, since every call the agent makes within one invocation shares it.
	if tp := r.Header.Get("traceparent"); tp != "" {
		parts := strings.Split(tp, "-")
		if len(parts) >= 3 && len(parts[1]) == 32 && parts[1] != strings.Repeat("0", 32) {
			return parts[1], OriginTrace
		}
	}
	return "", OriginSynthetic
}

func (s *Sessions) newSyntheticID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Randomness is unavailable: fall back to a counter rather than
		// colliding every session onto one ID.
		return "synthetic-" + hex.EncodeToString([]byte(time.Now().Format(time.RFC3339Nano)))
	}
	s.synthetic.Add(1)
	return hex.EncodeToString(b[:])
}

// evictLocked drops idle sessions. Called on every Identify, which is cheap
// because the map is small in practice and the scan stops being a concern
// long before a pod handles enough concurrent sessions for it to matter.
func (s *Sessions) evictLocked() {
	cutoff := s.now().Add(-s.ttl)
	for id, st := range s.byID {
		if st.seen.Before(cutoff) {
			delete(s.byID, id)
		}
	}
}

// Len reports how many sessions are being tracked, for metrics and tests.
func (s *Sessions) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.byID)
}
