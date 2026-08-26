// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package recorder

import (
	"context"
	"encoding/hex"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/waveoffai/waveoff/internal/cassette"
)

// OTelSink emits every recorded call as an OpenTelemetry span.
//
// It sits in front of the cassette sink rather than replacing it, because the
// two answer different questions. A cassette is a portable file that replay
// reads months later; a span is live telemetry an operator watches now. Sending
// only spans would make replay depend on querying a trace backend, and §4.2
// requires a cassette be something you can commit to a repository or hand to a
// vendor.
//
// The spans are stitched into the agent's own trace where there is one: the
// session id is the agent's trace id, so a recorded model call appears as a
// child of whatever the agent was doing, in whatever backend the operator
// already runs.
type OTelSink struct {
	tracer trace.Tracer
	next   Sink
}

// NewOTelSink wraps a sink. next may be nil.
func NewOTelSink(tracer trace.Tracer, next Sink) *OTelSink {
	return &OTelSink{tracer: tracer, next: next}
}

// Record implements Sink.
func (o *OTelSink) Record(r *Record) {
	o.emit(r)
	if o.next != nil {
		o.next.Record(r)
	}
}

func (o *OTelSink) emit(r *Record) {
	ctx := parentContext(r.Session)

	name, _ := SpanNameAndKind(r)
	_, span := o.tracer.Start(ctx, name,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithTimestamp(r.Start),
	)

	span.SetAttributes(toKeyValues(SpanAttributes(r))...)
	if st := SpanStatus(r); st.Code == "ERROR" {
		span.SetAttributes(attribute.String("error.message", st.Message))
		span.RecordError(fmt.Errorf("%s", st.Message))
	}
	span.End(trace.WithTimestamp(r.End))
}

// parentContext reconstructs the agent's trace so recorded spans land inside
// it. When the session was synthesised — no trace context on the request — the
// span starts a trace of its own, which is the honest representation of an
// uninstrumented agent.
func parentContext(session string) context.Context {
	ctx := context.Background()
	raw, err := hex.DecodeString(session)
	if err != nil || len(raw) != 16 {
		return ctx
	}
	var tid trace.TraceID
	copy(tid[:], raw)
	if !tid.IsValid() {
		return ctx
	}
	return trace.ContextWithRemoteSpanContext(ctx, trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: tid,
		// No span id: the recorder did not observe the agent's own span, only
		// its trace. Claiming a parent span that was never seen would put a
		// dangling reference in the operator's backend.
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	}))
}

// toKeyValues converts the shared attribute map into OTel attributes.
//
// Large payload bodies are deliberately dropped rather than shipped. A cassette
// keeps them; a trace backend charges by the byte, and pushing a full context
// window into every span is how an operator ends up disabling the exporter.
func toKeyValues(m map[string]any) []attribute.KeyValue {
	out := make([]attribute.KeyValue, 0, len(m))
	for k, v := range m {
		switch k {
		case cassette.AttrRequestBody, cassette.AttrResponseBody:
			if s, ok := v.(string); ok {
				out = append(out, attribute.Int(k+"_bytes", len(s)))
			}
			continue
		}
		switch t := v.(type) {
		case string:
			out = append(out, attribute.String(k, t))
		case bool:
			out = append(out, attribute.Bool(k, t))
		case int:
			out = append(out, attribute.Int(k, t))
		case int64:
			out = append(out, attribute.Int64(k, t))
		case float64:
			out = append(out, attribute.Float64(k, t))
		}
	}
	return out
}

// OTLPConfig configures export to a collector.
type OTLPConfig struct {
	// Endpoint is host:port. Empty disables export entirely — which is the
	// default, because a recorder that dials out to somewhere the operator did
	// not configure is exactly what §13.4 forbids.
	Endpoint string
	// Protocol is "grpc" or "http".
	Protocol string
	// Insecure disables TLS, for an in-cluster collector on a private network.
	Insecure bool
	// ServiceName identifies this recorder in the backend.
	ServiceName string
	Headers     map[string]string
}

// NewTracerProvider builds an OTLP-exporting provider.
//
// Returns a nil provider and no error when no endpoint is configured, so
// callers can treat export as optional without branching on config.
func NewTracerProvider(ctx context.Context, cfg OTLPConfig) (*sdktrace.TracerProvider, error) {
	if cfg.Endpoint == "" {
		return nil, nil
	}

	var (
		exporter sdktrace.SpanExporter
		err      error
	)
	switch cfg.Protocol {
	case "", "grpc":
		opts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(cfg.Endpoint)}
		if cfg.Insecure {
			opts = append(opts, otlptracegrpc.WithInsecure())
		}
		if len(cfg.Headers) > 0 {
			opts = append(opts, otlptracegrpc.WithHeaders(cfg.Headers))
		}
		exporter, err = otlptracegrpc.New(ctx, opts...)
	case "http":
		opts := []otlptracehttp.Option{otlptracehttp.WithEndpoint(cfg.Endpoint)}
		if cfg.Insecure {
			opts = append(opts, otlptracehttp.WithInsecure())
		}
		if len(cfg.Headers) > 0 {
			opts = append(opts, otlptracehttp.WithHeaders(cfg.Headers))
		}
		exporter, err = otlptracehttp.New(ctx, opts...)
	default:
		return nil, fmt.Errorf("unknown OTLP protocol %q: use grpc or http", cfg.Protocol)
	}
	if err != nil {
		return nil, fmt.Errorf("otlp exporter: %w", err)
	}

	name := cfg.ServiceName
	if name == "" {
		name = "waveoff-recorder"
	}
	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(name),
		attribute.String("waveoff.schema_version", cassette.SchemaVersion),
	))
	if err != nil {
		return nil, err
	}

	return sdktrace.NewTracerProvider(
		// Batched: exporting on the request path would put a collector's
		// availability on the agent's latency path, which is the same mistake
		// as a blocking cassette sink.
		sdktrace.WithBatcher(exporter, sdktrace.WithBatchTimeout(5*time.Second)),
		sdktrace.WithResource(res),
	), nil
}
