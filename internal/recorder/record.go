// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

// Package recorder is the sidecar that captures what an agent actually did.
//
// It sits on two chokepoints in one process: the model plane, as an
// OpenAI/Anthropic-compatible HTTP proxy, and the tool plane, as an MCP proxy.
// Both are reverse proxies rather than interceptors, so the agent needs no code
// change — the injection webhook rewrites its base URLs to localhost.
//
// The design constraint that shapes everything here is latency. A recorder that
// adds meaningful overhead to every model call will not be adopted, so nothing
// on the request path waits for a recording to be written: bodies are teed
// while they stream to the client, and the completed record is handed to an
// asynchronous sink. If the sink cannot keep up, recordings are dropped and
// counted rather than allowed to slow the agent down.
package recorder

import (
	"net/http"
	"time"
)

// Plane says which chokepoint a record came from.
type Plane string

const (
	// PlaneModel is traffic to a model provider.
	PlaneModel Plane = "model"
	// PlaneTool is traffic to an MCP server.
	PlaneTool Plane = "tool"
)

// Record is one proxied call, captured.
//
// It is deliberately a flat struct of already-copied values: it outlives the
// request it came from, and a field still pointing into a live http.Request
// would be a use-after-free waiting to happen once the sink is asynchronous.
type Record struct {
	Session string
	Plane   Plane
	Step    int

	Method string
	URL    string
	// Target is the upstream this was proxied to, recorded so a cassette can
	// tell which MCP server or model endpoint served a call.
	Target string

	// ReqHeader is already redacted: credential-bearing headers were replaced
	// before this record existed. ReqRedacted names what was removed, so a
	// reader can tell a stripped header from one that was never sent.
	ReqHeader   http.Header
	ReqRedacted []string
	ReqBody     []byte
	// ReqTruncated reports that the captured body is shorter than what was
	// sent. Better to say so than to record a prefix as though it were whole.
	ReqTruncated bool

	Status       int
	RespHeader   http.Header
	RespRedacted []string
	RespBody     []byte

	// RespTruncated reports that the captured response is shorter than what
	// was delivered to the agent.
	RespTruncated bool

	// Streamed and Chunks describe a server-sent-event response. Replay has to
	// reproduce the streaming shape, because an agent that sees one chunk
	// instead of forty behaves differently for reasons unrelated to the model.
	Streamed bool
	Chunks   int

	Start    time.Time
	End      time.Time
	Upstream time.Duration

	// MCP-plane detail, filled in by MCPAnnotator. Empty on the model plane.
	MCPMethod string
	ToolName  string
	// ToolContractDigest is the contract the server advertised for this tool,
	// as of the most recent tools/list in the same session. Comparing it
	// against the live contract at replay time is what stops a corpus rotting
	// invisibly.
	ToolContractDigest string
	// ToolArgsHash is the normalised hash of the call's arguments. Replay uses
	// it to find this call again once the model runs live and step indices no
	// longer line up.
	ToolArgsHash string
	// ToolEffect is the manifest's classification of this tool, recorded when
	// the suppressor acted on it.
	ToolEffect string
	// Suppressed marks a write answered without being executed. The request is
	// real — the agent asked for it — and only the effect is missing, which is
	// why it is recorded like any other step rather than logged and forgotten.
	Suppressed bool
	// Refused marks a call rejected because the manifest does not classify the
	// tool. Kept apart from Suppressed: one is evidence about the candidate,
	// the other is evidence about the manifest.
	Refused bool

	// Err is set when the upstream call itself failed. A failed call is still a
	// recording: "the provider 500ed here" is exactly the kind of thing a
	// replay needs to reproduce.
	Err error
}

// Duration is the wall time the proxied call took, end to end.
func (r *Record) Duration() time.Duration { return r.End.Sub(r.Start) }

// Sink consumes completed records.
//
// Implementations must not block: Record is called on the request path, after
// the response has been delivered but before the handler returns. A sink that
// blocks turns into latency on every model call, which is the one thing this
// package cannot afford.
type Sink interface {
	Record(*Record)
}

// SinkFunc adapts a function to Sink.
type SinkFunc func(*Record)

// Record implements Sink.
func (f SinkFunc) Record(r *Record) { f(r) }

// Discard drops everything. Used to measure the proxy's own cost with recording
// switched off, so the benchmark can attribute overhead honestly.
var Discard Sink = SinkFunc(func(*Record) {})
