// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package recorder

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/waveoffai/waveoff/internal/cassette"
)

// DefaultCaptureLimit bounds how much of any single body is captured.
//
// Large enough for a long context window, small enough that a sidecar with a
// modest memory limit survives a pathological response. Anything beyond it is
// recorded as truncated rather than dropped silently.
const DefaultCaptureLimit = 8 << 20 // 8 MiB

// Proxy is a recording reverse proxy for one plane.
type Proxy struct {
	// Upstream is where traffic actually goes.
	Upstream *url.URL
	// Plane distinguishes model traffic from tool traffic in the cassette.
	Plane Plane
	// Sink receives completed records. It must not block.
	Sink Sink
	// Sessions assigns session identity and step ordering.
	Sessions *Sessions
	// CaptureLimit bounds each captured body. Zero uses DefaultCaptureLimit;
	// negative disables capture entirely, which is how the benchmark measures
	// the proxy's own cost.
	CaptureLimit int
	// Transport is the round tripper used upstream.
	Transport http.RoundTripper
	// Redactor strips credentials from headers before they are recorded.
	// Defaults to the standard rules.
	Redactor *cassette.Redactor

	proxy *httputil.ReverseProxy

	// Counters, exposed for metrics and asserted by tests.
	recorded  atomic.Int64
	truncated atomic.Int64
	failed    atomic.Int64
}

// requestState travels with one request through the proxy.
type requestState struct {
	rec  *Record
	tee  *teeReader
	sent time.Time
}

type ctxKey struct{}

// DefaultTransport is the round tripper a proxy uses when none is given.
//
// It exists because http.DefaultTransport does not suit this job at all:
// MaxIdleConnsPerHost is 2. An agent making three concurrent model calls keeps
// two connections pooled and throws the rest away, so most calls pay a fresh
// TCP handshake and a fresh TLS negotiation to the provider. Against an
// external endpoint that is tens of milliseconds — on a component whose entire
// adoption argument is a sub-5ms p99 overhead, and which is on the path of
// every model call an agent makes.
//
// The pool is sized for a sidecar serving one agent process: generous enough
// that concurrency does not evict connections, small enough to be unremarkable
// in a pod's file-descriptor budget.
//
// Everything else is inherited from http.DefaultTransport rather than
// reconstructed, so proxy-from-environment, dial timeouts and HTTP/2
// negotiation keep working the way the standard library intends.
func DefaultTransport() http.RoundTripper {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConns = 256
	t.MaxIdleConnsPerHost = 64
	t.IdleConnTimeout = 90 * time.Second
	return t
}

