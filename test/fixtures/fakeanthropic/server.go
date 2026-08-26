// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

// Package fakeanthropic is an Anthropic-compatible endpoint for tests.
//
// It exists so the recorder can be exercised against a real agent framework
// without a funded API key and without non-determinism. The framework, the SDK,
// the HTTP shapes and the streaming protocol are all genuine; only the model is
// not. That is the right place to draw the line: the recorder never sees a
// model, it sees HTTP, and this produces the HTTP a model produces.
package fakeanthropic

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Turn is one scripted response.
type Turn struct {
	// Text is the assistant's reply. Ignored when ToolName is set.
	Text string
	// ToolName and ToolInput make the model ask for a tool call, which is what
	// drives an agent framework round the loop rather than stopping after one
	// exchange.
	ToolName  string
	ToolInput map[string]any
}

// Server serves a scripted conversation.
type Server struct {
	mu    sync.Mutex
	turns []Turn
	calls atomic.Int64

	// Model is echoed back so the recorder and cassette see a realistic id.
	Model string
}

// New builds a server that replies with the given turns in order, repeating the
// last one if the agent keeps going.
func New(turns ...Turn) *Server {
	if len(turns) == 0 {
		turns = []Turn{{Text: "ok"}}
	}
	return &Server{turns: turns, Model: "claude-sonnet-4-6"}
}

// Calls reports how many requests have been served.
func (s *Server) Calls() int64 { return s.calls.Load() }

func (s *Server) next() Turn {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := int(s.calls.Load())
	if i >= len(s.turns) {
		i = len(s.turns) - 1
	}
	return s.turns[i]
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasSuffix(r.URL.Path, "/messages") {
		http.NotFound(w, r)
		return
	}
	var req struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	body := map[string]any{}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if m, ok := body["model"].(string); ok {
		req.Model = m
	}
	if st, ok := body["stream"].(bool); ok {
		req.Stream = st
	}

	turn := s.next()
	s.calls.Add(1)

	model := req.Model
	if model == "" {
		model = s.Model
	}
	// Real providers return the resolved model version in a header, and a
	// manifest pins it, so the recorder must see one.
	w.Header().Set("anthropic-version", "2023-06-01")
	w.Header().Set("request-id", fmt.Sprintf("req_fake_%d", s.calls.Load()))

	if req.Stream {
		s.stream(w, model, turn)
		return
	}
	s.json(w, model, turn)
}

func (s *Server) json(w http.ResponseWriter, model string, turn Turn) {
	resp := map[string]any{
		"id": "msg_fake", "type": "message", "role": "assistant",
		"model": model, "content": contentFor(turn),
		"stop_reason": stopReason(turn),
		"usage":       map[string]any{"input_tokens": 1204, "output_tokens": 89},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// interEventDelay separates streamed events so they cannot coalesce into one
// segment. Small enough to be invisible in a test's runtime, large enough to
// survive a loaded CI runner.
const interEventDelay = 3 * time.Millisecond

// stream emits the server-sent event sequence the Anthropic SDK expects, so the
// streaming path through the recorder is exercised for real rather than
// approximated.
func (s *Server) stream(w http.ResponseWriter, model string, turn Turn) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flush, _ := w.(http.Flusher)

	emit := func(event string, payload any) {
		b, _ := json.Marshal(payload)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		if flush != nil {
			flush.Flush()
		}
		// A flush is a request, not a guarantee. Events written back to back
		// can still land in one TCP segment, and the proxy reading them then
		// legitimately sees a single chunk — which is correct behaviour and an
		// unprovable test. A real provider streams tokens as a model produces
		// them, milliseconds apart; this reproduces that spacing so the
		// streaming shape a test asserts on is one the network cannot flatten.
		time.Sleep(interEventDelay)
	}

	emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": "msg_fake", "type": "message", "role": "assistant", "model": model,
			"content": []any{}, "usage": map[string]any{"input_tokens": 1204, "output_tokens": 0},
		},
	})

	if turn.ToolName != "" {
		emit("content_block_start", map[string]any{
			"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "tool_use", "id": "toolu_fake", "name": turn.ToolName, "input": map[string]any{}},
		})
		args, _ := json.Marshal(turn.ToolInput)
		emit("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": string(args)},
		})
	} else {
		emit("content_block_start", map[string]any{
			"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "text", "text": ""},
		})
		// Several deltas, so the recorder's chunk counting sees a real stream.
		for _, word := range strings.Fields(turn.Text) {
			emit("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": 0,
				"delta": map[string]any{"type": "text_delta", "text": word + " "},
			})
		}
	}

	emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	emit("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason(turn), "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": 89},
	})
	emit("message_stop", map[string]any{"type": "message_stop"})
}

func contentFor(turn Turn) []any {
	if turn.ToolName != "" {
		input := turn.ToolInput
		if input == nil {
			input = map[string]any{}
		}
		return []any{map[string]any{
			"type": "tool_use", "id": "toolu_fake", "name": turn.ToolName, "input": input,
		}}
	}
	return []any{map[string]any{"type": "text", "text": turn.Text}}
}

func stopReason(turn Turn) string {
	if turn.ToolName != "" {
		return "tool_use"
	}
	return "end_turn"
}
