// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

// Package cassette is the recorded form of an agent session.
//
// A cassette is an OpenTelemetry trace plus references into a content-addressed
// blob store. Spans carry structure and ordering; payloads above a threshold are
// hashed, stored once, and referenced by digest, so the same retrieved document
// appearing in five hundred sessions costs one copy.
//
// Two properties are load-bearing and easy to lose:
//
//   - A cassette is a portable file. It can be committed to a repository,
//     attached to a bug report, or handed to an eval vendor without dragging a
//     trace backend along with it. Spans are exported to a collector as well
//     when one is configured, but replay never depends on one being there.
//   - The schema is versioned from the first line written. A breaking change
//     that silently invalidates a historical corpus is the failure mode that
//     kills this product, so readers refuse a version they do not understand
//     rather than guessing.
package cassette

import (
	"time"

	"github.com/waveoffai/waveoff/internal/cas"
)

// SchemaVersion identifies the cassette attribute schema.
//
// It is written into every cassette header and checked on read. Bump it only
// for a change that a reader of the previous version could misinterpret;
// additive attributes do not need one.
const SchemaVersion = "waveoff.ai/cassette/v1alpha1"

// InlineThreshold is how large a payload may be before it is offloaded to the
// blob store instead of being written into the span.
//
// Small payloads stay inline so a cassette can be read, diffed and understood
// without a blob store attached. Large ones are offloaded so a corpus does not
// grow linearly in duplicated context windows.
const InlineThreshold = 4 << 10 // 4 KiB

// Attribute keys. The waveoff.* prefix is reserved for attributes this system
// defines; everything describing the model or tool call itself uses the
// OpenTelemetry GenAI and MCP semantic conventions instead of being reinvented.
const (
	AttrSchemaVersion = "waveoff.schema_version"
	AttrSessionID     = "waveoff.session.id"
	AttrStepIndex     = "waveoff.step.index"

	// AttrBehaviorDigest and AttrContentDigest pin the manifest this session
	// ran under. Without them a corpus is a pile of traffic that cannot be
	// attributed to a version, which makes it useless as a regression suite.
	AttrBehaviorDigest = "waveoff.manifest.behavior_digest"
	AttrContentDigest  = "waveoff.manifest.content_digest"
	AttrAgent          = "waveoff.agent"

	// AttrRequestHash is the replay matching key: a hash of the request after
	// normalisation, so a replay can find the recorded response for a request
	// that is semantically the same but not byte identical.
	AttrRequestHash = "waveoff.request.hash"

	// Payload references. Each pair is either an inline value or a blob digest,
	// never both.
	AttrRequestBody   = "waveoff.request.body"
	AttrRequestRef    = "waveoff.request.body_ref"
	AttrResponseBody  = "waveoff.response.body"
	AttrResponseRef   = "waveoff.response.body_ref"
	AttrRequestBytes  = "waveoff.request.bytes"
	AttrResponseBytes = "waveoff.response.bytes"

	// Headers, already redacted. A provider reports the resolved model version
	// in a response header and §5 pins it, so dropping headers would leave the
	// manifest unable to record what actually served a call.
	AttrRequestHeaders  = "waveoff.request.headers"
	AttrResponseHeaders = "waveoff.response.headers"

	// AttrToolContractDigest records the tool contract as the server advertised
	// it at call time. Comparing it against the live contract during replay is
	// what turns a silently rotting corpus into a reported staleness.
	AttrToolContractDigest = "waveoff.tool.contract_digest"
	AttrToolEffect         = "waveoff.tool.effect"
	// AttrToolSuppressed marks a tool call that was answered without being
	// executed, because the session was shadow traffic and the tool writes.
	//
	// These are the most informative steps a shadow session produces. The
	// incumbent's writes happened and the candidate's did not, so nothing
	// downstream can compare their effects — but the attempts themselves are
	// directly comparable, and a candidate reaching for a write the incumbent
	// never makes is a finding that needs no judge.
	AttrToolSuppressed = "waveoff.tool.suppressed"
	// AttrToolRefused marks a tool call rejected because the manifest does not
	// classify the tool.
	//
	// Deliberately not the same attribute as a suppression. A suppressed write
	// is a candidate trying to do something and is evidence about the
	// candidate; a refusal is a manifest that does not describe the tools the
	// agent actually has, and is evidence about the manifest. They need
	// different fixes, so a reader has to be able to tell them apart.
	AttrToolRefused = "waveoff.tool.refused"
	// AttrToolArgsHash identifies a tool call by what it asked for, which is
	// how replay matches once the model runs live and step ordering shifts.
	AttrToolArgsHash = "waveoff.tool.args_hash"

	// AttrRedacted lists the fields stripped before the cassette was written,
	// so a reader can tell a redacted value from one that was never present.
	AttrRedacted = "waveoff.redacted"

	// AttrUpstreamStatus and AttrUpstreamDurationMS describe the call the
	// recorder proxied, as distinct from the span's own timing.
	AttrUpstreamStatus     = "waveoff.upstream.status_code"
	AttrUpstreamDurationMS = "waveoff.upstream.duration_ms"

	// AttrStreamed marks a model response delivered as a token stream, which
	// replay has to reproduce in the same shape or the agent behaves
	// differently for reasons that have nothing to do with the model.
	AttrStreamed     = "waveoff.response.streamed"
	AttrStreamChunks = "waveoff.response.stream_chunks"
)

