// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

// Package mcp is a minimal Model Context Protocol client: enough to ask a
// server what tools it advertises, and nothing else.
//
// Waveoff is not an MCP gateway and does not proxy tool calls here. It needs
// exactly one thing from a server — the contract each tool advertises — because
// that contract is prompt input, and a manifest that does not pin it cannot
// detect a rewritten tool description.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gowebpki/jcs"
)

// ProtocolVersion is the MCP revision this client speaks.
const ProtocolVersion = "2025-06-18"

// Tool is a tool contract exactly as a server advertised it.
type Tool struct {
	Name string `json:"name"`
	// Description is prompt input, which is why it is hashed into the contract
	// digest alongside the schema rather than treated as documentation.
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
	// Annotations are the server's own claims about what the tool does
	// (readOnlyHint, destructiveHint, idempotentHint). They are recorded for
	// display only. The server is the untrusted party in the tool-poisoning
	// threat model, so its claims never become an effect classification.
	Annotations *Annotations `json:"annotations,omitempty"`
}

// Annotations are advisory hints from the server.
type Annotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    *bool  `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool  `json:"destructiveHint,omitempty"`
	IdempotentHint  *bool  `json:"idempotentHint,omitempty"`
	OpenWorldHint   *bool  `json:"openWorldHint,omitempty"`
}

// ContractDigest hashes {name, description, inputSchema} under RFC 8785.
//
// Annotations are deliberately excluded: they are the server's claims about
// itself, not part of what the model is told, and including them would let a
// server invalidate a pinned contract without changing anything the model sees.
func (t Tool) ContractDigest() (string, error) {
	schema := t.InputSchema
	if len(schema) == 0 {
		schema = json.RawMessage("{}")
	}
	raw, err := json.Marshal(map[string]any{
		"name":        t.Name,
		"description": t.Description,
		// Already a json.RawMessage: pass it straight through so the schema is
		// hashed exactly as the server sent it, byte for byte.
		"inputSchema": schema,
	})
	if err != nil {
		return "", err
	}
	canonical, err := jcs.Transform(raw)
	if err != nil {
		return "", fmt.Errorf("canonicalise tool contract for %q: %w", t.Name, err)
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// SuggestedEffect maps the server's advisory hints onto an effect. The result
// is only ever rendered as a comment for a human to accept or reject: see
// docs/digest.md on why there is no flag to promote it automatically.
func (t Tool) SuggestedEffect() (effect string, ok bool) {
	if t.Annotations == nil {
		return "", false
	}
	a := t.Annotations
	switch {
	case a.ReadOnlyHint != nil && *a.ReadOnlyHint:
		return "read", true
	case a.DestructiveHint != nil && *a.DestructiveHint:
		return "write", true
	case a.IdempotentHint != nil && *a.IdempotentHint:
		return "idempotent-write", true
	}
	return "", false
}

// Client talks to one MCP server over the streamable HTTP transport.
type Client struct {
	Endpoint string
	HTTP     *http.Client
	Headers  map[string]string

	session string
	nextID  int
}

// New builds a client with a bounded timeout. `waveoff pin` contacts servers it
// was pointed at by a Deployment's config, which may be wrong or unreachable,
// and a hung introspection is worse than a failed one.
func New(endpoint string) *Client {
	return &Client{Endpoint: endpoint, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// ListTools performs the initialize handshake and returns the advertised tools.
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	if _, err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "waveoff", "version": "v1alpha1"},
	}); err != nil {
		return nil, fmt.Errorf("initialize %s: %w", c.Endpoint, err)
	}
	if err := c.notify(ctx, "notifications/initialized", map[string]any{}); err != nil {
		return nil, fmt.Errorf("initialized notification to %s: %w", c.Endpoint, err)
	}

	var tools []Tool
	cursor := ""
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := c.call(ctx, "tools/list", params)
		if err != nil {
			return nil, fmt.Errorf("tools/list on %s: %w", c.Endpoint, err)
		}
		var page struct {
			Tools      []Tool `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, fmt.Errorf("decode tools/list from %s: %w", c.Endpoint, err)
		}
		tools = append(tools, page.Tools...)
		// A truncated tool list would silently produce a manifest that pins
		// fewer tools than the agent can actually call, so paginate to the end.
		if page.NextCursor == "" || page.NextCursor == cursor {
			break
		}
		cursor = page.NextCursor
	}
	return tools, nil
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("server error %d: %s", e.Code, e.Message) }

func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.nextID++
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": c.nextID, "method": method, "params": params,
	})
	if err != nil {
		return nil, err
	}
	resp, err := c.post(ctx, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.session = sid
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("HTTP %s: %s", resp.Status, strings.TrimSpace(string(snippet)))
	}

	payload, err := readMessage(resp)
	if err != nil {
		return nil, err
	}
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  *rpcError       `json:"error"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if env.Error != nil {
		return nil, env.Error
	}
	return env.Result, nil
}

func (c *Client) notify(ctx context.Context, method string, params any) error {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
	if err != nil {
		return err
	}
	resp, err := c.post(ctx, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %s", resp.Status)
	}
	return nil
}

func (c *Client) post(ctx context.Context, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	// The transport may answer with either a plain JSON body or an SSE stream,
	// and a server picks per response, so both must be acceptable.
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", ProtocolVersion)
	if c.session != "" {
		req.Header.Set("Mcp-Session-Id", c.session)
	}
	for k, v := range c.Headers {
		req.Header.Set(k, v)
	}
	return c.HTTP.Do(req)
}

// readMessage handles both shapes the streamable HTTP transport may return.
func readMessage(resp *http.Response) ([]byte, error) {
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		return io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	}
	// SSE: take the first `data:` payload that parses as a JSON-RPC response.
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 32<<20)
	var data strings.Builder
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "data:"):
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		case line == "":
			if data.Len() > 0 {
				return []byte(data.String()), nil
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if data.Len() > 0 {
		return []byte(data.String()), nil
	}
	return nil, fmt.Errorf("event stream closed without a response")
}
