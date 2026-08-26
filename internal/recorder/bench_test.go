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
		requests    = 2000
		concurrency = 8
		// Repeat the whole measurement and take the median delta.
		//
		// A single p99 is an order statistic: with 400 requests it is the
		// fourth-worst, so one GC pause or one scheduler hiccup in the recorded
		// arm decides the result. Measured on a quiet laptop, the original
		// shape of this test failed roughly one run in five while the actual
		// overhead at p50 was zero — and on a shared CI runner it failed more.
		//
		// That matters more than it sounds. §9 calls this budget an adoption
		// blocker, and a gate on an adoption blocker that cries wolf every
		// fifth run is worse than no gate at all: people learn to re-run it,
		// and then it no longer catches the regression it exists for. So the
		// budget below is unchanged and the estimator is fixed instead.
		repeats = 5
		// §9's budget, on the response a real model call produces rather than a
		// trivial one.
		budget = 5 * time.Millisecond
		// The median is stable enough to hold to a much tighter bound, and it
		// is the number that actually describes the typical call. A real
		// regression in the capture path moves this long before it moves p99.
		medianBudget = 1 * time.Millisecond
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

	pct := func(d []time.Duration, p float64) time.Duration {
		i := int(float64(len(d)) * p)
		if i >= len(d) {
			i = len(d) - 1
		}
		return d[i]
	}

	var p50s, p99s []time.Duration
	for r := 0; r < repeats; r++ {
		// Alternating rather than all-off-then-all-on, so that a machine that
		// gets busier partway through the test penalises both arms equally
		// instead of only the one measured second.
		off := measure(-1)
		on := measure(recorder.DefaultCaptureLimit)
		p50s = append(p50s, pct(on, 0.50)-pct(off, 0.50))
		p99s = append(p99s, pct(on, 0.99)-pct(off, 0.99))
		t.Logf("  run %d: p50 %+7.3fms   p99 %+7.3fms",
			r+1, ms(p50s[r]), ms(p99s[r]))
	}

	p50, p99 := median(p50s), median(p99s)
	t.Logf("median over %d runs of %d requests at concurrency %d, 64KB responses:",
		repeats, requests, concurrency)
	t.Logf("  p50  %+7.3fms", ms(p50))
	t.Logf("  p99  %+7.3fms", ms(p99))

	if p99 > budget {
		t.Errorf("recording adds %v at p99, over the %v budget in §9.\n"+
			"That budget is a stated adoption blocker, not a nice-to-have: a recorder that "+
			"slows down every model call does not get deployed.", p99, budget)
	}
	if p50 > medianBudget {
		t.Errorf("recording adds %v to the median call, over the %v this test holds it to.\n"+
			"The median is the stable statistic here, so a move in it is a real regression in "+
			"the capture path rather than a noisy runner.", p50, medianBudget)
	}
}

// median of a slice of durations. Sorts a copy: the caller's ordering is the
// order the runs happened in, which the log reports.
func median(d []time.Duration) time.Duration {
	c := append([]time.Duration(nil), d...)
	sort.Slice(c, func(i, j int) bool { return c[i] < c[j] })
	return c[len(c)/2]
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
