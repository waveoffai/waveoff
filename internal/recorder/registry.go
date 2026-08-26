// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package recorder

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"time"
)

// syntheticRegistry remembers the objects a suppressed write pretended to
// create, so that the agent can read them back.
//
// Without this, shadow traffic is unmeasurable for any agent that checks its
// own work — which is most agents worth shadowing. A suppressed
// jira.create_issue answers with an id; the agent then comments on that issue,
// or reads it back to confirm the write landed. If that read reaches the live
// server the object is not there, so the agent retries, escalates, or reports
// failure. All of which is measured as a behavioural regression in the shadow
// arm, and none of which the candidate did: it is an artefact of suppression
// dressed up as evidence.
//
// The registry is scoped to a session, because that is the lifetime over which
// an agent expects its own writes to be visible, and because two concurrent
// sessions inventing the same identifier would be able to see each other's
// work.
type syntheticRegistry struct {
	mu       sync.RWMutex
	sessions map[string]*syntheticSession
	ttl      time.Duration
	now      func() time.Time
}

type syntheticSession struct {
	// objects maps a synthetic id to the result that was handed back for it.
	objects map[string]map[string]any
	seen    time.Time
}

func newSyntheticRegistry(ttl time.Duration) *syntheticRegistry {
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	return &syntheticRegistry{
		sessions: map[string]*syntheticSession{},
		ttl:      ttl,
		now:      time.Now,
	}
}

// mint records a synthetic object for a suppressed write and returns its id.
//
// The id is derived from the session, the tool and the arguments, so the same
// call made twice within a session yields the same object. An agent that
// retries a write it believes failed should find the thing it already
// "created", not a second copy.
func (r *syntheticRegistry) mint(session, tool string, args json.RawMessage, result map[string]any) string {
	sum := sha256.Sum256([]byte(session + "\x00" + tool + "\x00" + string(args)))
	id := "waveoff-synthetic-" + hex.EncodeToString(sum[:])[:16]

	r.mu.Lock()
	defer r.mu.Unlock()
	r.evictLocked()

	s, ok := r.sessions[session]
	if !ok {
		s = &syntheticSession{objects: map[string]map[string]any{}}
		r.sessions[session] = s
	}
	s.seen = r.now()
	if _, exists := s.objects[id]; !exists {
		s.objects[id] = result
	}
	return id
}

// lookup finds a synthetic object referenced anywhere in a call's arguments.
//
// Matching on the whole argument blob rather than a known field name is
// deliberate: every tool names its identifiers differently — issue, issueKey,
// id, ticket, parent — and a list of field names would be wrong for the first
// server nobody thought of. A synthetic id is a string this process invented
// and nothing else can produce, so finding it anywhere in the arguments is
// sufficient evidence that the call is about an object that does not exist.
func (r *syntheticRegistry) lookup(session string, args json.RawMessage) (map[string]any, string, bool) {
	if len(args) == 0 {
		return nil, "", false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	s, ok := r.sessions[session]
	if !ok {
		return nil, "", false
	}
	blob := string(args)
	for id, result := range s.objects {
		if strings.Contains(blob, id) {
			return result, id, true
		}
	}
	return nil, "", false
}

// evictLocked drops sessions that have gone quiet, so a sidecar running for the
// life of a pod does not accumulate every object it ever invented.
func (r *syntheticRegistry) evictLocked() {
	cutoff := r.now().Add(-r.ttl)
	for id, s := range r.sessions {
		if s.seen.Before(cutoff) {
			delete(r.sessions, id)
		}
	}
}
