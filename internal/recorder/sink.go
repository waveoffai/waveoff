// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package recorder

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gowebpki/jcs"

	"github.com/waveoffai/waveoff/internal/cas"
	"github.com/waveoffai/waveoff/internal/cassette"
	"github.com/waveoffai/waveoff/internal/corpus"
)

// CassetteSink turns proxied calls into cassettes, off the request path.
//
// The contract with the proxy is that Record never blocks. Everything expensive
// — redaction, blob offload, file writes — happens on a worker goroutine, and
// if the queue is full records are dropped and counted rather than allowed to
// become latency on a model call. Losing a recording is a bad day; slowing down
// every request is a product that does not get deployed.
type CassetteSink struct {
	store corpus.Store
	blobs cas.Store
	queue chan *Record
	log   *slog.Logger
	idle  time.Duration
	now   func() time.Time

	// Manifest identity stamped into every cassette header. Without it a corpus
	// is traffic that cannot be attributed to a version.
	agent          string
	behaviorDigest string
	contentDigest  string

	mu   sync.Mutex
	open map[string]*openCassette

	wg        sync.WaitGroup
	closing   chan struct{}
	closeOnce sync.Once

	written atomic.Int64
	dropped atomic.Int64
	errored atomic.Int64
}

type openCassette struct {
	w        io.WriteCloser
	cw       *cassette.Writer
	lastSeen time.Time
}

// SinkConfig configures a CassetteSink.
type SinkConfig struct {
	Store corpus.Store
	Blobs cas.Store
	Log   *slog.Logger

	Agent          string
	BehaviorDigest string
	ContentDigest  string

	// QueueDepth bounds how many completed records may be waiting. Deeper
	// tolerates a longer burst at the cost of memory; it does not change the
	// drop-rather-than-block rule.
	QueueDepth int
	// IdleTimeout is how long a session may go quiet before its cassette is
	// closed. A session has no explicit end — the agent just stops calling.
	IdleTimeout time.Duration
}

