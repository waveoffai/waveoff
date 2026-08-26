// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

// Package jsonrpc holds the small amount of JSON-RPC handling both the recorder
// and the replayer need.
//
// It exists as its own package because the replayer already depends on the
// recorder, so the recorder cannot depend back on it — and both have to answer
// a tool call with a response the client will actually correlate.
package jsonrpc

import (
	"encoding/json"
	"strings"
)

// Call is the part of a request either side needs to look at.
type Call struct {
	Method string          `json:"method"`
	ID     json.RawMessage `json:"id"`
	Params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"params"`
}

// Parse reads a JSON-RPC request. A body that is not one yields a zero Call
// rather than an error: plenty of traffic on these paths is not JSON-RPC, and
// it is not this package's business to reject it.
func Parse(body []byte) Call {
	var c Call
	_ = json.Unmarshal(body, &c)
	return c
}

// RequestID extracts the id a client used.
func RequestID(body []byte) json.RawMessage { return Parse(body).ID }

// RewriteID retargets a response at the request being answered.
//
// Clients correlate responses to requests by id, and generate fresh ids on
// every run. Handing back an id the client never issued means it keeps waiting
// for a reply that will never come — the call does not fail, it hangs, which is
// the worst shape this bug can take because it reads as slowness.
//
// Handles both a bare JSON body and the server-sent-event framing the
// streamable HTTP transport uses, since a server chooses between them per
// response.
func RewriteID(body []byte, id json.RawMessage) []byte {
	if len(id) == 0 || len(body) == 0 {
		return body
	}
	if isSSE(body) {
		return rewriteSSE(body, id)
	}
	if out, ok := retarget(body, id); ok {
		return out
	}
	return body
}

func isSSE(body []byte) bool {
	head := body
	if len(head) > 512 {
		head = head[:512]
	}
	s := string(head)
	return strings.Contains(s, "data:") &&
		(strings.HasPrefix(s, "event:") || strings.HasPrefix(s, "data:") || strings.HasPrefix(s, "id:"))
}

// rewriteSSE retargets every JSON-RPC response inside an event stream, leaving
// the framing untouched.
func rewriteSSE(body []byte, id json.RawMessage) []byte {
	lines := strings.Split(string(body), "\n")
	for i, line := range lines {
		trimmed := strings.TrimRight(line, "\r")
		payload, ok := strings.CutPrefix(trimmed, "data:")
		if !ok {
			continue
		}
		payload = strings.TrimSpace(payload)
		if payload == "" {
			continue
		}
		out, changed := retarget([]byte(payload), id)
		if !changed {
			continue
		}
		suffix := ""
		if strings.HasSuffix(line, "\r") {
			suffix = "\r"
		}
		lines[i] = "data: " + string(out) + suffix
	}
	return []byte(strings.Join(lines, "\n"))
}

// retarget replaces the id of a JSON-RPC response.
//
// A message with no id is a notification and is left alone: giving one an id
// turns it into a response the client never asked for.
func retarget(payload []byte, id json.RawMessage) ([]byte, bool) {
	var msg map[string]json.RawMessage
	if err := json.Unmarshal(payload, &msg); err != nil {
		return payload, false
	}
	if _, ok := msg["id"]; !ok {
		return payload, false
	}
	msg["id"] = id
	out, err := json.Marshal(msg)
	if err != nil {
		return payload, false
	}
	return out, true
}
