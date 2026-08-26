// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package replay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/waveoffai/waveoff/internal/cassette"
	"github.com/waveoffai/waveoff/internal/jsonrpc"
	"github.com/waveoffai/waveoff/internal/recorder"
)

// Server answers an agent's calls from a cassette.
//
// It presents exactly the surface the recorder does, so an agent pointed at it
// cannot tell the difference. That is what makes a replay a test of the agent
// rather than of the harness: nothing in the agent has to know it is being
// replayed, and nothing about it changes because it is.
type Server struct {
	mu      sync.Mutex
	idx     *Index
	policy  *Policy
	tracker *Tracker
	reader  *cassette.Reader

	// modelUpstream is where model traffic goes when the mode runs it live.
	modelUpstream *url.URL
	// toolUpstreams maps a label to an MCP endpoint, for forked reads.
	toolUpstreams map[string]*url.URL

	// output records what the candidate produced, which is the artifact a
	// scorer reads. Optional: a divergence-only run needs no output.
	output *Output

	mux   *http.ServeMux
	proxy *httputil.ReverseProxy

	// servedProtocol tracks which recorded handshake exchanges have been
	// handed out.
	servedProtocol map[string]bool
}

// ServerConfig configures a replay server.
type ServerConfig struct {
	Index   *Index
	Reader  *cassette.Reader
	Policy  *Policy
	Tracker *Tracker

	// ModelUpstream is required for modes that run the model live.
	ModelUpstream string
	// ToolUpstreams are required for hybrid mode's forked reads.
	ToolUpstreams map[string]string

	// Output, when set, records the replay as a cassette of its own. That is
	// what a scorer reads: a divergence report says where the candidate left
	// the path, not what it produced.
	Output *Output
}

// NewServer builds a replay server.
func NewServer(cfg ServerConfig) (*Server, error) {
	s := &Server{
		idx: cfg.Index, policy: cfg.Policy, tracker: cfg.Tracker,
		reader: cfg.Reader, output: cfg.Output, mux: http.NewServeMux(),
		toolUpstreams:  map[string]*url.URL{},
		servedProtocol: map[string]bool{},
	}

	if cfg.ModelUpstream != "" {
		u, err := url.Parse(cfg.ModelUpstream)
		if err != nil {
			return nil, fmt.Errorf("model upstream %q: %w", cfg.ModelUpstream, err)
		}
		s.modelUpstream = u
	}
	if s.policy.Mode == ModeModelLiveToolsReplayed && s.modelUpstream == nil {
		return nil, fmt.Errorf("mode %s runs the model live, so a model upstream is required", s.policy.Mode)
	}
	for label, endpoint := range cfg.ToolUpstreams {
		u, err := url.Parse(endpoint)
		if err != nil {
			return nil, fmt.Errorf("tool upstream %q: %w", label, err)
		}
		s.toolUpstreams[label] = u
	}

	s.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			target := s.modelUpstream
			if t, ok := pr.In.Context().Value(upstreamKey{}).(*url.URL); ok {
				target = t
			}
			pr.SetURL(target)
			pr.Out.Host = target.Host
		},
		FlushInterval: -1,
	}

	s.mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	s.mux.Handle("/mcp/", http.HandlerFunc(s.serveTool))
	s.mux.Handle("/", http.HandlerFunc(s.serveModel))
	return s, nil
}

type upstreamKey struct{}

// Handler exposes the routes.
func (s *Server) Handler() http.Handler { return s.mux }

// Report returns the replay report so far.
func (s *Server) Report() *Report { return s.tracker.Report() }

// Finish closes the report.
func (s *Server) Finish() *Report {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tracker.Finish()
}

func (s *Server) serveModel(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = r.Body.Close()

	req := Request{
		Kind: cassette.KindModel,
		Hash: recorder.NormalisedHash(body),
	}
	s.handle(w, r, req, body)
}

