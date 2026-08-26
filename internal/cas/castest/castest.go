// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

// Package castest holds the conformance suite every cas.Store must pass.
//
// It lives in its own package so the filesystem and S3 backends are held to
// exactly the same contract. A store that dedupes on one backend and not the
// other, or that reports a different error for a missing blob, turns into a
// replay that behaves differently depending on where the corpus happens to
// live — which is the worst kind of bug this system can have, because it looks
// like a behavioural difference in the agent.
package castest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/waveoffai/waveoff/internal/cas"
)

// Factory builds a fresh, empty store for one test.
type Factory func(t *testing.T) cas.Store

// RunConformance exercises the whole Store contract.
func RunConformance(t *testing.T, newStore Factory) {
	t.Helper()
	t.Run("RoundTrip", func(t *testing.T) { testRoundTrip(t, newStore) })
	t.Run("ContentAddressed", func(t *testing.T) { testContentAddressed(t, newStore) })
	t.Run("Dedup", func(t *testing.T) { testDedup(t, newStore) })
	t.Run("MissingBlob", func(t *testing.T) { testMissing(t, newStore) })
	t.Run("Empty", func(t *testing.T) { testEmpty(t, newStore) })
	t.Run("Large", func(t *testing.T) { testLarge(t, newStore) })
	t.Run("Binary", func(t *testing.T) { testBinary(t, newStore) })
	t.Run("Concurrent", func(t *testing.T) { testConcurrent(t, newStore) })
	t.Run("RejectsMalformedDigest", func(t *testing.T) { testMalformed(t, newStore) })
}

