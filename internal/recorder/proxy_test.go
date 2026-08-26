// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package recorder_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/waveoffai/waveoff/internal/recorder"
)

type collector struct {
	mu   sync.Mutex
	recs []*recorder.Record
}

func (c *collector) Record(r *recorder.Record) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recs = append(c.recs, r)
}

func (c *collector) all() []*recorder.Record {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*recorder.Record, len(c.recs))
	copy(out, c.recs)
	return out
}

func newProxy(t *testing.T, target string, sink recorder.Sink) *httptest.Server {
	t.Helper()
	u, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	p := recorder.New(&recorder.Proxy{Upstream: u, Plane: recorder.PlaneModel, Sink: sink})
	s := httptest.NewServer(p)
	t.Cleanup(s.Close)
	return s
}

func TestProxyRecordsBothDirections(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "refund policy") {
			t.Errorf("upstream did not receive the request body: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"content":[{"type":"text","text":"30 days"}]}`)
	}))
	defer up.Close()

	c := &collector{}
	px := newProxy(t, up.URL, c)

	resp, err := http.Post(px.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"refund policy?"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// The client must receive the upstream's response unaltered. A recorder
	// that changes what the agent sees is not observing it.
	if !strings.Contains(string(got), "30 days") {
		t.Errorf("client received %q", got)
	}

	recs := c.all()
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	r := recs[0]
	if !strings.Contains(string(r.ReqBody), "refund policy") {
		t.Errorf("request body not captured: %s", r.ReqBody)
	}
	if !strings.Contains(string(r.RespBody), "30 days") {
		t.Errorf("response body not captured: %s", r.RespBody)
	}
	if r.Status != 200 || r.Method != "POST" || r.URL != "/v1/messages" {
		t.Errorf("record = %+v", r)
	}
	if r.Upstream <= 0 || r.Duration() <= 0 {
		t.Error("timings were not recorded")
	}
}

// TestStreamingIsNotBuffered is the property that makes the recorder usable at
// all: an agent reading a token stream must see chunks arrive as the upstream
// emits them, not in one lump once recording finishes.
func TestStreamingIsNotBuffered(t *testing.T) {
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f := w.(http.Flusher)
		io.WriteString(w, "event: start\ndata: {\"n\":0}\n\n")
		f.Flush()
		// Hold the stream open. If the proxy buffered, the client below would
		// block here and time out.
		<-release
		io.WriteString(w, "event: end\ndata: {\"n\":1}\n\n")
		f.Flush()
	}))
	defer up.Close()

	c := &collector{}
	px := newProxy(t, up.URL, c)

	resp, err := http.Post(px.URL, "application/json", strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	first := make(chan string, 1)
	go func() {
		buf := make([]byte, 256)
		n, _ := resp.Body.Read(buf)
		first <- string(buf[:n])
	}()

	select {
	case chunk := <-first:
		if !strings.Contains(chunk, `"n":0`) {
			t.Errorf("first chunk = %q", chunk)
		}
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("the first chunk never arrived; the proxy is buffering the stream")
	}

	close(release)
	io.Copy(io.Discard, resp.Body)

	// Wait for the record to land.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(c.all()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	recs := c.all()
	if len(recs) != 1 {
		t.Fatalf("got %d records", len(recs))
	}
	if !recs[0].Streamed {
		t.Error("the response was not marked as streamed")
	}
	if recs[0].Chunks < 2 {
		t.Errorf("chunks = %d, want at least 2; replay has to reproduce the streaming shape", recs[0].Chunks)
	}
	if !strings.Contains(string(recs[0].RespBody), `"n":1`) {
		t.Errorf("the full stream was not captured: %s", recs[0].RespBody)
	}
}

// TestSessionFromTraceparent: deriving the session from the agent's own trace
// context is what lets a multi-step loop be recorded as one session with no
// code change.
func TestSessionFromTraceparent(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "{}")
	}))
	defer up.Close()

	c := &collector{}
	px := newProxy(t, up.URL, c)

	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	client := &http.Client{}
	for i := 0; i < 3; i++ {
		req, _ := http.NewRequest("POST", px.URL, strings.NewReader("{}"))
		req.Header.Set("traceparent", "00-"+traceID+"-00f067aa0ba902b7-01")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	recs := c.all()
	if len(recs) != 3 {
		t.Fatalf("got %d records", len(recs))
	}
	for i, r := range recs {
		if r.Session != traceID {
			t.Errorf("record %d: session = %q, want the trace id %q", i, r.Session, traceID)
		}
		if r.Step != i {
			t.Errorf("record %d: step = %d; ordering is part of the replay matching key", i, r.Step)
		}
	}
}