// NewCassetteSink starts the worker. Close must be called to flush.
func NewCassetteSink(cfg SinkConfig) *CassetteSink {
	if cfg.QueueDepth <= 0 {
		cfg.QueueDepth = 1024
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 2 * time.Minute
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	s := &CassetteSink{
		store: cfg.Store, blobs: cfg.Blobs, log: cfg.Log,
		queue:          make(chan *Record, cfg.QueueDepth),
		idle:           cfg.IdleTimeout,
		now:            time.Now,
		agent:          cfg.Agent,
		behaviorDigest: cfg.BehaviorDigest,
		contentDigest:  cfg.ContentDigest,
		open:           map[string]*openCassette{},
		closing:        make(chan struct{}),
	}
	s.wg.Add(1)
	go s.run()
	return s
}

// Record implements Sink. It never blocks.
func (s *CassetteSink) Record(r *Record) {
	select {
	case s.queue <- r:
	default:
		// The queue is full. Dropping is the correct failure: the alternative
		// is back-pressure onto the agent's model calls.
		if n := s.dropped.Add(1); n == 1 || n%1000 == 0 {
			s.log.Warn("dropping recordings; the sink cannot keep up",
				"dropped", n, "session", r.Session,
				"hint", "raise QueueDepth or check blob store latency")
		}
	}
}

func (s *CassetteSink) run() {
	defer s.wg.Done()
	sweep := time.NewTicker(s.idle / 2)
	defer sweep.Stop()

	for {
		select {
		case rec := <-s.queue:
			s.write(rec)
		case <-sweep.C:
			s.closeIdle(false)
		case <-s.closing:
			// Drain whatever is queued before shutting down, so a graceful
			// pod termination does not throw away the tail of a session.
			for {
				select {
				case rec := <-s.queue:
					s.write(rec)
				default:
					s.closeIdle(true)
					return
				}
			}
		}
	}
}

func (s *CassetteSink) write(rec *Record) {
	ctx := context.Background()
	oc, err := s.cassetteFor(ctx, rec)
	if err != nil {
		s.errored.Add(1)
		s.log.Error("cannot open cassette", "session", rec.Session, "err", err)
		return
	}
	if err := oc.cw.WriteSpan(ctx, s.span(rec)); err != nil {
		s.errored.Add(1)
		s.log.Error("cannot write span", "session", rec.Session, "err", err)
		return
	}
	oc.lastSeen = s.now()
	s.written.Add(1)
}

func (s *CassetteSink) cassetteFor(ctx context.Context, rec *Record) (*openCassette, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if oc, ok := s.open[rec.Session]; ok {
		return oc, nil
	}

	h := cassette.Header{
		SessionID:      rec.Session,
		Agent:          s.agent,
		BehaviorDigest: s.behaviorDigest,
		ContentDigest:  s.contentDigest,
		RecordedAt:     s.now().UTC(),
		Recorder:       "waveoff-recorder",
	}
	w, err := s.store.Create(ctx, h)
	if err != nil {
		return nil, err
	}
	oc := &openCassette{w: w, cw: cassette.NewWriter(w, s.blobs), lastSeen: s.now()}
	// The corpus store already wrote the header; the writer needs to know that
	// so it does not write a second one.
	if err := oc.cw.AdoptHeader(); err != nil {
		_ = w.Close()
		return nil, err
	}
	s.open[rec.Session] = oc
	return oc, nil
}

// closeIdle closes cassettes whose sessions have gone quiet. A session has no
// explicit end: the agent simply stops calling, so quiet is the only signal.
func (s *CassetteSink) closeIdle(all bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := s.now().Add(-s.idle)
	for id, oc := range s.open {
		if all || oc.lastSeen.Before(cutoff) {
			if err := oc.w.Close(); err != nil {
				s.errored.Add(1)
				s.log.Error("cannot close cassette", "session", id, "err", err)
			}
			delete(s.open, id)
		}
	}
}

// span converts a proxied call into a cassette span, using the OpenTelemetry
// GenAI and MCP semantic conventions for everything that describes the call
// itself, and waveoff.* only for what this system adds.
func (s *CassetteSink) span(rec *Record) *cassette.Span {
	name, kind := SpanNameAndKind(rec)
	return &cassette.Span{
		Name:       name,
		Kind:       kind,
		StartTime:  rec.Start.UTC(),
		EndTime:    rec.End.UTC(),
		Attributes: SpanAttributes(rec),
		Status:     SpanStatus(rec),
	}
}

// SpanNameAndKind derives a span's name and classification from a record.
//
// Exported so the cassette writer and the OTLP exporter describe the same call
// the same way. Two views of one call that disagree are worse than one view.
func SpanNameAndKind(rec *Record) (string, cassette.Kind) {
	if rec.Plane == PlaneTool {
		if rec.ToolName != "" {
			return cassette.OpTool + " " + rec.ToolName, cassette.KindTool
		}
		return cassette.OpTool, cassette.KindTool
	}
	if model := modelFrom(rec.ReqBody); model != "" {
		return cassette.OpChat + " " + model, cassette.KindModel
	}
	return cassette.OpChat, cassette.KindModel
}

// SpanAttributes builds the attribute set for a recorded call, using the
// OpenTelemetry GenAI and MCP semantic conventions for everything that
// describes the call itself and waveoff.* only for what this system adds.
func SpanAttributes(rec *Record) map[string]any {
	attrs := map[string]any{
		cassette.AttrSessionID:          rec.Session,
		cassette.AttrStepIndex:          rec.Step,
		cassette.AttrRequestBody:        string(rec.ReqBody),
		cassette.AttrResponseBody:       string(rec.RespBody),
		cassette.AttrUpstreamStatus:     rec.Status,
		cassette.AttrUpstreamDurationMS: rec.Upstream.Milliseconds(),
		cassette.AttrRequestHash:        NormalisedHash(rec.ReqBody),
		"http.request.method":           rec.Method,
		"url.path":                      rec.URL,
		"server.address":                rec.Target,
	}
	// Headers matter for replay and for the manifest: a provider reports the
	// resolved model version in a response header, and §5 pins it. They arrive
	// here already redacted.
	if h := flattenHeader(rec.ReqHeader); len(h) > 0 {
		attrs[cassette.AttrRequestHeaders] = h
	}
	if h := flattenHeader(rec.RespHeader); len(h) > 0 {
		attrs[cassette.AttrResponseHeaders] = h
	}
	if labels := append(append([]string{}, rec.ReqRedacted...), rec.RespRedacted...); len(labels) > 0 {
		attrs[cassette.AttrRedacted] = labels
	}
	if rec.Streamed {
		attrs[cassette.AttrStreamed] = true
		attrs[cassette.AttrStreamChunks] = rec.Chunks
	}
	if rec.ReqTruncated || rec.RespTruncated {
		// Say so rather than let a prefix be read as a whole payload.
		attrs["waveoff.truncated"] = true
	}
	if model := modelFrom(rec.ReqBody); model != "" {
		attrs["gen_ai.request.model"] = model
	}
	if rec.Plane == PlaneModel {
		attrs["gen_ai.operation.name"] = cassette.OpChat
	} else {
		mcpAttributes(rec, attrs)
	}
	return attrs
}

// flattenHeader renders headers as a single string per name.
//
// A map of slices survives neither an OTel attribute nor a clean JSON round
// trip, and multi-valued headers are rare enough in this traffic that joining
// them loses nothing a replay needs.
func flattenHeader(h http.Header) map[string]string {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string]string, len(h))
	for name, values := range h {
		out[strings.ToLower(name)] = strings.Join(values, ", ")
	}
	return out
}