func testRoundTrip(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := context.Background()
	body := []byte(`{"role":"user","content":"what is the refund policy?"}`)

	d, err := s.Put(ctx, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Validate(); err != nil {
		t.Fatalf("Put returned a malformed digest: %v", err)
	}

	got, err := cas.GetBytes(ctx, s, d, -1)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("round trip changed the content:\n want %q\n  got %q", body, got)
	}

	ok, err := s.Has(ctx, d)
	if err != nil || !ok {
		t.Errorf("Has = (%v, %v), want (true, nil)", ok, err)
	}
	size, err := s.Stat(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if size != int64(len(body)) {
		t.Errorf("Stat = %d, want %d", size, len(body))
	}
}

// testContentAddressed: the digest must be the hash of the content and nothing
// else. Replay looks blobs up by a digest recorded elsewhere, possibly by a
// different process on a different machine, so a store-local naming scheme
// would silently fail to resolve.
func testContentAddressed(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := context.Background()
	body := []byte("the same bytes")

	d, err := s.Put(ctx, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if want := cas.Compute(body); d != want {
		t.Errorf("Put returned %s, want the content hash %s", d, want)
	}
}

// testDedup is the property the corpus economics depend on: the same retrieved
// document appearing in many sessions is stored once.
func testDedup(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := context.Background()
	body := []byte("a document retrieved in five hundred sessions")

	first, err := s.Put(ctx, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		again, err := s.Put(ctx, bytes.NewReader(body))
		if err != nil {
			t.Fatalf("re-Put %d: %v", i, err)
		}
		if again != first {
			t.Fatalf("re-Put %d returned %s, want %s", i, again, first)
		}
	}
	got, err := cas.GetBytes(ctx, s, first, -1)
	if err != nil || !bytes.Equal(got, body) {
		t.Errorf("content damaged by repeated Put: %v", err)
	}
}

func testMissing(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := context.Background()
	absent := cas.Compute([]byte("never stored"))

	if _, err := s.Get(ctx, absent); !errors.Is(err, cas.ErrNotFound) {
		t.Errorf("Get on a missing blob = %v, want cas.ErrNotFound", err)
	}
	if _, err := s.Stat(ctx, absent); !errors.Is(err, cas.ErrNotFound) {
		t.Errorf("Stat on a missing blob = %v, want cas.ErrNotFound", err)
	}
	ok, err := s.Has(ctx, absent)
	if err != nil {
		t.Errorf("Has on a missing blob returned an error: %v", err)
	}
	if ok {
		t.Error("Has reported a blob that was never stored")
	}
}

// testEmpty: an empty tool result is a real recording, and it must round trip
// rather than being confused with an absent one.
func testEmpty(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := context.Background()

	d, err := s.Put(ctx, bytes.NewReader(nil))
	if err != nil {
		t.Fatal(err)
	}
	got, err := cas.GetBytes(ctx, s, d, -1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("empty blob came back as %q", got)
	}
	ok, err := s.Has(ctx, d)
	if err != nil || !ok {
		t.Errorf("an empty blob must still be present: Has = (%v, %v)", ok, err)
	}
}

// testLarge exercises the streaming path: a retrieval result or a long
// transcript is routinely larger than a comfortable buffer.
func testLarge(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := context.Background()
	body := bytes.Repeat([]byte("0123456789abcdef"), 1<<16) // 1 MiB

	d, err := s.Put(ctx, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	got, err := cas.GetBytes(ctx, s, d, -1)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("large blob round trip damaged the content (%d bytes in, %d out)", len(body), len(got))
	}

	// And the read limit must actually bound the read.
	capped, err := cas.GetBytes(ctx, s, d, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if len(capped) != 1024 {
		t.Errorf("limited read returned %d bytes, want 1024", len(capped))
	}
}

// testBinary: payloads are not all text. An image returned by a tool, or a
// gzipped resource, must survive unchanged.
func testBinary(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := context.Background()
	body := make([]byte, 256)
	for i := range body {
		body[i] = byte(i)
	}

	d, err := s.Put(ctx, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	got, err := cas.GetBytes(ctx, s, d, -1)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Error("binary content was altered in the store")
	}
}

// testConcurrent: a recorder sidecar writes from every in-flight request at
// once, and several of those are frequently the same content.
func testConcurrent(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := context.Background()
	const workers = 24

	shared := []byte("a system prompt every session sends")
	var wg sync.WaitGroup
	digests := make([]cas.Digest, workers)
	errs := make([]error, workers)

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Half write identical content, half write unique content, so the
			// test covers both the dedup race and independent writes.
			body := shared
			if i%2 == 1 {
				body = []byte(fmt.Sprintf("unique payload %d", i))
			}
			digests[i], errs[i] = s.Put(ctx, bytes.NewReader(body))
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent Put %d failed: %v", i, err)
		}
	}
	sharedDigest := cas.Compute(shared)
	for i := 0; i < workers; i += 2 {
		if digests[i] != sharedDigest {
			t.Errorf("worker %d wrote %s, want %s", i, digests[i], sharedDigest)
		}
	}
	for i := range digests {
		got, err := cas.GetBytes(ctx, s, digests[i], -1)
		if err != nil {
			t.Errorf("reading back worker %d: %v", i, err)
			continue
		}
		if cas.Compute(got) != digests[i] {
			t.Errorf("worker %d: content does not match its digest", i)
		}
	}
}

// testMalformed: digests reaching a store come out of cassettes, which are
// untrusted input. A malformed one must be refused before it becomes a
// filesystem path or an object key.
func testMalformed(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := context.Background()

	bad := []cas.Digest{
		"",
		"sha256:",
		"deadbeef",
		cas.Digest("md5:" + strings.Repeat("a", 32)),
		cas.Digest("sha256:" + strings.Repeat("a", 63)),
		cas.Digest("sha256:" + strings.Repeat("A", 64)), // uppercase
		"sha256:../../../../etc/passwd",
		cas.Digest("sha256:" + strings.Repeat("a", 62) + "/."),
	}
	for _, d := range bad {
		if _, err := s.Get(ctx, d); err == nil {
			t.Errorf("Get accepted the malformed digest %q", d)
		}
		if _, err := s.Has(ctx, d); err == nil {
			t.Errorf("Has accepted the malformed digest %q", d)
		}
	}
}
