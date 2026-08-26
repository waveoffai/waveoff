// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package analysis

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Remote sends the decision to an analyzer running somewhere else.
//
// Statistical practice for non-deterministic systems is not settled, and a team
// with a statistician will want their own test. This is how they bring one
// without forking: implement the wire shape, point a rollout at it, and the
// controller neither knows nor cares which implementation answered.
//
// The wire shape is Request and Verdict as JSON — the same types the in-process
// analyzers use, so an implementation in any language has exactly one document
// to read.
type Remote struct {
	// Endpoint receives a POST with the analysis request.
	Endpoint string
	Headers  map[string]string
	Timeout  time.Duration
	Client   *http.Client
}

var _ Analyzer = (*Remote)(nil)

// Name implements Analyzer.
func (r *Remote) Name() string { return "remote:" + r.Endpoint }

// Analyze calls the endpoint.
//
// Every failure path returns an error rather than a verdict. A controller that
// cannot reach its analyzer must hold and page, not promote: failing open on a
// promotion decision is the worst thing this system can do, because it ships
// the exact change nobody was able to check.
func (r *Remote) Analyze(ctx context.Context, req Request) (Verdict, error) {
	if r.Endpoint == "" {
		return Verdict{}, fmt.Errorf("analyzer: no endpoint configured")
	}
	if err := req.Validate(); err != nil {
		return Verdict{}, err
	}

	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	payload, err := json.Marshal(req)
	if err != nil {
		return Verdict{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, r.Endpoint, bytes.NewReader(payload))
	if err != nil {
		return Verdict{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	for k, v := range r.Headers {
		httpReq.Header.Set(k, v)
	}

	client := r.Client
	if client == nil {
		client = &http.Client{}
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return Verdict{}, fmt.Errorf("analyzer %s unreachable: %w", r.Endpoint, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return Verdict{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Verdict{}, fmt.Errorf("analyzer %s returned %s: %s", r.Endpoint, resp.Status, truncate(string(body)))
	}

	var verdict Verdict
	if err := json.Unmarshal(body, &verdict); err != nil {
		return Verdict{}, fmt.Errorf("analyzer %s did not return a verdict: %w\n%s", r.Endpoint, err, truncate(string(body)))
	}
	if err := verdict.Validate(); err != nil {
		return Verdict{}, fmt.Errorf("analyzer %s: %w", r.Endpoint, err)
	}
	if verdict.Analyzer == "" {
		verdict.Analyzer = r.Name()
	}
	return verdict, nil
}

// Validate checks a verdict that arrived from outside this process.
//
// An unrecognised outcome must not be treated as approval. A remote analyzer
// returning something this build does not understand is a version mismatch, and
// guessing which way it meant is how a rollout promotes on a misread.
func (v Verdict) Validate() error {
	switch v.Outcome {
	case OutcomePromote, OutcomeWaveOff, OutcomeInconclusive:
	default:
		return fmt.Errorf("outcome is %q, which this build does not recognise", v.Outcome)
	}
	if v.Outcome == OutcomePromote && v.N == 0 {
		return fmt.Errorf("promotion was returned with no observations behind it")
	}
	return nil
}

func truncate(s string) string {
	const max = 1000
	if len(s) > max {
		return s[:max] + "… (truncated)"
	}
	return s
}
