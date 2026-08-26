// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package analysis_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/waveoffai/waveoff/internal/analysis"
)

func remoteReturning(t *testing.T, status int, body any) *analysis.Remote {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The request must be the documented shape, or an implementation in
		// another language has nothing stable to read.
		var req analysis.Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("the analyzer received something that is not a Request: %v", err)
		}
		if req.Primary.Name == "" || len(req.Observations) == 0 {
			t.Errorf("the analyzer received an incomplete request: %+v", req)
		}
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return &analysis.Remote{Endpoint: srv.URL, Timeout: 10 * time.Second}
}

func sampleRequest() analysis.Request {
	return request(noisyPair(20, 0, 1), -0.02)
}

func TestRemoteAnalyzerRoundTrip(t *testing.T) {
	r := remoteReturning(t, 200, analysis.Verdict{
		Outcome: analysis.OutcomePromote, Reason: "non-inferior", N: 20,
	})
	v, err := r.Analyze(context.Background(), sampleRequest())
	if err != nil {
		t.Fatal(err)
	}
	if v.Outcome != analysis.OutcomePromote {
		t.Errorf("outcome = %s", v.Outcome)
	}
	// A decision must be traceable to whatever made it.
	if v.Analyzer == "" {
		t.Error("the verdict does not say which analyzer produced it")
	}
}

// TestUnreachableAnalyzerIsAnError, never a verdict. A controller that cannot
// reach its analyzer must hold and page, not promote.
func TestUnreachableAnalyzerIsAnError(t *testing.T) {
	r := &analysis.Remote{Endpoint: "http://127.0.0.1:1", Timeout: time.Second}
	if _, err := r.Analyze(context.Background(), sampleRequest()); err == nil {
		t.Fatal("an unreachable analyzer returned a verdict")
	}
}

func TestAnalyzerErrorIsNotAVerdict(t *testing.T) {
	r := remoteReturning(t, 500, map[string]string{"error": "boom"})
	if _, err := r.Analyze(context.Background(), sampleRequest()); err == nil {
		t.Fatal("a 500 from the analyzer was read as a decision")
	}
}

// TestUnknownOutcomeIsRejected: a remote analyzer returning something this
// build does not understand is a version mismatch, and guessing which way it
// meant is how a rollout promotes on a misread.
func TestUnknownOutcomeIsRejected(t *testing.T) {
	r := remoteReturning(t, 200, map[string]any{"outcome": "probably-fine", "n": 20})
	_, err := r.Analyze(context.Background(), sampleRequest())
	if err == nil {
		t.Fatal("an unrecognised outcome was accepted")
	}
	if !strings.Contains(err.Error(), "does not recognise") {
		t.Errorf("err = %v", err)
	}
}

// TestPromotionWithNoEvidenceIsRejected guards the worst failure mode: an
// analyzer that says promote without having measured anything.
func TestPromotionWithNoEvidenceIsRejected(t *testing.T) {
	r := remoteReturning(t, 200, analysis.Verdict{Outcome: analysis.OutcomePromote, N: 0})
	if _, err := r.Analyze(context.Background(), sampleRequest()); err == nil {
		t.Fatal("promotion with no observations behind it was accepted")
	}
}

func TestRemoteTimeoutIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	defer srv.Close()

	r := &analysis.Remote{Endpoint: srv.URL, Timeout: 200 * time.Millisecond}
	if _, err := r.Analyze(context.Background(), sampleRequest()); err == nil {
		t.Fatal("a hanging analyzer returned a verdict")
	}
}

// TestInProcessAndRemoteAgree: the two are interchangeable behind the
// interface, which is what makes bringing your own test possible.
func TestInProcessAndRemoteAgree(t *testing.T) {
	req := request(noisyPair(40, -0.15, 3), -0.02)

	local, err := (&analysis.PairedBootstrap{Resamples: 2000}).Analyze(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}

	// A remote analyzer that happens to run the same test.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got analysis.Request
		json.NewDecoder(r.Body).Decode(&got)
		v, err := (&analysis.PairedBootstrap{Resamples: 2000}).Analyze(r.Context(), got)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		json.NewEncoder(w).Encode(v)
	}))
	defer srv.Close()

	remote, err := (&analysis.Remote{Endpoint: srv.URL}).Analyze(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if remote.Outcome != local.Outcome {
		t.Errorf("the same request decided differently in-process (%s) and remotely (%s)",
			local.Outcome, remote.Outcome)
	}
}