// Span names. Model and tool spans follow the GenAI convention of
// "<operation> <target>"; the session span is ours.
const (
	SpanSession = "waveoff.session"
	OpChat      = "chat"
	OpTool      = "execute_tool"
)

// Kind classifies a recorded span for replay, which cares about what a step
// *is* rather than what it was called.
type Kind string

const (
	// KindSession is the root span for one agent invocation.
	KindSession Kind = "session"
	// KindModel is a call to a model provider.
	KindModel Kind = "model"
	// KindTool is a tool call, whether or not it went through MCP.
	KindTool Kind = "tool"
	// KindDecision is an agent-internal decision point, consumed from the
	// agent's own instrumentation. Without these, divergence detection can only
	// compare final answers, not the path taken to reach them.
	KindDecision Kind = "decision"
)

// AttrKind carries Kind on a span.
const AttrKind = "waveoff.kind"

// Header is the first line of a cassette file.
type Header struct {
	SchemaVersion string `json:"schemaVersion"`
	SessionID     string `json:"sessionId"`
	TraceID       string `json:"traceId,omitempty"`
	Agent         string `json:"agent,omitempty"`

	// BehaviorDigest and ContentDigest pin the manifest this session ran
	// against. A cassette that cannot name its manifest cannot be replayed
	// meaningfully, because there is nothing to compare a candidate to.
	BehaviorDigest string `json:"behaviorDigest,omitempty"`
	ContentDigest  string `json:"contentDigest,omitempty"`

	RecordedAt time.Time `json:"recordedAt"`

	// Recorder identifies what produced this cassette, so a corpus recorded
	// across a recorder upgrade can be reasoned about.
	Recorder string `json:"recorder,omitempty"`

	// SourceSession and Arm are set only on a replay's own output.
	//
	// A replay produces a cassette of what the candidate actually did, which is
	// the artifact a scorer reads. SourceSession is the recorded session it was
	// driven from — and therefore the pairing key, since both arms of a
	// comparison derive from the same one. Arm says which side this is.
	//
	// Both are additive: a reader of an ordinary recording simply finds them
	// empty, so they do not need a schema version bump.
	SourceSession string `json:"sourceSession,omitempty"`
	Arm           string `json:"arm,omitempty"`
}

// Span is one recorded event.
//
// It is shaped after an OpenTelemetry span rather than being one, so that a
// cassette stays a self-describing file. The fields map onto OTLP directly for
// anyone who wants to load a cassette into a trace backend.
type Span struct {
	TraceID    string         `json:"traceId"`
	SpanID     string         `json:"spanId"`
	ParentID   string         `json:"parentSpanId,omitempty"`
	Name       string         `json:"name"`
	Kind       Kind           `json:"kind"`
	StartTime  time.Time      `json:"startTime"`
	EndTime    time.Time      `json:"endTime"`
	Attributes map[string]any `json:"attributes,omitempty"`
	Status     Status         `json:"status,omitempty"`
	Events     []Event        `json:"events,omitempty"`
}

// Status mirrors the OTel span status.
type Status struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// Event is a point-in-time record within a span, used for stream chunks and
// anything else that happens during a call rather than bounding it.
type Event struct {
	Name       string         `json:"name"`
	Time       time.Time      `json:"time"`
	Attributes map[string]any `json:"attributes,omitempty"`
}

// StepIndex returns the recorded step index, and whether one was present.
// Ordering is part of the replay matching key, so a span without one cannot
// participate in replay.
func (s *Span) StepIndex() (int, bool) {
	v, ok := s.Attributes[AttrStepIndex]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64: // survives a JSON round trip
		return int(n), true
	}
	return 0, false
}

// String returns a string attribute.
func (s *Span) String(key string) string {
	if v, ok := s.Attributes[key].(string); ok {
		return v
	}
	return ""
}

// RequestHash is the normalised request hash used to match a replayed request
// against this recording.
func (s *Span) RequestHash() string { return s.String(AttrRequestHash) }

// BodyRef returns the blob digest for an offloaded payload, if there is one.
func (s *Span) BodyRef(refKey string) (cas.Digest, bool) {
	d := cas.Digest(s.String(refKey))
	if d == "" {
		return "", false
	}
	return d, true
}
