// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package recorder_test

import (
	"context"
	"strings"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/waveoffai/waveoff/internal/cassette"
	"github.com/waveoffai/waveoff/internal/recorder"
)

func otelHarness(t *testing.T, next recorder.Sink) (*recorder.OTelSink, *tracetest.InMemoryExporter, func()) {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	sink := recorder.NewOTelSink(tp.Tracer("test"), next)
	return sink, exp, func() { _ = tp.Shutdown(context.Background()) }
}

// TestSpansJoinTheAgentsTrace is the payoff of exporting to a collector at all:
// a recorded model call shows up inside whatever the agent was doing, in the
// backend the operator already runs, rather than as an orphan trace.
func TestSpansJoinTheAgentsTrace(t *testing.T) {
	sink, exp, done := otelHarness(t, nil)
	defer done()

	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	sink.Record(rec(traceID, 0, `{"model":"claude-sonnet-4-6"}`, `{}`))

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	if got := spans[0].SpanContext.TraceID().String(); got != traceID {
		t.Errorf("trace id = %s, want the agent's %s", got, traceID)
	}
	if !spans[0].Parent.IsRemote() {
		t.Error("the span should hang off a remote parent, i.e. the agent's trace")
	}
	if spans[0].Name != "chat claude-sonnet-4-6" {
		t.Errorf("name = %q", spans[0].Name)
	}
}

// TestSyntheticSessionStartsItsOwnTrace: an uninstrumented agent has no trace
// to join, and inventing one would be a lie about where the call came from.
func TestSyntheticSessionStartsItsOwnTrace(t *testing.T) {
	sink, exp, done := otelHarness(t, nil)
	defer done()

	sink.Record(rec("not-a-trace-id", 0, `{"model":"m"}`, `{}`))

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d spans", len(spans))
	}
	if spans[0].Parent.IsValid() {
		t.Error("a synthetic session produced a span claiming a parent that was never observed")
	}
}

// TestBodiesAreNotShippedToTheCollector: a cassette keeps payloads, a trace
// backend charges by the byte. Pushing a full context window into every span is
// how an operator ends up turning the exporter off.
func TestBodiesAreNotShippedToTheCollector(t *testing.T) {
	sink, exp, done := otelHarness(t, nil)
	defer done()

	secret := strings.Repeat("prompt text ", 500)
	sink.Record(rec("4bf92f3577b34da6a3ce929d0e0e4736", 0, `{"model":"m","messages":"`+secret+`"}`, `{}`))

	attrs := map[string]string{}
	for _, kv := range exp.GetSpans()[0].Attributes {
		attrs[string(kv.Key)] = kv.Value.Emit()
	}
	for k, v := range attrs {
		if strings.Contains(v, "prompt text prompt text") {
			t.Errorf("attribute %q carries the payload body into the collector", k)
		}
	}
	// The size must still be reported, so an operator can see what was there.
	if _, ok := attrs[cassette.AttrRequestBody+"_bytes"]; !ok {
		t.Errorf("the request size was not reported; attributes were %v", attrs)
	}
}

// TestSemanticConventionsAreCarried: the point of using the real SDK is that
// existing backends already understand these attributes.
func TestSemanticConventionsAreCarried(t *testing.T) {
	sink, exp, done := otelHarness(t, nil)
	defer done()

	sink.Record(rec("4bf92f3577b34da6a3ce929d0e0e4736", 2, `{"model":"claude-sonnet-4-6"}`, `{}`))

	attrs := map[string]string{}
	for _, kv := range exp.GetSpans()[0].Attributes {
		attrs[string(kv.Key)] = kv.Value.Emit()
	}
	for _, want := range []string{"gen_ai.request.model", "gen_ai.operation.name", "http.request.method", "server.address"} {
		if _, ok := attrs[want]; !ok {
			t.Errorf("missing semantic convention attribute %q", want)
		}
	}
	// And the waveoff-specific ones replay needs.
	for _, want := range []string{cassette.AttrSessionID, cassette.AttrStepIndex, cassette.AttrRequestHash} {
		if _, ok := attrs[want]; !ok {
			t.Errorf("missing %q", want)
		}
	}
}

func TestToolSpansCarryMCPConventions(t *testing.T) {
	sink, exp, done := otelHarness(t, nil)
	defer done()

	r := &recorder.Record{
		Session: "4bf92f3577b34da6a3ce929d0e0e4736", Plane: recorder.PlaneTool, Step: 0,
		MCPMethod: "tools/call", ToolName: "jira.create_issue",
		ToolContractDigest: "sha256:" + strings.Repeat("c", 64),
		Status:             200,
		Start:              time.Now(), End: time.Now().Add(time.Millisecond),
	}
	sink.Record(r)

	span := exp.GetSpans()[0]
	if span.Name != "execute_tool jira.create_issue" {
		t.Errorf("name = %q", span.Name)
	}
	attrs := map[string]string{}
	for _, kv := range span.Attributes {
		attrs[string(kv.Key)] = kv.Value.Emit()
	}
	for _, want := range []string{"mcp.method.name", "mcp.tool.name", cassette.AttrToolContractDigest} {
		if _, ok := attrs[want]; !ok {
			t.Errorf("missing %q; attributes were %v", want, attrs)
		}
	}
}

// TestChainsToTheCassetteSink: exporting must not replace recording. A cassette
// is what replay reads months later; a span is what an operator watches now.
func TestChainsToTheCassetteSink(t *testing.T) {
	c := &collector{}
	sink, exp, done := otelHarness(t, c)
	defer done()

	sink.Record(rec("4bf92f3577b34da6a3ce929d0e0e4736", 0, `{"model":"m"}`, `{}`))

	if len(exp.GetSpans()) != 1 {
		t.Error("nothing was exported")
	}
	if len(c.all()) != 1 {
		t.Error("the record did not reach the cassette sink; export must not replace recording")
	}
}

func TestErrorsAreRecordedOnTheSpan(t *testing.T) {
	sink, exp, done := otelHarness(t, nil)
	defer done()

	r := rec("4bf92f3577b34da6a3ce929d0e0e4736", 0, `{"model":"m"}`, `{"error":"overloaded"}`)
	r.Status = 529
	sink.Record(r)

	span := exp.GetSpans()[0]
	if len(span.Events) == 0 {
		t.Error("no exception event was recorded for a failed upstream call")
	}
}

// TestNoEndpointMeansNoExporter: a recorder that dials out to somewhere the
// operator did not configure is exactly what §13.4 forbids.
func TestNoEndpointMeansNoExporter(t *testing.T) {
	tp, err := recorder.NewTracerProvider(context.Background(), recorder.OTLPConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if tp != nil {
		t.Error("a tracer provider was built with no endpoint configured")
	}
}

func TestUnknownProtocolIsRejected(t *testing.T) {
	_, err := recorder.NewTracerProvider(context.Background(), recorder.OTLPConfig{
		Endpoint: "localhost:4317", Protocol: "carrier-pigeon",
	})
	if err == nil {
		t.Fatal("an unknown OTLP protocol was accepted")
	}
}
