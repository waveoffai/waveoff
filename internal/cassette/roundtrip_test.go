// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package cassette_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/waveoffai/waveoff/internal/cas"
	"github.com/waveoffai/waveoff/internal/cassette"
)

func store(t *testing.T) cas.Store {
	t.Helper()
	s, err := cas.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func header() cassette.Header {
	return cassette.Header{
		SessionID:      "sess-1",
		TraceID:        "4bf92f3577b34da6a3ce929d0e0e4736",
		Agent:          "support-agent",
		BehaviorDigest: "sha256:" + strings.Repeat("a", 64),
		ContentDigest:  "sha256:" + strings.Repeat("b", 64),
		RecordedAt:     time.Date(2026, 8, 26, 9, 0, 0, 0, time.UTC),
	}
}

func TestRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	blobs := store(t)
	ctx := context.Background()

	w := cassette.NewWriter(&buf, blobs)
	if err := w.WriteHeader(header()); err != nil {
		t.Fatal(err)
	}
	in := &cassette.Span{
		TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", SpanID: "00f067aa0ba902b7",
		Name: "chat claude-sonnet-4-6", Kind: cassette.KindModel,
		StartTime: time.Now().UTC().Truncate(time.Millisecond),
		EndTime:   time.Now().UTC().Truncate(time.Millisecond),
		Attributes: map[string]any{
			"gen_ai.request.model":      "claude-sonnet-4-6",
			cassette.AttrRequestBody:    `{"messages":[{"role":"user","content":"hi"}]}`,
			cassette.AttrResponseBody:   `{"content":[{"type":"text","text":"hello"}]}`,
			cassette.AttrRequestHash:    "sha256:deadbeef",
			cassette.AttrUpstreamStatus: 200,
		},
	}
	if err := w.WriteSpan(ctx, in); err != nil {
		t.Fatal(err)
	}

	r, err := cassette.NewReader(bytes.NewReader(buf.Bytes()), blobs)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Header(); got.SessionID != "sess-1" || got.Agent != "support-agent" {
		t.Errorf("header = %+v", got)
	}
	if r.Header().SchemaVersion != cassette.SchemaVersion {
		t.Errorf("schema version = %q", r.Header().SchemaVersion)
	}
	// A cassette that cannot name its manifest cannot be replayed meaningfully.
	if r.Header().BehaviorDigest == "" {
		t.Error("the header lost the manifest digest")
	}

	spans, err := r.All()
	if err != nil {
		t.Fatal(err)
	}
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	got := spans[0]
	if got.Name != in.Name || got.Kind != cassette.KindModel {
		t.Errorf("span = %+v", got)
	}
	if idx, ok := got.StepIndex(); !ok || idx != 0 {
		t.Errorf("step index = (%d, %v), want (0, true)", idx, ok)
	}
	if got.RequestHash() != "sha256:deadbeef" {
		t.Errorf("request hash = %q", got.RequestHash())
	}

	body, err := r.Payload(ctx, got, cassette.AttrRequestBody, cassette.AttrRequestRef)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"content":"hi"`) {
		t.Errorf("request payload = %s", body)
	}
}

// TestStepIndicesAreSequential: ordering is part of the replay matching key, so
// it has to be assigned reliably and only to steps.
func TestStepIndicesAreSequential(t *testing.T) {
	var buf bytes.Buffer
	w := cassette.NewWriter(&buf, nil)
	if err := w.WriteHeader(header()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// A session span bounds the recording; it is not a step.
	if err := w.WriteSpan(ctx, &cassette.Span{Name: cassette.SpanSession, Kind: cassette.KindSession}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		kind := cassette.KindModel
		if i%2 == 1 {
			kind = cassette.KindTool
		}
		if err := w.WriteSpan(ctx, &cassette.Span{Name: "step", Kind: kind}); err != nil {
			t.Fatal(err)
		}
	}

	r, err := cassette.NewReader(bytes.NewReader(buf.Bytes()), nil)
	if err != nil {
		t.Fatal(err)
	}
	spans, err := r.All()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := spans[0].StepIndex(); ok {
		t.Error("the session span was given a step index; it bounds the recording rather than being a step in it")
	}
	for i, s := range spans[1:] {
		idx, ok := s.StepIndex()
		if !ok || idx != i {
			t.Errorf("span %d has step index (%d, %v), want (%d, true)", i, idx, ok, i)
		}
	}
}

// TestLargePayloadsAreOffloaded is the property the corpus economics rest on.
func TestLargePayloadsAreOffloaded(t *testing.T) {
	var buf bytes.Buffer
	blobs := store(t)
	ctx := context.Background()

	big := strings.Repeat("retrieved document chunk. ", 1000) // well over the threshold
	w := cassette.NewWriter(&buf, blobs)
	if err := w.WriteHeader(header()); err != nil {
		t.Fatal(err)
	}
	// The same document in three sessions' worth of spans.
	for i := 0; i < 3; i++ {
		err := w.WriteSpan(ctx, &cassette.Span{
			Name: "execute_tool docs.search", Kind: cassette.KindTool,
			Attributes: map[string]any{cassette.AttrResponseBody: big},
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	// The cassette itself must not carry the payload three times.
	if bytes.Contains(buf.Bytes(), []byte("retrieved document chunk. retrieved")) {
		t.Error("a large payload was inlined into the cassette")
	}

	r, err := cassette.NewReader(bytes.NewReader(buf.Bytes()), blobs)
	if err != nil {
		t.Fatal(err)
	}
	spans, err := r.All()
	if err != nil {
		t.Fatal(err)
	}
	refs := map[string]bool{}
	for _, s := range spans {
		ref, ok := s.BodyRef(cassette.AttrResponseRef)
		if !ok {
			t.Fatal("payload was not offloaded")
		}
		refs[string(ref)] = true

		body, err := r.Payload(ctx, s, cassette.AttrResponseBody, cassette.AttrResponseRef)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != big {
			t.Error("offloaded payload did not round trip")
		}
	}
	if len(refs) != 1 {
		t.Errorf("identical payloads produced %d distinct blobs, want 1", len(refs))
	}
}

// TestSmallPayloadsStayInline: a cassette should be readable and diffable
// without a blob store attached.
func TestSmallPayloadsStayInline(t *testing.T) {
	var buf bytes.Buffer
	w := cassette.NewWriter(&buf, store(t))
	if err := w.WriteHeader(header()); err != nil {
		t.Fatal(err)
	}
	err := w.WriteSpan(context.Background(), &cassette.Span{
		Name: "chat", Kind: cassette.KindModel,
		Attributes: map[string]any{cassette.AttrRequestBody: `{"messages":[]}`},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(buf.Bytes(), []byte(`{\"messages\":[]}`)) {
		t.Errorf("a small payload was offloaded; the cassette is no longer self-contained:\n%s", buf.String())
	}
}

