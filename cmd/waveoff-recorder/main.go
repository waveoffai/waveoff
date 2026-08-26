// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

// Command waveoff-recorder is the sidecar that records what an agent did.
//
// It runs beside the agent container and proxies two planes: model traffic and
// MCP tool traffic. The injection webhook rewrites the agent's base URLs to
// point here, so the agent itself needs no change.
//
// Nothing in this binary phones home, checks for updates, or reports usage.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/waveoffai/waveoff/api/v1alpha1"
	"github.com/waveoffai/waveoff/internal/cas"
	"github.com/waveoffai/waveoff/internal/cassette"
	"github.com/waveoffai/waveoff/internal/corpus"
	"github.com/waveoffai/waveoff/internal/recorder"
)

type kvFlag map[string]string

func (k kvFlag) String() string { return fmt.Sprint(map[string]string(k)) }

func (k kvFlag) Set(v string) error {
	name, endpoint, ok := strings.Cut(v, "=")
	if !ok || name == "" || endpoint == "" {
		return fmt.Errorf("want <label>=<url>, got %q", v)
	}
	k[name] = endpoint
	return nil
}

func main() {
	var (
		listen      = flag.String("listen", "127.0.0.1:8080", "address to bind (loopback only)")
		modelUp     = flag.String("model-upstream", "", "real model provider base URL")
		corpusDir   = flag.String("corpus-dir", "/var/lib/waveoff/corpus", "where cassettes are written")
		blobDir     = flag.String("blob-dir", "/var/lib/waveoff/blobs", "content-addressed blob store directory")
		agent       = flag.String("agent", "", "logical agent name, stamped into every cassette")
		behavior    = flag.String("behavior-digest", "", "manifest behaviorDigest this agent is running")
		content     = flag.String("content-digest", "", "manifest contentDigest this agent is running")
		captureMax  = flag.Int("capture-limit", recorder.DefaultCaptureLimit, "maximum bytes captured per body")
		queueDepth  = flag.Int("queue-depth", 1024, "completed records buffered before recordings are dropped")
		idleTimeout = flag.Duration("session-idle", 2*time.Minute, "how long a session may be quiet before its cassette is closed")
		verbose     = flag.Bool("verbose", false, "debug logging")

		// Shadow deployments receive mirrored production traffic. Without
		// suppression the candidate acts on it — files the ticket, sends the
		// email — and mirroring alone stops none of that.
		shadow = flag.Bool("shadow", false,
			"refuse tool calls that would change the world; for a shadow deployment")

		// The recorder binds loopback only, on purpose: it proxies with the
		// pod's credentials, so a non-loopback bind would make it an open
		// relay. That rules out a Kubernetes HTTP readiness probe, which is
		// dialled from the kubelet's network namespace and cannot reach a
		// pod's loopback. So the binary probes itself and the pod spec uses an
		// exec probe.
		healthcheck = flag.Bool("healthcheck", false, "probe the local recorder, print the result and exit")

		// The sidecar image is distroless: no shell, no coreutils, nothing to
		// exec into. An operator who needs to know what a running recorder has
		// captured has no other way to ask, so the binary answers for itself.
		list = flag.Bool("list", false, "print the cassettes in -corpus-dir and exit")
		dump = flag.String("dump", "", "print one session's cassette from -corpus-dir and exit")

		// Export is off unless an operator asks for it. A recorder that dials
		// out to somewhere nobody configured is exactly what the no-telemetry
		// rule forbids.
		otlpEndpoint = flag.String("otlp-endpoint", os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
			"OTLP collector as host:port; empty disables export")
		otlpProtocol = flag.String("otlp-protocol", "grpc", "OTLP protocol: grpc or http")
		otlpInsecure = flag.Bool("otlp-insecure", true, "disable TLS to the collector")
	)
	tools := kvFlag{}
	flag.Var(tools, "tool-upstream", "MCP server as <label>=<url> (repeatable)")
	toolEffects := kvFlag{}
	flag.Var(toolEffects, "tool-effect",
		"tool classification as <name>=<read|idempotent-write|write> (repeatable); required with -shadow")
	flag.Parse()

	if *healthcheck {
		os.Exit(probe(*listen))
	}
	if *list {
		os.Exit(listCorpus(*corpusDir))
	}
	if *dump != "" {
		os.Exit(dumpCassette(*corpusDir, *dump))
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	if *modelUp == "" && len(tools) == 0 {
		fatal(log, "nothing to record: pass -model-upstream, -tool-upstream, or both")
	}
	// A cassette that cannot name the manifest it was recorded against cannot
	// be replayed meaningfully, because there is nothing to compare a candidate
	// to. Warn rather than refuse: a corpus recorded before manifests were
	// adopted is still worth having.
	if *behavior == "" {
		log.Warn("no -behavior-digest given; cassettes will not name the manifest they were recorded against, " +
			"which makes them unusable as a regression suite")
	}

	blobs, err := cas.NewFS(*blobDir)
	if err != nil {
		fatal(log, "blob store: %v", err)
	}
	store, err := corpus.NewFS(*corpusDir)
	if err != nil {
		fatal(log, "corpus: %v", err)
	}

	sink := recorder.NewCassetteSink(recorder.SinkConfig{
		Store: store, Blobs: blobs, Log: log,
		Agent: *agent, BehaviorDigest: *behavior, ContentDigest: *content,
		QueueDepth: *queueDepth, IdleTimeout: *idleTimeout,
	})

	// Spans go to the collector as well as into cassettes when one is
	// configured. The two answer different questions: a cassette is a portable
	// file replay reads months later, a span is live telemetry an operator
	// watches now. Neither replaces the other.
	var sinkChain recorder.Sink = sink
	ctxSetup := context.Background()
	tp, err := recorder.NewTracerProvider(ctxSetup, recorder.OTLPConfig{
		Endpoint:    *otlpEndpoint,
		Protocol:    *otlpProtocol,
		Insecure:    *otlpInsecure,
		ServiceName: serviceName(*agent),
	})
	if err != nil {
		// Export is an operator convenience. Failing to reach a collector must
		// not stop the recording that the corpus depends on.
		log.Error("OTLP export disabled", "err", err)
	} else if tp != nil {
		sinkChain = recorder.NewOTelSink(tp.Tracer("github.com/waveoffai/waveoff"), sink)
		log.Info("exporting spans", "endpoint", *otlpEndpoint, "protocol", *otlpProtocol)
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := tp.Shutdown(shutdownCtx); err != nil {
				log.Error("flushing spans to the collector", "err", err)
			}
		}()
	}

	effects, err := parseEffects(toolEffects)
	if err != nil {
		fatal(log, "%v", err)
	}
	if *shadow {
		log.Info("write suppression on", "classified_tools", len(effects))
	}

	srv, err := recorder.NewServer(recorder.Config{
		ModelUpstream:  *modelUp,
		ToolUpstreams:  tools,
		Listen:         *listen,
		Sink:           sinkChain,
		Log:            log,
		CaptureLimit:   *captureMax,
		SuppressWrites: *shadow,
		ToolEffects:    effects,
	})
	if err != nil {
		fatal(log, "%v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Info("waveoff recorder starting",
		"schema", cassette.SchemaVersion, "agent", *agent,
		"corpus", *corpusDir, "blobs", *blobDir)

	serveErr := srv.Serve(ctx)

	// Flush before exiting, or the tail of every in-flight session is lost.
	if err := sink.Close(); err != nil {
		log.Error("flushing recordings", "err", err)
	}
	written, dropped, errored := sink.Stats()
	log.Info("recorder stopped", "spans_written", written, "records_dropped", dropped, "errors", errored)
	if dropped > 0 {
		// Say it loudly: a corpus with holes nobody knows about is worse than
		// a smaller one somebody trusts.
		log.Warn("recordings were dropped; the corpus for this pod is incomplete", "dropped", dropped)
	}
	if serveErr != nil {
		fatal(log, "%v", serveErr)
	}
}

// listCorpus prints one line of JSON per recorded session.
//
// This is the only way to inspect a running recorder: the image is distroless
// by design, so there is no shell to exec into and no ls to run. A sidecar an
// operator cannot interrogate is a sidecar they stop trusting.
func listCorpus(dir string) int {
	store, err := corpus.NewFS(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	headers, err := store.List(context.Background(), corpus.Filter{})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	enc := json.NewEncoder(os.Stdout)
	for _, h := range headers {
		if err := enc.Encode(h); err != nil {
			return 1
		}
	}
	return 0
}

// dumpCassette prints one recorded session, for the same reason as listCorpus.
func dumpCassette(dir, session string) int {
	store, err := corpus.NewFS(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	f, err := store.Open(context.Background(), session)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer f.Close()
	if _, err := io.Copy(os.Stdout, f); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// probe checks a running recorder on loopback. It is the readiness probe: an
// exec probe runs inside the container, which is the only vantage point from
// which a loopback listener is reachable.
func probe(listen string) int {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + listen + "/healthz")
	if err != nil {
		fmt.Fprintf(os.Stderr, "recorder not reachable on %s: %v\n", listen, err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "recorder on %s returned %s\n", listen, resp.Status)
		return 1
	}
	fmt.Println("ok")
	return 0
}

// parseEffects turns the repeated -tool-effect flags into a classification map.
//
// An unrecognised effect is an error rather than a default. Defaulting an
// unknown value to read would let a typo turn a write tool loose on mirrored
// production traffic.
func parseEffects(raw map[string]string) (map[string]v1alpha1.ToolEffect, error) {
	out := make(map[string]v1alpha1.ToolEffect, len(raw))
	for name, value := range raw {
		switch v1alpha1.ToolEffect(value) {
		case v1alpha1.EffectRead, v1alpha1.EffectIdempotentWrite, v1alpha1.EffectWrite:
			out[name] = v1alpha1.ToolEffect(value)
		default:
			return nil, fmt.Errorf("tool %q has effect %q: use read, idempotent-write or write", name, value)
		}
	}
	return out, nil
}

// serviceName identifies this recorder in a trace backend. Naming it after the
// agent means a cluster running several agents produces several distinguishable
// services rather than one indistinguishable blob.
func serviceName(agent string) string {
	if agent == "" {
		return "waveoff-recorder"
	}
	return "waveoff-recorder/" + agent
}

func fatal(log *slog.Logger, format string, args ...any) {
	log.Error(fmt.Sprintf(format, args...))
	os.Exit(1)
}
