// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package recorder

import (
	"context"
	"net/http"
)

// The reverse proxy hands ModifyResponse an *http.Response whose Request is the
// outbound one, so per-request state travels through the context rather than a
// map keyed by pointer. A map would need locking on the hot path and would leak
// on any path that does not reach finalise.

func withState(r *http.Request, st *requestState) context.Context {
	return context.WithValue(r.Context(), ctxKey{}, st)
}

func stateFrom(r *http.Request) *requestState {
	if r == nil {
		return nil
	}
	st, _ := r.Context().Value(ctxKey{}).(*requestState)
	return st
}