// SpanStatus maps a recorded call onto a span status.
func SpanStatus(rec *Record) cassette.Status {
	if rec.Err != nil {
		return cassette.Status{Code: "ERROR", Message: rec.Err.Error()}
	}
	if rec.Status >= 400 {
		return cassette.Status{Code: "ERROR", Message: "upstream returned " + itoa(rec.Status)}
	}
	return cassette.Status{}
}

// NormalisedHash is the replay matching key.
//
// It canonicalises a JSON request under RFC 8785 before hashing, so a request
// that is semantically identical but differs in key order or number formatting
// still matches its recording. A byte-for-byte hash would make replay miss
// constantly for reasons that have nothing to do with the agent.
func NormalisedHash(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	canonical := body
	if looksLikeJSON(body) {
		if c, err := jcs.Transform(body); err == nil {
			canonical = c
		}
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func looksLikeJSON(b []byte) bool {
	t := strings.TrimLeft(string(b), " \t\r\n")
	return strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[")
}

// modelFrom pulls the model id out of a request body. Both the Anthropic and
// OpenAI shapes put it at the top level under "model".
func modelFrom(body []byte) string {
	if !looksLikeJSON(body) {
		return ""
	}
	var probe struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return ""
	}
	return probe.Model
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

// Close flushes and stops the worker.
//
// Idempotent on purpose. A sidecar's shutdown path routinely closes
// defensively — a deferred Close plus an explicit one on the graceful path —
// and panicking there would turn a clean termination into a crash loop that
// loses the tail of every in-flight session.
func (s *CassetteSink) Close() error {
	s.closeOnce.Do(func() {
		close(s.closing)
		s.wg.Wait()
	})
	return nil
}

// Stats reports what the sink has done: written spans, dropped records, and
// errors. Drops are the number an operator needs to see, because a silently
// lossy recorder produces a corpus with holes nobody knows about.
func (s *CassetteSink) Stats() (written, dropped, errored int64) {
	return s.written.Load(), s.dropped.Load(), s.errored.Load()
}
