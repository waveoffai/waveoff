// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package recorder_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/waveoffai/waveoff/internal/recorder"
)

// §9 states a <5ms p99 overhead budget and calls it an adoption blocker. These
// benchmarks exist so that number is measured rather than asserted, and they
// run in CI so a regression is caught by the build rather than by a customer.
//
// The comparison is deliberately against the same reverse proxy with capture
// switched off, not against a direct call. That isolates what *recording* costs
// from what proxying costs, because an operator who adopts a sidecar is paying
// the proxy hop either way.

// upstream returns a server that responds with a body of the given size.
func upstream(tb testing.TB, size int) *httptest.Server {
	body := strings.Repeat("x", size)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		// Non-streaming model responses arrive with a Content-Length, and the
		// recorder uses it to size its capture buffer. Omitting it here would
		// benchmark a path real traffic does not take.
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		io.WriteString(w, body)
	}))
	tb.Cleanup(s.Close)
	return s
}

// streamingUpstream emits an SSE response in chunks, like a token stream.
func streamingUpstream(tb testing.TB, chunks int) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for i := 0; i < chunks; i++ {
			fmt.Fprintf(w, "event: content_block_delta\ndata: {\"delta\":{\"text\":\"token %d\"}}\n\n", i)
			flusher.Flush()
		}
	}))
	tb.Cleanup(s.Close)
	return s
}

func proxyFor(tb testing.TB, target string, capture int) *httptest.Server {
	u, err := url.Parse(target)
	if err != nil {
		tb.Fatal(err)
	}
	p := recorder.New(&recorder.Proxy{
		Upstream:     u,
		Plane:        recorder.PlaneModel,
		Sink:         recorder.Discard,
		CaptureLimit: capture,
	})
	s := httptest.NewServer(p)
	tb.Cleanup(s.Close)
	return s
}

func post(tb testing.TB, client *http.Client, target, body string) {
	tb.Helper()
	resp, err := client.Post(target, "application/json", strings.NewReader(body))
	if err != nil {
		tb.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

// BenchmarkProxy measures per-request cost with and without capture, across the
// payload sizes a model call actually produces.
func BenchmarkProxy(b *testing.B) {
	sizes := []struct {
		name string
		n    int
	}{
		{"1KB", 1 << 10},
		{"64KB", 64 << 10},
		{"1MB", 1 << 20},
	}
	reqBody := strings.Repeat("q", 4<<10) // a modest prompt

	for _, size := range sizes {
		up := upstream(b, size.n)
		for _, mode := range []struct {
			name    string
			capture int
		}{
			{"capture-off", -1},
			{"capture-on", recorder.DefaultCaptureLimit},
		} {
			b.Run(size.name+"/"+mode.name, func(b *testing.B) {
				px := proxyFor(b, up.URL, mode.capture)
				client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 64}}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					post(b, client, px.URL, reqBody)
				}
			})
		}
	}
}

// BenchmarkStreaming measures the streaming path, which is the one that matters
// for perceived latency: an agent reading a token stream must not have to wait
// for the recorder.
func BenchmarkStreaming(b *testing.B) {
	up := streamingUpstream(b, 200)
	for _, mode := range []struct {
		name    string
		capture int
	}{
		{"capture-off", -1},
		{"capture-on", recorder.DefaultCaptureLimit},
	} {
		b.Run(mode.name, func(b *testing.B) {
			px := proxyFor(b, up.URL, mode.capture)
			client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 64}}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				post(b, client, px.URL, `{"stream":true}`)
			}
		})
	}
}

// TestOverheadBudget is the assertion, as distinct from the measurement.
//
// It compares percentiles of the same proxy with capture on and off under
// concurrency, and fails if recording costs more than the budget §9 sets. A
// benchmark nobody reads does not stop a regression; this does.
func TestOverheadBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("latency measurement is not meaningful under -short")
	}

	const (
		requests    = 400
		concurrency = 8
		// §9's budget. Measured on the response a real model call produces
		// rather than a trivial one.
		budget = 5 * time.Millisecond
	)
	up := upstream(t, 64<<10)
	reqBody := strings.Repeat("q", 4<<10)

	measure := func(capture int) []time.Duration {
		px := proxyFor(t, up.URL, capture)
		client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: concurrency * 2}}

		// Warm the connection pool so the first-request TLS/TCP cost does not
		// land in the sample.
		for i := 0; i < concurrency; i++ {
			post(t, client, px.URL, reqBody)
		}

		out := make([]time.Duration, requests)
		var wg sync.WaitGroup
		sem := make(chan struct{}, concurrency)
		for i := 0; i < requests; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				start := time.Now()
				post(t, client, px.URL, reqBody)
				out[i] = time.Since(start)
			}(i)
		}
		wg.Wait()
		sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
		return out
	}

	off := measure(-1)
	on := measure(recorder.DefaultCaptureLimit)

	pct := func(d []time.Duration, p float64) time.Duration {
		i := int(float64(len(d)) * p)
		if i >= len(d) {
			i = len(d) - 1
		}
		return d[i]
	}

	p50, p99 := pct(on, 0.50)-pct(off, 0.50), pct(on, 0.99)-pct(off, 0.99)
	t.Logf("recording overhead over %d requests at concurrency %d, 64KB responses:", requests, concurrency)
	t.Logf("  p50  %6.3fms  (%v -> %v)", float64(p50)/float64(time.Millisecond), pct(off, 0.50), pct(on, 0.50))
	t.Logf("  p99  %6.3fms  (%v -> %v)", float64(p99)/float64(time.Millisecond), pct(off, 0.99), pct(on, 0.99))

	if p99 > budget {
		t.Errorf("recording adds %v at p99, over the %v budget in §9.\n"+
			"That budget is a stated adoption blocker, not a nice-to-have: a recorder that "+
			"slows down every model call does not get deployed.", p99, budget)
	}
}