func TestExplicitSessionHeaderWins(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "{}")
	}))
	defer up.Close()

	c := &collector{}
	px := newProxy(t, up.URL, c)

	req, _ := http.NewRequest("POST", px.URL, strings.NewReader("{}"))
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	req.Header.Set(recorder.SessionHeader, "my-session")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if got := c.all()[0].Session; got != "my-session" {
		t.Errorf("session = %q, want the explicit header to win", got)
	}
}

// TestWithoutTraceContextSessionsAreDistinct: each request becomes its own
// session, which is degraded but honest — steps cannot be correlated across a
// boundary that does not exist.
func TestWithoutTraceContextSessionsAreDistinct(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "{}")
	}))
	defer up.Close()

	c := &collector{}
	px := newProxy(t, up.URL, c)
	for i := 0; i < 3; i++ {
		resp, err := http.Post(px.URL, "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	seen := map[string]bool{}
	for _, r := range c.all() {
		if r.Session == "" {
			t.Fatal("a record has no session")
		}
		seen[r.Session] = true
	}
	if len(seen) != 3 {
		t.Errorf("got %d distinct sessions, want 3", len(seen))
	}
}

// TestTruncationIsReported: recording a prefix as though it were a whole
// payload would make a replay compare against something that never happened.
func TestTruncationIsReported(t *testing.T) {
	big := strings.Repeat("z", 100_000)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, big)
	}))
	defer up.Close()

	u, _ := url.Parse(up.URL)
	c := &collector{}
	p := recorder.New(&recorder.Proxy{
		Upstream: u, Plane: recorder.PlaneModel, Sink: c,
		CaptureLimit: 1000,
	})
	px := httptest.NewServer(p)
	defer px.Close()

	resp, err := http.Post(px.URL, "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// The client still gets everything: capture limits bound what is recorded,
	// never what the agent receives.
	if len(got) != len(big) {
		t.Errorf("client received %d bytes, want %d; a capture limit must not truncate the response", len(got), len(big))
	}

	r := c.all()[0]
	if !r.RespTruncated {
		t.Error("truncation was not reported")
	}
	if len(r.RespBody) != 1000 {
		t.Errorf("captured %d bytes, want the 1000-byte limit", len(r.RespBody))
	}
}

func TestUpstreamFailureIsRecorded(t *testing.T) {
	u, _ := url.Parse("http://127.0.0.1:1") // nothing listening
	c := &collector{}
	p := recorder.New(&recorder.Proxy{Upstream: u, Plane: recorder.PlaneModel, Sink: c})
	px := httptest.NewServer(p)
	defer px.Close()

	resp, err := http.Post(px.URL, "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	recs := c.all()
	if len(recs) != 1 {
		t.Fatalf("got %d records; a failed call is still a recording", len(recs))
	}
	if recs[0].Err == nil {
		t.Error("the upstream error was not recorded")
	}
	if _, _, failed := p.Stats(); failed != 1 {
		t.Errorf("failed count = %d", failed)
	}
}

func TestConcurrentSessionsKeepTheirOwnStepOrder(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "{}")
	}))
	defer up.Close()

	c := &collector{}
	px := newProxy(t, up.URL, c)

	const sessions, steps = 6, 10
	var wg sync.WaitGroup
	for s := 0; s < sessions; s++ {
		wg.Add(1)
		go func(s int) {
			defer wg.Done()
			client := &http.Client{}
			for i := 0; i < steps; i++ {
				req, _ := http.NewRequest("POST", px.URL, strings.NewReader("{}"))
				req.Header.Set(recorder.SessionHeader, fmt.Sprintf("sess-%d", s))
				resp, err := client.Do(req)
				if err != nil {
					return
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}(s)
	}
	wg.Wait()

	bySession := map[string][]int{}
	for _, r := range c.all() {
		bySession[r.Session] = append(bySession[r.Session], r.Step)
	}
	if len(bySession) != sessions {
		t.Fatalf("got %d sessions, want %d", len(bySession), sessions)
	}
	for id, got := range bySession {
		seen := map[int]bool{}
		for _, step := range got {
			if seen[step] {
				t.Errorf("%s: step %d issued twice; step ordering is the replay matching key", id, step)
			}
			seen[step] = true
		}
		if len(got) != steps {
			t.Errorf("%s: %d steps, want %d", id, len(got), steps)
		}
	}
}
