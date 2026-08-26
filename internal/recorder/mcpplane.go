// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package recorder

import (
	"encoding/json"
	"strings"
	"sync"

	"github.com/waveoffai/waveoff/internal/cassette"
	"github.com/waveoffai/waveoff/internal/mcp"
)

// MCPAnnotator understands enough JSON-RPC to describe what a tool call did.
//
// The transport is an ordinary reverse proxy, so this is not a second proxy: it
// is a Sink wrapper that reads the traffic already being recorded and adds the
// attributes that make a tool span useful.
//
// The important one is the contract digest. Every tools/list response is
// hashed, per tool, and the digest is attached to subsequent calls of that
// tool. That is what lets replay compare the contract a session was recorded
// against with the contract a server advertises today — turning a corpus that
// would otherwise rot invisibly into one that reports its own staleness.
type MCPAnnotator struct {
	next Sink

	mu        sync.RWMutex
	contracts map[string]string // tool name -> contract digest
}

// NewMCPAnnotator wraps a sink.
func NewMCPAnnotator(next Sink) *MCPAnnotator {
	return &MCPAnnotator{next: next, contracts: map[string]string{}}
}

// jsonRPCRequest is the subset of a request this needs.
type jsonRPCRequest struct {
	Method string `json:"method"`
	Params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"params"`
}

// Record implements Sink.
func (a *MCPAnnotator) Record(r *Record) {
	if r.Plane == PlaneTool {
		a.annotate(r)
	}
	a.next.Record(r)
}

func (a *MCPAnnotator) annotate(r *Record) {
	var req jsonRPCRequest
	if err := json.Unmarshal(r.ReqBody, &req); err != nil || req.Method == "" {
		return
	}
	r.MCPMethod = req.Method

	switch req.Method {
	case "tools/list":
		a.learnContracts(r.RespBody)
	case "tools/call":
		r.ToolName = req.Params.Name
		// The argument hash is how replay finds this call again once the model
		// is running live and step indices no longer line up. Normalised, so a
		// reordered JSON object still matches.
		r.ToolArgsHash = NormalisedHash(req.Params.Arguments)
		a.mu.RLock()
		r.ToolContractDigest = a.contracts[req.Params.Name]
		a.mu.RUnlock()
	}
}

// learnContracts records the contract of every advertised tool.
//
// The digest covers the description text as well as the schema, because the
// description is prompt input — which is what makes a silent rewrite both a
// behavioural change and a detectable tool-poisoning attempt.
func (a *MCPAnnotator) learnContracts(body []byte) {
	payload := body
	// A streamable HTTP server may answer with server-sent events, in which
	// case the JSON-RPC message is inside a data: frame.
	if p, ok := firstSSEData(body); ok {
		payload = p
	}

	var env struct {
		Result struct {
			Tools []mcp.Tool `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	for _, t := range env.Result.Tools {
		d, err := t.ContractDigest()
		if err != nil {
			continue
		}
		a.contracts[t.Name] = d
	}
}

// Contracts returns a copy of what has been learned, for tests and metrics.
func (a *MCPAnnotator) Contracts() map[string]string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make(map[string]string, len(a.contracts))
	for k, v := range a.contracts {
		out[k] = v
	}
	return out
}

// firstSSEData extracts the first data: payload from an event stream.
func firstSSEData(b []byte) ([]byte, bool) {
	s := string(b)
	if !strings.Contains(s, "data:") {
		return nil, false
	}
	var sb strings.Builder
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimRight(line, "\r")
		if rest, ok := strings.CutPrefix(line, "data:"); ok {
			sb.WriteString(strings.TrimSpace(rest))
			continue
		}
		if line == "" && sb.Len() > 0 {
			break
		}
	}
	if sb.Len() == 0 {
		return nil, false
	}
	return []byte(sb.String()), true
}

// mcpAttributes adds the MCP-specific attributes to a span. Names follow the
// OpenTelemetry MCP semantic conventions rather than being invented.
func mcpAttributes(r *Record, attrs map[string]any) {
	if r.MCPMethod != "" {
		attrs["mcp.method.name"] = r.MCPMethod
	}
	if r.ToolName != "" {
		attrs["mcp.tool.name"] = r.ToolName
		attrs["gen_ai.tool.name"] = r.ToolName
	}
	if r.ToolContractDigest != "" {
		attrs[cassette.AttrToolContractDigest] = r.ToolContractDigest
	}
	if r.ToolArgsHash != "" {
		attrs[cassette.AttrToolArgsHash] = r.ToolArgsHash
	}
	if r.ToolEffect != "" {
		attrs[cassette.AttrToolEffect] = r.ToolEffect
	}
	if r.Suppressed {
		attrs[cassette.AttrToolSuppressed] = true
	}
	if r.Refused {
		attrs[cassette.AttrToolRefused] = true
	}
}
