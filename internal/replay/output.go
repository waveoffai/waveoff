// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package replay

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/waveoffai/waveoff/internal/cas"
	"github.com/waveoffai/waveoff/internal/cassette"
	"github.com/waveoffai/waveoff/internal/corpus"
)

// Output records what the candidate actually produced during a replay.
//
// A divergence report says where the candidate left the recorded path. It does
// not say what the candidate said, and a judge scoring "did this complete the
// task?" needs exactly that. Without an output artifact there is nothing for a
// scorer to read, and no gate can be built on top of a replay at all.
//
// The artifact is another cassette. That is not a convenience: it means the
// output is a portable file in a format that already has a reader, a schema
// version and a redaction pass, and that a scorer written against a recording
// works unchanged against a replay.
type Output struct {
	mu     sync.Mutex
	writer *cassette.Writer
	closer io.Closer
	step   int
}

// ArmLabel names one side of a comparison.
type ArmLabel string

const (
	// ArmIncumbent is what is running now.
	ArmIncumbent ArmLabel = "incumbent"
	// ArmCandidate is what is being proposed.
	ArmCandidate ArmLabel = "candidate"
)

// OutputName is the session id a replay's output is stored under.
//
// Derived from the source session and the arm, so the two sides of a paired
// comparison sit next to each other in the corpus and neither overwrites the
// other. The pairing key is recoverable from the name alone.
func OutputName(sourceSession string, arm ArmLabel) string {
	return sourceSession + "." + string(arm)
}

// NewOutput opens an output cassette in a corpus store.
func NewOutput(ctx context.Context, store corpus.Store, header cassette.Header,
	source string, arm ArmLabel, blobs cas.Store) (*Output, error) {

	header.SessionID = OutputName(source, arm)
	header.SourceSession = source
	header.Arm = string(arm)
	header.RecordedAt = time.Now().UTC()
	header.Recorder = "waveoff-replay"

	w, err := store.Create(ctx, header)
	if err != nil {
		return nil, fmt.Errorf("open replay output: %w", err)
	}
	cw := cassette.NewWriter(w, blobs)
	if err := cw.AdoptHeader(); err != nil {
		_ = w.Close()
		return nil, err
	}
	return &Output{writer: cw, closer: w}, nil
}

// Write appends one replayed exchange.
func (o *Output) Write(ctx context.Context, req Request, decision Decision, exchange *Exchange) error {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()

	kind := cassette.KindModel
	name := cassette.OpChat
	if req.Kind == cassette.KindTool {
		kind = cassette.KindTool
		name = cassette.OpTool
		if req.Tool != "" {
			name += " " + req.Tool
		}
	}

	attrs := map[string]any{
		cassette.AttrStepIndex:      o.step,
		cassette.AttrRequestBody:    string(exchange.Request),
		cassette.AttrResponseBody:   string(exchange.Response),
		cassette.AttrRequestHash:    req.Hash,
		cassette.AttrUpstreamStatus: exchange.Status,
		// How this step was handled is part of the evidence. A score computed
		// over a session where half the tools were no-op'd means something
		// different from one where they all ran, and a scorer that cannot see
		// the difference will report both the same.
		"waveoff.replay.action": string(decision.Action),
		"waveoff.replay.reason": decision.Reason,
		"waveoff.replay.match":  string(exchange.Match),
	}
	if req.Tool != "" {
		attrs["mcp.tool.name"] = req.Tool
		attrs["gen_ai.tool.name"] = req.Tool
	}
	if req.ArgsHash != "" {
		attrs[cassette.AttrToolArgsHash] = req.ArgsHash
	}
	o.step++

	return o.writer.WriteSpan(ctx, &cassette.Span{
		Name:       name,
		Kind:       kind,
		StartTime:  exchange.Start.UTC(),
		EndTime:    exchange.End.UTC(),
		Attributes: attrs,
		Status:     exchangeStatus(exchange),
	})
}

// Close finalises the output cassette.
func (o *Output) Close() error {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closer == nil {
		return nil
	}
	err := o.closer.Close()
	o.closer = nil
	return err
}

// Exchange is one request and whatever the replayer answered with.
type Exchange struct {
	Request  []byte
	Response []byte
	Status   int
	Match    MatchKind
	Start    time.Time
	End      time.Time
}

func exchangeStatus(e *Exchange) cassette.Status {
	if e.Status >= 400 {
		return cassette.Status{Code: "ERROR", Message: fmt.Sprintf("replay returned %d", e.Status)}
	}
	return cassette.Status{}
}

// capturingWriter tees a response so the replayer can record what it sent
// without buffering it first. The agent sees bytes as they are written; the
// copy is what the output cassette keeps.
type capturingWriter struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
	limit  int
}

func newCapturingWriter(w http.ResponseWriter, limit int) *capturingWriter {
	return &capturingWriter{ResponseWriter: w, status: http.StatusOK, limit: limit}
}

func (c *capturingWriter) WriteHeader(status int) {
	c.status = status
	c.ResponseWriter.WriteHeader(status)
}

func (c *capturingWriter) Write(p []byte) (int, error) {
	if remaining := c.limit - c.body.Len(); remaining > 0 {
		if len(p) <= remaining {
			c.body.Write(p)
		} else {
			c.body.Write(p[:remaining])
		}
	}
	return c.ResponseWriter.Write(p)
}

// Flush passes through, so a streamed response still reaches the agent chunk by
// chunk. Without this the replayer buffers what the recorder was careful not
// to, and the agent behaves differently under replay than under recording —
// which would make every comparison meaningless.
func (c *capturingWriter) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (c *capturingWriter) captured() []byte {
	out := make([]byte, c.body.Len())
	copy(out, c.body.Bytes())
	return out
}
