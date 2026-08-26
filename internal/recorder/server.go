// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package recorder

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/waveoffai/waveoff/api/v1alpha1"
)

// Config describes a recorder sidecar.
type Config struct {
	// ModelUpstream is the real model provider, e.g. https://api.anthropic.com.
	ModelUpstream string
	// ToolUpstreams maps a label to an MCP server endpoint. Each is served on
	// its own path prefix, /mcp/<label>, so one sidecar can front several
	// servers without the agent needing to know it is proxied.
	ToolUpstreams map[string]string

	// Listen is the address to bind. It defaults to localhost only: a recorder
	// that accepts traffic from outside its own pod is an open relay to a model
	// provider using that pod's credentials.
	Listen string

	Sink     Sink
	Sessions *Sessions
	Log      *slog.Logger

	CaptureLimit int

	// SuppressWrites refuses tool calls that would change the world.
	//
	// Set for a shadow deployment, where the candidate receives mirrored
	// production traffic and must not act on it. Without this, mirroring alone
	// stops nothing: the candidate files the ticket, sends the email, charges
	// the card, exactly as it would in production.
	SuppressWrites bool

	// ToolEffects classifies each tool, from the manifest the agent is running.
	// Required when SuppressWrites is set: with no classification every call is
	// refused, which is safe and useless.
	ToolEffects map[string]v1alpha1.ToolEffect
}

// Server fronts both planes on one listener.
type Server struct {
	cfg  Config
	mux  *http.ServeMux
	http *http.Server
	log  *slog.Logger

	model *Proxy
	tools map[string]*Proxy

	// toolSink is the annotated sink both the tool proxies and the
	// suppressors record through.
	toolSink Sink

	// suppressors are the write-suppression layers, kept for their counters.
	suppressors []*Suppressor
}

// NewServer wires the planes.
func NewServer(cfg Config) (*Server, error) {
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:8080"
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Sink == nil {
		cfg.Sink = Discard
	}
	if cfg.Sessions == nil {
		cfg.Sessions = NewSessions(0)
	}
	if err := checkListen(cfg.Listen); err != nil {
		return nil, err
	}
	if cfg.SuppressWrites && len(cfg.ToolEffects) == 0 {
		// Refusing every call is safe and useless: the agent cannot complete a
		// single session, so the shadow stage observes nothing.
		return nil, fmt.Errorf("write suppression is on but no tool effects were given, " +
			"so every tool call would be refused and nothing would be observed")
	}

	s := &Server{cfg: cfg, mux: http.NewServeMux(), log: cfg.Log, tools: map[string]*Proxy{}}

	if cfg.ModelUpstream != "" {
		u, err := url.Parse(cfg.ModelUpstream)
		if err != nil {
			return nil, fmt.Errorf("model upstream %q: %w", cfg.ModelUpstream, err)
		}
		s.model = New(&Proxy{
			Upstream: u, Plane: PlaneModel, Sink: cfg.Sink,
			Sessions: cfg.Sessions, CaptureLimit: cfg.CaptureLimit,
		})
	}

	// Tool traffic is annotated before it reaches the sink, so tool spans carry
	// the contract the server advertised at call time.
	toolSink := Sink(NewMCPAnnotator(cfg.Sink))
	s.toolSink = toolSink
	for label, endpoint := range cfg.ToolUpstreams {
		u, err := url.Parse(endpoint)
		if err != nil {
			return nil, fmt.Errorf("tool upstream %q (%s): %w", label, endpoint, err)
		}
		s.tools[label] = New(&Proxy{
			Upstream: u, Plane: PlaneTool, Sink: toolSink,
			Sessions: cfg.Sessions, CaptureLimit: cfg.CaptureLimit,
		})
	}

	s.routes()
	s.http = &http.Server{
		Addr:    cfg.Listen,
		Handler: s.mux,
		// No write timeout: a model call streaming tokens can legitimately run
		// for minutes, and cutting it off would be the recorder changing the
		// agent's behaviour rather than observing it.
		ReadHeaderTimeout: 30 * time.Second,
	}
	return s, nil
}

func (s *Server) routes() {
	s.mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintln(w, "ok")
	})

	for label, p := range s.tools {
		prefix := "/mcp/" + label
		var handler http.Handler = p
		if s.cfg.SuppressWrites {
			// In front of the proxy, not inside it: a call that has already
			// reached the server cannot be un-made.
			sup := NewSuppressor(s.cfg.ToolEffects, p)
			// Through the same sink as everything else, so a suppressed write
			// lands in the cassette annotated with the contract the server
			// advertised. Otherwise the one class of call a shadow stage never
			// executes is also the one class drift detection never sees.
			sup.Sink = s.toolSink
			s.suppressors = append(s.suppressors, sup)
			handler = sup
		}
		// Both routes strip the prefix. The exact-match one is not optional:
		// an MCP client posts to the endpoint itself, and forwarding that
		// unstripped appends the label to the upstream's own path, which the
		// server answers with a 404 that surfaces as "session terminated"
		// somewhere far from the cause.
		stripped := http.StripPrefix(prefix, handler)
		s.mux.Handle(prefix+"/", stripped)
		s.mux.Handle(prefix, stripped)
	}

	// The model plane takes everything else, so an agent pointed at this
	// sidecar as its base URL needs no path rewriting at all.
	if s.model != nil {
		s.mux.Handle("/", s.model)
	}
}

// checkListen refuses to bind anywhere but loopback unless the operator has
// clearly asked for it.
//
// The sidecar proxies to a model provider using the pod's credentials. Bound to
// 0.0.0.0 it is an open relay for anything that can reach the pod, which in a
// flat cluster network is everything.
func checkListen(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("listen address %q: %w", addr, err)
	}
	switch host {
	case "127.0.0.1", "localhost", "::1", "":
		return nil
	}
	if strings.HasPrefix(host, "0.0.0.0") || host == "::" {
		return fmt.Errorf("refusing to listen on %q: the recorder proxies to a model provider with this "+
			"pod's credentials, so binding a non-loopback address makes it an open relay. "+
			"Bind 127.0.0.1 and let the injection webhook point the agent at it", addr)
	}
	return nil
}

// Serve runs until the context is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.cfg.Listen, err)
	}
	s.log.Info("recorder listening",
		"addr", ln.Addr().String(),
		"model_upstream", s.cfg.ModelUpstream,
		"tool_upstreams", len(s.tools))

	done := make(chan error, 1)
	go func() {
		err := s.http.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		done <- err
	}()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		// Give in-flight calls a chance to finish. A model call cut off at
		// shutdown is an error the agent sees, caused by the recorder.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return s.http.Shutdown(shutdownCtx)
	}
}

// Addr reports the configured listen address.
func (s *Server) Addr() string { return s.cfg.Listen }

// Handler exposes the mux, for tests that want to drive it without a listener.
func (s *Server) Handler() http.Handler { return s.mux }
