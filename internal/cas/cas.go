// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

// Package cas is a content-addressed blob store.
//
// Cassettes reference large payloads by digest rather than embedding them, so
// the same retrieved document appearing in five hundred recorded sessions is
// stored once. That is not an optimisation detail — recording volume is the
// asset this product is built on, and a corpus that grows linearly in duplicate
// payloads stops being affordable to keep long before it stops being useful.
package cas

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ErrNotFound reports that a digest is not in the store.
var ErrNotFound = errors.New("blob not found")

// Digest is a content hash, formatted "sha256:<64 hex>". It is the only name a
// blob has: there is no path, no key, and nothing mutable to get stale.
type Digest string

// Compute returns the digest of a byte slice.
func Compute(b []byte) Digest {
	sum := sha256.Sum256(b)
	return Digest("sha256:" + hex.EncodeToString(sum[:]))
}

// Validate reports whether a digest is well formed. Anything reaching a store
// from a cassette is untrusted input: a malformed digest must be rejected
// before it is turned into a filesystem path or an object key.
func (d Digest) Validate() error {
	s := string(d)
	rest, ok := strings.CutPrefix(s, "sha256:")
	if !ok {
		return fmt.Errorf("digest %q: want a sha256: prefix", s)
	}
	if len(rest) != 64 {
		return fmt.Errorf("digest %q: want 64 hex characters, got %d", s, len(rest))
	}
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return fmt.Errorf("digest %q: %q is not lowercase hex", s, c)
		}
	}
	return nil
}

// Hex returns the digest without its algorithm prefix.
func (d Digest) Hex() string {
	return strings.TrimPrefix(string(d), "sha256:")
}

// Short returns the first n hex characters, for logs and reports.
func (d Digest) Short(n int) string {
	h := d.Hex()
	if len(h) < n {
		return h
	}
	return h[:n]
}

// Store holds blobs by content.
//
// Implementations must be safe for concurrent use: a recorder sidecar writes
// from every in-flight request at once.
type Store interface {
	// Put stores content and returns its digest. Storing content that is
	// already present is a no-op that returns the same digest, so callers do
	// not need to check first.
	Put(ctx context.Context, r io.Reader) (Digest, error)

	// Get opens a blob. It returns ErrNotFound if the digest is absent.
	Get(ctx context.Context, d Digest) (io.ReadCloser, error)

	// Has reports whether a blob is present, without transferring it.
	Has(ctx context.Context, d Digest) (bool, error)

	// Stat returns the size of a blob in bytes.
	Stat(ctx context.Context, d Digest) (int64, error)
}

// PutBytes is a convenience wrapper for callers that already hold the content.
func PutBytes(ctx context.Context, s Store, b []byte) (Digest, error) {
	return s.Put(ctx, strings.NewReader(string(b)))
}

// GetBytes reads a whole blob.
//
// limit caps how much is read, because a blob referenced by a cassette may have
// been written by a different, larger deployment than the one reading it. Pass
// a negative limit for no cap.
func GetBytes(ctx context.Context, s Store, d Digest, limit int64) ([]byte, error) {
	rc, err := s.Get(ctx, d)
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	var r io.Reader = rc
	if limit >= 0 {
		r = io.LimitReader(rc, limit)
	}
	return io.ReadAll(r)
}
