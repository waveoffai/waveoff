// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package score

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// HTTPScorer posts refs to an endpoint and reads results back.
//
// The other half of the vendor story: a hosted eval service exposes an
// endpoint, and this speaks to it without either side depending on the other's
// code. Same wire shape as ExecScorer, so a scorer written for one works for
// the other with a different front door.
type HTTPScorer struct {
	// Endpoint receives a POST with the refs document.
	Endpoint string
	// Headers carries authentication. Values are never logged.
	Headers map[string]string
	// Timeout bounds the call.
	Timeout time.Duration
	// Client may be replaced, for tests or for a custom transport.
	Client *http.Client
}

var _ Scorer = (*HTTPScorer)(nil)

// Score calls the endpoint.
func (h *HTTPScorer) Score(ctx context.Context, refs []Ref) ([]Result, error) {
	if h.Endpoint == "" {
		return nil, fmt.Errorf("scorer: no endpoint configured")
	}
	timeout := h.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	client := h.Client
	if client == nil {
		client = &http.Client{}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	payload, err := json.Marshal(request{SchemaVersion: SchemaVersion, Refs: refs})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.Endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for k, v := range h.Headers {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("scorer %s: %w", h.Endpoint, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// A scoring service being down is an infrastructure failure, not a
		// verdict. It must surface as an error so the rollout holds, rather
		// than as an absence of scores that a gate could read as a pass.
		return nil, fmt.Errorf("scorer %s returned %s: %s", h.Endpoint, resp.Status, trim(string(body)))
	}

	var out response
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("scorer %s did not return the expected JSON: %w\n%s",
			h.Endpoint, err, trim(string(body)))
	}
	if err := Validate(out.Results); err != nil {
		return nil, fmt.Errorf("scorer %s: %w", h.Endpoint, err)
	}
	return out.Results, nil
}
