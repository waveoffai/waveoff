// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package jsonrpc

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRecordedResponseIsRetargeted is the fix for a bug that presents as a
// hang, not a failure: a JSON-RPC client correlates by id, so a recorded
// response carrying the id of a request nobody made leaves the client waiting
// forever.
func TestRecordedResponseIsRetargeted(t *testing.T) {
	recorded := []byte(`{"jsonrpc":"2.0","id":7,"result":{"content":[{"type":"text","text":"Echo"}]}}`)
	out := RewriteID(recorded, json.RawMessage(`42`))

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("the rewritten response is not valid JSON: %v\n%s", err, out)
	}
	if got["id"] != float64(42) {
		t.Errorf("id = %v, want 42", got["id"])
	}
	// The payload must otherwise survive intact.
	if !strings.Contains(string(out), "Echo") {
		t.Errorf("the result was damaged: %s", out)
	}
}

// TestSSEFramedResponseIsRetargeted: the streamable HTTP transport chooses per
// response, so the same rewrite has to work inside an event stream without
// disturbing the framing.
func TestSSEFramedResponseIsRetargeted(t *testing.T) {
	recorded := []byte("id: abc\ndata: \n\nevent: message\nid: def\n" +
		`data: {"jsonrpc":"2.0","id":3,"result":{"content":[]}}` + "\n\n")

	out := string(RewriteID(recorded, json.RawMessage(`"call-1"`)))

	if !strings.Contains(out, `"id":"call-1"`) {
		t.Errorf("the JSON-RPC id inside the stream was not rewritten:\n%s", out)
	}
	// Framing is not ours to touch.
	for _, want := range []string{"event: message", "id: abc", "id: def"} {
		if !strings.Contains(out, want) {
			t.Errorf("the stream framing lost %q:\n%s", want, out)
		}
	}
}

// TestNotificationsAreLeftAlone: a message with no id is a notification, and
// giving it one turns it into a response nobody asked for.
func TestNotificationsAreLeftAlone(t *testing.T) {
	recorded := []byte(`{"jsonrpc":"2.0","method":"notifications/message","params":{"level":"info"}}`)
	out := RewriteID(recorded, json.RawMessage(`9`))
	if strings.Contains(string(out), `"id"`) {
		t.Errorf("a notification was given an id: %s", out)
	}
}

func TestRewriteIsSafeOnJunk(t *testing.T) {
	for _, body := range []string{"", "not json at all", "{", `{"id":}`} {
		out := RewriteID([]byte(body), json.RawMessage(`1`))
		if string(out) != body {
			t.Errorf("rewriting %q changed it to %q; unparseable bodies must pass through", body, out)
		}
	}
}

func TestRequestIDExtraction(t *testing.T) {
	cases := map[string]string{
		`{"jsonrpc":"2.0","id":5,"method":"tools/call"}`:     "5",
		`{"jsonrpc":"2.0","id":"abc","method":"tools/call"}`: `"abc"`,
		`{"jsonrpc":"2.0","method":"notifications/x"}`:       "",
	}
	for body, want := range cases {
		if got := string(RequestID([]byte(body))); got != want {
			t.Errorf("RequestID(%s) = %q, want %q", body, got, want)
		}
	}
}