// TestWriterRedactsBeforeStorage is the last line of defence: whatever the
// recorder did or failed to do, a credential must not reach durable storage.
func TestWriterRedactsBeforeStorage(t *testing.T) {
	var buf bytes.Buffer
	blobs := store(t)
	ctx := context.Background()

	secret := "sk-ant-api03-Zx8fK2mQpL9vN4bR7tY1wE5uI0oP3aS6dF"
	w := cassette.NewWriter(&buf, blobs)
	if err := w.WriteHeader(header()); err != nil {
		t.Fatal(err)
	}
	err := w.WriteSpan(ctx, &cassette.Span{
		Name: "chat", Kind: cassette.KindModel,
		Attributes: map[string]any{
			cassette.AttrRequestBody:  `{"prompt":"my key is ` + secret + `"}`,
			cassette.AttrResponseBody: strings.Repeat("x", 5000) + secret, // offloaded path
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if bytes.Contains(buf.Bytes(), []byte(secret)) {
		t.Error("a credential reached the cassette")
	}

	r, _ := cassette.NewReader(bytes.NewReader(buf.Bytes()), blobs)
	spans, err := r.All()
	if err != nil {
		t.Fatal(err)
	}
	// And the offloaded copy in the blob store must be clean too.
	body, err := r.Payload(ctx, spans[0], cassette.AttrResponseBody, cassette.AttrResponseRef)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte(secret)) {
		t.Error("a credential reached the blob store")
	}
	if _, ok := spans[0].Attributes[cassette.AttrRedacted]; !ok {
		t.Error("the cassette does not record that anything was redacted")
	}
}

// TestUnknownSchemaIsRefused: a reader that silently misinterprets an older
// attribute layout produces a replay that looks fine and is wrong.
func TestUnknownSchemaIsRefused(t *testing.T) {
	line := `{"schemaVersion":"waveoff.ai/cassette/v0","sessionId":"s"}` + "\n"
	_, err := cassette.NewReader(strings.NewReader(line), nil)
	if !errors.Is(err, cassette.ErrUnsupportedSchema) {
		t.Fatalf("err = %v, want ErrUnsupportedSchema", err)
	}
	if !strings.Contains(err.Error(), cassette.SchemaVersion) {
		t.Errorf("the error should name the version this build understands: %v", err)
	}
}

// TestMissingBlobIsReported: a replay that quietly substitutes an empty tool
// result is a replay that lies, and it would be scored as if it were real.
func TestMissingBlobIsReported(t *testing.T) {
	var buf bytes.Buffer
	writeBlobs := store(t)
	ctx := context.Background()

	w := cassette.NewWriter(&buf, writeBlobs)
	if err := w.WriteHeader(header()); err != nil {
		t.Fatal(err)
	}
	err := w.WriteSpan(ctx, &cassette.Span{
		Name: "execute_tool", Kind: cassette.KindTool,
		Attributes: map[string]any{cassette.AttrResponseBody: strings.Repeat("y", 9000)},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Read with a different, empty blob store: the corpus has lost its blobs.
	r, err := cassette.NewReader(bytes.NewReader(buf.Bytes()), store(t))
	if err != nil {
		t.Fatal(err)
	}
	spans, _ := r.All()
	_, err = r.Payload(ctx, spans[0], cassette.AttrResponseBody, cassette.AttrResponseRef)
	if err == nil {
		t.Fatal("a missing blob was reported as an empty payload")
	}
	if !errors.Is(err, cas.ErrNotFound) {
		t.Errorf("err = %v, want it to wrap cas.ErrNotFound", err)
	}
}

func TestHeaderRequiredBeforeSpans(t *testing.T) {
	w := cassette.NewWriter(&bytes.Buffer{}, nil)
	if err := w.WriteSpan(context.Background(), &cassette.Span{Kind: cassette.KindModel}); err == nil {
		t.Error("a span was written before the header")
	}
}

// TestTruncatedCassetteYieldsAPrefix: a crashed recorder should leave something
// readable, which is most of why the format is newline-delimited.
func TestTruncatedCassetteYieldsAPrefix(t *testing.T) {
	var buf bytes.Buffer
	w := cassette.NewWriter(&buf, nil)
	if err := w.WriteHeader(header()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if err := w.WriteSpan(context.Background(), &cassette.Span{Name: "step", Kind: cassette.KindModel}); err != nil {
			t.Fatal(err)
		}
	}

	full := buf.String()
	cut := strings.LastIndex(full[:len(full)-len(full)/5], "\n") + 1
	truncated := full[:cut] + `{"name":"partial","ki` // a half-written line

	r, err := cassette.NewReader(strings.NewReader(truncated), nil)
	if err != nil {
		t.Fatalf("a truncated cassette should still open: %v", err)
	}
	var good int
	for {
		_, err := r.Next()
		if err != nil {
			break
		}
		good++
	}
	if good == 0 {
		t.Error("no spans were recoverable from a truncated cassette")
	}
}