func (s *Server) serveTool(w http.ResponseWriter, r *http.Request) {
	// The streamable HTTP transport uses more than POST.
	//
	// A GET opens the server-to-client stream. During a replay nothing is
	// pushing messages, and holding the request open leaves the client waiting
	// forever — a hang that looks like the agent misbehaving. The specification
	// allows a server to decline the stream with 405, and clients carry on
	// without it, so that is the honest answer here.
	//
	// A DELETE terminates the session, which for a replay is a no-op.
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("X-Waveoff-Replay", string(ActionServe))
		w.Header().Set("Allow", "POST, DELETE")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	case http.MethodDelete:
		w.Header().Set("X-Waveoff-Replay", string(ActionServe))
		w.WriteHeader(http.StatusNoContent)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = r.Body.Close()

	var rpc struct {
		Method string `json:"method"`
		Params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"params"`
	}
	_ = json.Unmarshal(body, &rpc)

	req := Request{
		Kind:     cassette.KindTool,
		Hash:     recorder.NormalisedHash(body),
		Tool:     rpc.Params.Name,
		ArgsHash: recorder.NormalisedHash(rpc.Params.Arguments),
	}
	// Only tools/call is a step. Everything else on this plane — the
	// initialize handshake, the initialized notification, discovery, pings —
	// is protocol chatter the client must perform before it can call anything.
	//
	// Running it through the effect policy refuses it as a tool with no
	// asserted effect, which is right for a tool and wrong for a handshake;
	// counting it as a step would offset every subsequent index against the
	// recording. Both are silent failures that look like the candidate
	// misbehaving.
	if rpc.Method != "tools/call" {
		s.serveProtocol(w, r, rpc.Method, body)
		return
	}
	s.handle(w, r, req, body)
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request, req Request, body []byte) {
	s.mu.Lock()
	req.Step = s.tracker.next
	match := s.idx.Lookup(req)
	decision := s.policy.Decide(req, match)
	s.tracker.Observe(req, match, decision)
	s.mu.Unlock()

	start := time.Now()
	// Tee the response rather than buffer it: the agent must see a streamed
	// answer arrive chunk by chunk under replay exactly as it did under
	// recording, or the two runs are not comparable.
	capture := newCapturingWriter(w, outputCaptureLimit)

	switch decision.Action {
	case ActionServe:
		s.serveRecorded(capture, match, decision, jsonrpc.RequestID(body))
	case ActionForward:
		s.forward(capture, r, req, body)
	case ActionNoOp:
		s.noOp(capture, req, decision, body)
	default:
		s.refuse(capture, decision)
	}

	if s.output != nil {
		_ = s.output.Write(r.Context(), req, decision, &Exchange{
			Request:  body,
			Response: capture.captured(),
			Status:   capture.status,
			Match:    match.Kind,
			Start:    start,
			End:      time.Now(),
		})
	}
}

// outputCaptureLimit bounds what the replay output keeps per exchange, on the
// same reasoning as the recorder's: a replay runs beside the thing it is
// measuring and must not be the reason it runs out of memory.
const outputCaptureLimit = 8 << 20

// serveRecorded answers from the cassette.
func (s *Server) serveRecorded(w http.ResponseWriter, match Match, decision Decision, id json.RawMessage) {
	body, err := s.reader.Payload(context.Background(), match.Span,
		cassette.AttrResponseBody, cassette.AttrResponseRef)
	if err != nil {
		// A missing blob is a corpus integrity problem, not an agent
		// behaviour. Reporting it as a 502 rather than an empty body keeps a
		// broken corpus from being scored as a bad candidate.
		s.fail(w, http.StatusBadGateway, fmt.Sprintf("cassette is incomplete: %v", err))
		return
	}

	// The recorded response carries the id of the request that produced it.
	// The client replaying now issued a different one, and correlates by id.
	body = jsonrpc.RewriteID(body, id)

	w.Header().Set("Content-Type", contentTypeOf(match.Span))
	w.Header().Set("X-Waveoff-Replay", string(decision.Action))
	w.Header().Set("X-Waveoff-Match", string(match.Kind))
	if status := statusOf(match.Span); status > 0 {
		// A recorded failure is replayed as a failure. "The provider 529ed
		// here" is exactly the kind of thing an agent's retry path needs to
		// meet again.
		w.WriteHeader(status)
	}
	_, _ = w.Write(body)
}

func (s *Server) forward(w http.ResponseWriter, r *http.Request, req Request, body []byte) {
	target := s.modelUpstream
	if req.Kind == cassette.KindTool {
		label := toolLabel(r.URL.Path)
		u, ok := s.toolUpstreams[label]
		if !ok {
			s.fail(w, http.StatusBadGateway, fmt.Sprintf("no live endpoint configured for tool server %q", label))
			return
		}
		target = u
		r.URL.Path = strings.TrimPrefix(r.URL.Path, "/mcp/"+label)
	}
	if target == nil {
		s.fail(w, http.StatusBadGateway, "no upstream configured for a request the mode runs live")
		return
	}

	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	w.Header().Set("X-Waveoff-Replay", string(ActionForward))
	s.proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), upstreamKey{}, target)))
}

// noOp refuses a write and synthesises a plausible success.
//
// This is the property that makes evaluating a candidate against production
// traffic safe: a replayed session can call a ticket-creating tool as many
// times as it likes and no ticket is ever created.
func (s *Server) noOp(w http.ResponseWriter, req Request, decision Decision, reqBody []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Waveoff-Replay", string(ActionNoOp))
	w.WriteHeader(http.StatusOK)

	// Shaped like an MCP tool result, and honest about being synthetic: an
	// agent that reads the text will see that nothing happened.
	response := map[string]any{
		"jsonrpc": "2.0",
		"result": map[string]any{
			"content": []any{map[string]any{
				"type": "text",
				"text": fmt.Sprintf("[waveoff] %s was not executed: %s", req.Tool, decision.Reason),
			}},
			"isError": false,
		},
	}
	if id := jsonrpc.RequestID(reqBody); len(id) > 0 {
		response["id"] = id
	}
	_ = json.NewEncoder(w).Encode(response)
}

func (s *Server) refuse(w http.ResponseWriter, decision Decision) {
	s.fail(w, http.StatusConflict, decision.Reason)
}

func (s *Server) fail(w http.ResponseWriter, status int, reason string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Waveoff-Replay", string(ActionRefuse))
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"type": "waveoff_replay_refused", "message": reason},
	})
}

// serveProtocol answers MCP protocol chatter from the recording.
//
// The response is found by method rather than by request hash: a client
// generates a fresh session on every run, so the bytes differ even when the
// exchange is identical. What has to be reproduced is the shape of the
// handshake, not its request ids.
func (s *Server) serveProtocol(w http.ResponseWriter, r *http.Request, method string, body []byte) {
	s.mu.Lock()
	span := s.protocolSpan(method)
	s.mu.Unlock()

	if span != nil {
		recorded, err := s.reader.Payload(context.Background(), span,
			cassette.AttrResponseBody, cassette.AttrResponseRef)
		if err == nil {
			recorded = jsonrpc.RewriteID(recorded, jsonrpc.RequestID(body))
			w.Header().Set("Content-Type", contentTypeOf(span))
			w.Header().Set("X-Waveoff-Replay", string(ActionServe))
			w.Header().Set("X-Waveoff-Protocol", method)
			// A session id the client can echo back. The recorded one belongs
			// to a session that no longer exists, and reusing it would have a
			// live server reject the client mid-conversation.
			if w.Header().Get("Mcp-Session-Id") == "" {
				w.Header().Set("Mcp-Session-Id", "waveoff-replay")
			}
			if status := statusOf(span); status > 0 {
				w.WriteHeader(status)
			}
			_, _ = w.Write(recorded)
			return
		}
	}

	// A notification expects no body, so an empty 202 is a correct answer even
	// with nothing recorded.
	if strings.HasPrefix(method, "notifications/") {
		w.Header().Set("X-Waveoff-Replay", string(ActionServe))
		w.WriteHeader(http.StatusAccepted)
		return
	}

	if len(s.toolUpstreams) > 0 {
		s.forward(w, r, Request{Kind: cassette.KindTool}, body)
		return
	}
	s.fail(w, http.StatusConflict, fmt.Sprintf(
		"the cassette does not record an MCP %s, so the handshake cannot be replayed", method))
}

// protocolSpan finds a recorded exchange for one MCP method, consuming it so a
// repeated handshake gets successive recordings rather than the same one twice.
func (s *Server) protocolSpan(method string) *cassette.Span {
	for _, span := range s.idx.Spans() {
		if span.Kind != cassette.KindTool {
			continue
		}
		if span.String("mcp.method.name") != method {
			continue
		}
		if s.servedProtocol[span.SpanID+method] {
			continue
		}
		s.servedProtocol[span.SpanID+method] = true
		return span
	}
	// Exhausted: reuse the last one rather than failing, since a client may
	// legitimately repeat a handshake more often than the recording did.
	for _, span := range s.idx.Spans() {
		if span.Kind == cassette.KindTool && span.String("mcp.method.name") == method {
			return span
		}
	}
	return nil
}

func toolLabel(path string) string {
	rest := strings.TrimPrefix(path, "/mcp/")
	if i := strings.Index(rest, "/"); i >= 0 {
		return rest[:i]
	}
	return rest
}

func contentTypeOf(s *cassette.Span) string {
	if h, ok := s.Attributes[cassette.AttrResponseHeaders].(map[string]any); ok {
		if ct, ok := h["content-type"].(string); ok && ct != "" {
			return ct
		}
	}
	return "application/json"
}

func statusOf(s *cassette.Span) int {
	switch v := s.Attributes[cassette.AttrUpstreamStatus].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}
