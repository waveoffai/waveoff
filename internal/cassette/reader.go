// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package cassette

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/waveoffai/waveoff/internal/cas"
)

// ErrUnsupportedSchema reports a cassette written by a version this build does
// not understand.
var ErrUnsupportedSchema = errors.New("unsupported cassette schema")

// maxLine bounds a single NDJSON line. A cassette is untrusted input — it may
// have come from another cluster, or from a vendor — so a malformed one must
// fail rather than exhaust memory.
const maxLine = 64 << 20 // 64 MiB

// Reader parses a cassette.
type Reader struct {
	sc     *bufio.Scanner
	header Header
	read   bool
	blobs  cas.Store
}

// NewReader reads the header immediately, so a caller learns about an
// unreadable or wrong-schema cassette before iterating.
//
// blobs may be nil; payload resolution then fails for offloaded payloads
// rather than silently returning nothing, because a replay that quietly
// substitutes an empty tool result is a replay that lies.
func NewReader(r io.Reader, blobs cas.Store) (*Reader, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxLine)

	cr := &Reader{sc: sc, blobs: blobs}
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return nil, fmt.Errorf("cassette: read header: %w", err)
		}
		return nil, errors.New("cassette: empty")
	}
	if err := json.Unmarshal(sc.Bytes(), &cr.header); err != nil {
		return nil, fmt.Errorf("cassette: parse header: %w", err)
	}
	if cr.header.SchemaVersion != SchemaVersion {
		// Refusing beats guessing. A reader that silently misinterprets an
		// older attribute layout produces a replay that looks fine and is
		// wrong, and a corpus that rots invisibly is worse than no corpus.
		return nil, fmt.Errorf("%w: cassette is %q, this build understands %q",
			ErrUnsupportedSchema, cr.header.SchemaVersion, SchemaVersion)
	}
	cr.read = true
	return cr, nil
}

// Header returns the cassette header.
func (r *Reader) Header() Header { return r.header }

// Next returns the next span, or io.EOF.
func (r *Reader) Next() (*Span, error) {
	for r.sc.Scan() {
		line := r.sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var s Span
		if err := json.Unmarshal(line, &s); err != nil {
			return nil, fmt.Errorf("cassette: parse span: %w", err)
		}
		return &s, nil
	}
	if err := r.sc.Err(); err != nil {
		return nil, fmt.Errorf("cassette: read: %w", err)
	}
	return nil, io.EOF
}

// All reads the rest of the cassette into memory.
func (r *Reader) All() ([]*Span, error) {
	var out []*Span
	for {
		s, err := r.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
}

// Payload resolves a recorded payload, following a blob reference when the
// value was offloaded.
//
// bodyKey and refKey are the inline and reference attribute names, for example
// AttrRequestBody and AttrRequestRef.
func (r *Reader) Payload(ctx context.Context, s *Span, bodyKey, refKey string) ([]byte, error) {
	if v, ok := s.Attributes[bodyKey]; ok {
		b, _ := payloadBytes(v)
		return b, nil
	}
	ref, ok := s.BodyRef(refKey)
	if !ok {
		return nil, nil
	}
	if r.blobs == nil {
		return nil, fmt.Errorf("cassette: payload %s is in the blob store, but no store was provided", ref.Short(12))
	}
	body, err := cas.GetBytes(ctx, r.blobs, ref, -1)
	if err != nil {
		// Distinguishable on purpose: a missing blob is a corpus integrity
		// problem, and replay must report it rather than treat it as an empty
		// response and score the result.
		return nil, fmt.Errorf("cassette: resolve payload %s: %w", ref.Short(12), err)
	}
	return body, nil
}
