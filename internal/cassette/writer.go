// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package cassette

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/waveoffai/waveoff/internal/cas"
)

// Writer serialises a session to a cassette.
//
// The format is newline-delimited JSON: one header, then one span per line.
// That is a deliberate choice over a single JSON document. A session is written
// incrementally while it is still running, a crashed recorder should leave a
// readable prefix rather than an unparseable file, and an operator debugging a
// production incident at 2am should be able to grep it.
type Writer struct {
	mu       sync.Mutex
	enc      *json.Encoder
	out      io.Writer
	blobs    cas.Store
	redactor *Redactor

	threshold   int
	wroteHeader bool
	nextStep    int
}

// WriterOption configures a Writer.
type WriterOption func(*Writer)

// WithInlineThreshold sets how large a payload may be before it is offloaded to
// the blob store. Zero means never offload, which is useful for tests that want
// a self-contained file.
func WithInlineThreshold(n int) WriterOption {
	return func(w *Writer) { w.threshold = n }
}

// WithRedactor replaces the default credential rules.
func WithRedactor(r *Redactor) WriterOption {
	return func(w *Writer) { w.redactor = r }
}

// NewWriter builds a cassette writer.
//
// blobs may be nil, in which case payloads are always inlined. That is the
// right behaviour for a single self-contained cassette attached to a bug
// report, and the wrong one for a corpus.
func NewWriter(out io.Writer, blobs cas.Store, opts ...WriterOption) *Writer {
	w := &Writer{
		enc:       json.NewEncoder(out),
		out:       out,
		blobs:     blobs,
		redactor:  MustRedactor(),
		threshold: InlineThreshold,
	}
	for _, o := range opts {
		o(w)
	}
	return w
}

// WriteHeader writes the first line. It must be called before any span.
func (w *Writer) WriteHeader(h Header) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.wroteHeader {
		return fmt.Errorf("cassette: header already written")
	}
	// The schema version is set here rather than trusted from the caller. A
	// cassette claiming a version it was not written against is worse than one
	// with no version at all.
	h.SchemaVersion = SchemaVersion
	if err := w.enc.Encode(h); err != nil {
		return fmt.Errorf("cassette: write header: %w", err)
	}
	w.wroteHeader = true
	return nil
}

// AdoptHeader marks the header as already written, for a caller that created
// the cassette through a corpus store and holds only the remaining writer.
func (w *Writer) AdoptHeader() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.wroteHeader {
		return fmt.Errorf("cassette: header already written")
	}
	w.wroteHeader = true
	return nil
}

// WriteSpan appends a span, assigning its step index, redacting its payloads
// and offloading anything above the inline threshold.
//
// Redaction happens here as well as at the point of capture. That is
// deliberate duplication: this is the last place before bytes reach durable
// storage, and a credential that gets past both is one nobody will notice until
// the corpus is shared.
func (w *Writer) WriteSpan(ctx context.Context, s *Span) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.wroteHeader {
		return fmt.Errorf("cassette: WriteHeader must be called first")
	}

	if s.Attributes == nil {
		s.Attributes = map[string]any{}
	}
	s.Attributes[AttrSchemaVersion] = SchemaVersion
	s.Attributes[AttrKind] = string(s.Kind)

	// Session spans bound the recording rather than being a step in it, so they
	// are not numbered and cannot be matched against during replay.
	if s.Kind != KindSession {
		if _, ok := s.StepIndex(); !ok {
			s.Attributes[AttrStepIndex] = w.nextStep
			w.nextStep++
		}
	}

	for _, p := range []struct{ body, ref, size string }{
		{AttrRequestBody, AttrRequestRef, AttrRequestBytes},
		{AttrResponseBody, AttrResponseRef, AttrResponseBytes},
	} {
		if err := w.handlePayload(ctx, s, p.body, p.ref, p.size); err != nil {
			return err
		}
	}

	if err := w.enc.Encode(s); err != nil {
		return fmt.Errorf("cassette: write span: %w", err)
	}
	return nil
}

// handlePayload redacts a payload and decides whether it stays inline.
func (w *Writer) handlePayload(ctx context.Context, s *Span, bodyKey, refKey, sizeKey string) error {
	raw, ok := payloadBytes(s.Attributes[bodyKey])
	if !ok {
		return nil
	}

	clean, fired := w.redactor.Body(raw)
	if len(fired) > 0 {
		s.Attributes[AttrRedacted] = mergeLabels(s.Attributes[AttrRedacted], fired)
	}
	s.Attributes[sizeKey] = len(clean)

	if w.blobs == nil || w.threshold <= 0 || len(clean) <= w.threshold {
		// Store as a string so the cassette stays readable. Payloads are JSON
		// or text in practice; anything genuinely binary is over the threshold
		// and goes to the blob store.
		s.Attributes[bodyKey] = string(clean)
		return nil
	}

	digest, err := w.blobs.Put(ctx, bytes.NewReader(clean))
	if err != nil {
		return fmt.Errorf("cassette: offload payload: %w", err)
	}
	delete(s.Attributes, bodyKey)
	s.Attributes[refKey] = string(digest)
	return nil
}

func payloadBytes(v any) ([]byte, bool) {
	switch b := v.(type) {
	case []byte:
		return b, true
	case string:
		return []byte(b), true
	case nil:
		return nil, false
	}
	return nil, false
}

func mergeLabels(existing any, add []string) []string {
	seen := map[string]bool{}
	var out []string
	if prev, ok := existing.([]string); ok {
		for _, s := range prev {
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	for _, s := range add {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