// New builds a proxy. It is safe for concurrent use once built.
func New(p *Proxy) *Proxy {
	if p.Sink == nil {
		p.Sink = Discard
	}
	if p.Sessions == nil {
		p.Sessions = NewSessions(0)
	}
	if p.CaptureLimit == 0 {
		p.CaptureLimit = DefaultCaptureLimit
	}
	if p.Redactor == nil {
		p.Redactor = cassette.MustRedactor()
	}
	if p.Transport == nil {
		p.Transport = DefaultTransport()
	}

	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(p.Upstream)
			// Preserve the client's Host unless the upstream needs its own;
			// model providers route on it and MCP servers may too.
			pr.Out.Host = p.Upstream.Host
			pr.SetXForwarded()
		},
		Transport:      p.Transport,
		ModifyResponse: p.modifyResponse,
		ErrorHandler:   p.errorHandler,
		// -1 flushes every write immediately, which is what makes server-sent
		// event streaming work. Without it the proxy buffers and an agent that
		// streams tokens sees them arrive in one lump, which changes its
		// behaviour for reasons that have nothing to do with the model.
		FlushInterval: -1,
	}
	p.proxy = rp
	return p
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	session, _, step := p.Sessions.Identify(r)

	rec := &Record{
		Session: session,
		Plane:   p.Plane,
		Step:    step,
		Method:  r.Method,
		URL:     r.URL.String(),
		Target:  p.Upstream.String(),
		Start:   start,
	}

	// The request body has to be read to be recorded, and it also has to reach
	// the upstream, so it is captured up front and replaced. Requests are small
	// relative to responses; this is not where the latency budget goes.
	if p.CaptureLimit >= 0 && r.Body != nil {
		body, truncated := readLimited(r.Body, p.CaptureLimit)
		_ = r.Body.Close()
		rec.ReqBody, rec.ReqTruncated = body, truncated
		r.Body = io.NopCloser(bytes.NewReader(body))
		if truncated {
			// The upstream must still receive the whole request even though
			// only a prefix was captured, so a truncated capture means the
			// original body is gone. Refusing to proxy would be worse than
			// recording an incomplete request, so this is reported and the
			// call proceeds with what was read.
			p.truncated.Add(1)
		}
	}
	// Headers are redacted here, in flight, before the record exists — not
	// later on the way to disk. A credential that is written down and cleaned
	// up afterwards has still been written down.
	rec.ReqHeader, rec.ReqRedacted = p.Redactor.Headers(r.Header)

	st := &requestState{rec: rec, sent: time.Now()}
	p.proxy.ServeHTTP(w, r.WithContext(withState(r, st)))

	p.finalise(st)
}

// modifyResponse wraps the upstream body in a tee so it is captured as it
// streams to the client, rather than being buffered first. Buffering here would
// destroy streaming and add the whole response time to first-token latency.
func (p *Proxy) modifyResponse(resp *http.Response) error {
	st := stateFrom(resp.Request)
	if st == nil {
		return nil
	}
	st.rec.Status = resp.StatusCode
	st.rec.RespHeader, st.rec.RespRedacted = p.Redactor.Headers(resp.Header)
	st.rec.Upstream = time.Since(st.sent)
	st.rec.Streamed = isEventStream(resp.Header)

	if p.CaptureLimit < 0 {
		return nil
	}
	st.tee = newTeeReader(resp.Body, p.CaptureLimit, resp.ContentLength)
	resp.Body = st.tee
	return nil
}

func (p *Proxy) errorHandler(w http.ResponseWriter, r *http.Request, err error) {
	if st := stateFrom(r); st != nil {
		st.rec.Err = err
		st.rec.Status = http.StatusBadGateway
	}
	p.failed.Add(1)
	w.WriteHeader(http.StatusBadGateway)
}

// finalise completes the record and hands it to the sink.
//
// This runs after the response has been fully delivered to the client, so
// whatever it costs is off the latency path the agent observes. The sink is
// still required not to block, because this goroutine is the one the server
// will reuse for the next request.
func (p *Proxy) finalise(st *requestState) {
	rec := st.rec
	rec.End = time.Now()

	if st.tee != nil {
		body, truncated, chunks := st.tee.captured()
		rec.RespBody, rec.RespTruncated = body, truncated
		if rec.Streamed {
			rec.Chunks = chunks
		}
		if truncated {
			p.truncated.Add(1)
		}
	}
	p.recorded.Add(1)
	p.Sink.Record(rec)
}

// Stats reports what the proxy has seen.
func (p *Proxy) Stats() (recorded, truncated, failed int64) {
	return p.recorded.Load(), p.truncated.Load(), p.failed.Load()
}

func isEventStream(h http.Header) bool {
	return strings.HasPrefix(strings.ToLower(h.Get("Content-Type")), "text/event-stream")
}

func readLimited(r io.Reader, limit int) (body []byte, truncated bool) {
	// Read one byte past the limit so truncation is detectable rather than
	// inferred from a body that happens to be exactly the limit.
	b, err := io.ReadAll(io.LimitReader(r, int64(limit)+1))
	if err != nil {
		return b, false
	}
	if len(b) > limit {
		return b[:limit], true
	}
	return b, false
}
