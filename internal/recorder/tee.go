// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package recorder

import (
	"bytes"
	"io"
	"sync"
)

// teeReader copies what it reads into a bounded buffer while passing the bytes
// straight through.
//
// The bound matters. A recorder sidecar runs beside the agent with a small
// memory limit, and a retrieval-heavy response can be tens of megabytes. An
// unbounded capture turns a large response into an OOM kill of the pod it was
// supposed to be observing — so capture stops at the limit and the record says
// it was truncated.
type teeReader struct {
	src   io.ReadCloser
	buf   bytes.Buffer
	limit int

	mu        sync.Mutex
	truncated bool
	// chunks counts Read calls that returned data, which for a server-sent
	// event stream approximates the number of events delivered.
	chunks int
}

// newTeeReader builds a tee. hint is the expected body size, from
// Content-Length where the upstream provided one.
//
// Pre-sizing matters more here than it usually would: without it a 1 MiB
// response costs roughly three times its own size in allocation as the buffer
// doubles its way up, and this runs in a sidecar sharing a small memory limit
// with the agent it is recording.
func newTeeReader(src io.ReadCloser, limit int, hint int64) *teeReader {
	t := &teeReader{src: src, limit: limit}
	if hint > 0 {
		size := hint
		if size > int64(limit) {
			size = int64(limit)
		}
		t.buf.Grow(int(size))
	}
	return t
}

func (t *teeReader) Read(p []byte) (int, error) {
	n, err := t.src.Read(p)
	if n > 0 {
		t.mu.Lock()
		t.chunks++
		if remaining := t.limit - t.buf.Len(); remaining > 0 {
			if n <= remaining {
				t.buf.Write(p[:n])
			} else {
				t.buf.Write(p[:remaining])
				t.truncated = true
			}
		} else if t.limit >= 0 {
			t.truncated = true
		}
		t.mu.Unlock()
	}
	return n, err
}

func (t *teeReader) Close() error { return t.src.Close() }

// captured returns what was seen. It is safe to call once the body is fully
// read; the lock is held because the reverse proxy's copy goroutine and the
// handler that finalises the record are not guaranteed to be the same one.
func (t *teeReader) captured() (body []byte, truncated bool, chunks int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]byte, t.buf.Len())
	copy(out, t.buf.Bytes())
	return out, t.truncated, t.chunks
}
