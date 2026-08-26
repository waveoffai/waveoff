// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package recorder_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/waveoffai/waveoff/api/v1alpha1"
	"github.com/waveoffai/waveoff/internal/cassette"
	"github.com/waveoffai/waveoff/internal/recorder"
)

func effects() map[string]v1alpha1.ToolEffect {
	return map[string]v1alpha1.ToolEffect{
		"docs.search":       v1alpha1.EffectRead,
		"jira.create_issue": v1alpha1.EffectWrite,
		"cache.warm":        v1alpha1.EffectIdempotentWrite,
	}
}

// upstreamCounting records every call that actually reached the server.
func upstreamCounting(t *testing.T, reached *[]string) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var call struct {
			Method string `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		json.Unmarshal(body, &call)
		*reached = append(*reached, call.Params.Name+"/"+call.Method)
		io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[]}}`)
	})
}

func rpc(id int, method, tool string) string {
	return `{"jsonrpc":"2.0","id":` + itoaTest(id) + `,"method":"` + method +
		`","params":{"name":"` + tool + `","arguments":{}}}`
}

func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func postTo(t *testing.T, ts *httptest.Server, body string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Post(ts.URL, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

// TestAWriteNeverReachesTheServer is the property that makes shadow traffic
// safe. Mirroring alone stops nothing: the candidate would file the ticket.
func TestAWriteNeverReachesTheServer(t *testing.T) {
	var reached []string
	s := recorder.NewSuppressor(effects(), upstreamCounting(t, &reached))
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, body := postTo(t, ts, rpc(7, "tools/call", "jira.create_issue"))

	if len(reached) != 0 {
		t.Errorf("a write reached the server: %v", reached)
	}
	if resp.Header.Get("X-Waveoff-Suppressed") != "true" {
		t.Error("the response is not marked as suppressed")
	}
	// The agent must see a plausible success, and one honest about what
	// happened, so a shadow session runs to completion instead of collapsing.
	if !strings.Contains(body, "was not executed") {
		t.Errorf("the synthesised result does not say nothing happened: %s", body)
	}
	if strings.Contains(body, `"error"`) {
		t.Errorf("a suppressed write was reported as an error: %s", body)
	}
	// And it must carry the id the client used, or the client waits forever.
	if !strings.Contains(body, `"id":7`) {
		t.Errorf("the response does not answer the request's id: %s", body)
	}
}

// TestIdempotentWritesAreAlsoSuppressed: "safe to repeat" is not "safe to
// perform". A shadow candidate re-sending a webhook is still sending it.
func TestIdempotentWritesAreAlsoSuppressed(t *testing.T) {
	var reached []string
	s := recorder.NewSuppressor(effects(), upstreamCounting(t, &reached))
	ts := httptest.NewServer(s)
	defer ts.Close()

	postTo(t, ts, rpc(1, "tools/call", "cache.warm"))
	if len(reached) != 0 {
		t.Errorf("an idempotent write reached the server: %v", reached)
	}
}

// TestReadsPassThrough: a shadow candidate that cannot read anything is not
// exercising anything.
func TestReadsPassThrough(t *testing.T) {
	var reached []string
	s := recorder.NewSuppressor(effects(), upstreamCounting(t, &reached))
	ts := httptest.NewServer(s)
	defer ts.Close()

	postTo(t, ts, rpc(1, "tools/call", "docs.search"))
	if len(reached) != 1 {
		t.Fatalf("a read was blocked: reached = %v", reached)
	}
	if resp, _ := postTo(t, ts, rpc(2, "tools/call", "docs.search")); resp.Header.Get("X-Waveoff-Suppressed") == "true" {
		t.Error("a read was marked suppressed")
	}
}

// TestUnclassifiedToolsFailClosed: the tool nobody classified is exactly the
// one that might write.
func TestUnclassifiedToolsFailClosed(t *testing.T) {
	var reached []string
	s := recorder.NewSuppressor(effects(), upstreamCounting(t, &reached))
	ts := httptest.NewServer(s)
	defer ts.Close()

	_, body := postTo(t, ts, rpc(1, "tools/call", "mystery.tool"))
	if len(reached) != 0 {
		t.Errorf("an unclassified tool reached the server: %v", reached)
	}
	if !strings.Contains(body, "no asserted effect") {
		t.Errorf("the refusal should say why: %s", body)
	}
}

// TestProtocolChatterIsNotSuppressed: an agent that cannot complete the MCP
// handshake never gets as far as a tool call, so nothing would be observed.
func TestProtocolChatterIsNotSuppressed(t *testing.T) {
	var reached []string
	s := recorder.NewSuppressor(effects(), upstreamCounting(t, &reached))
	ts := httptest.NewServer(s)
	defer ts.Close()

	for _, method := range []string{"initialize", "tools/list", "notifications/initialized", "ping"} {
		postTo(t, ts, `{"jsonrpc":"2.0","id":1,"method":"`+method+`"}`)
	}
	if len(reached) != 4 {
		t.Errorf("protocol chatter was blocked: reached = %v", reached)
	}
}

// TestTransportMethodsPassThrough: a GET opens the server-to-client stream and
// a DELETE ends the session; neither carries a tool call.
func TestTransportMethodsPassThrough(t *testing.T) {
	var seen []string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method)
		w.WriteHeader(http.StatusOK)
	})
	ts := httptest.NewServer(recorder.NewSuppressor(effects(), next))
	defer ts.Close()

	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		req, _ := http.NewRequest(method, ts.URL, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	if len(seen) != 2 {
		t.Errorf("transport methods were blocked: %v", seen)
	}
}

func TestSuppressorCounts(t *testing.T) {
	var reached []string
	s := recorder.NewSuppressor(effects(), upstreamCounting(t, &reached))
	ts := httptest.NewServer(s)
	defer ts.Close()

	postTo(t, ts, rpc(1, "tools/call", "jira.create_issue"))
	postTo(t, ts, rpc(2, "tools/call", "docs.search"))
	postTo(t, ts, rpc(3, "tools/call", "mystery.tool"))

	suppressed, refused, allowed, _ := s.Stats()
	if suppressed != 1 || refused != 1 || allowed != 1 {
		t.Errorf("stats = (%d, %d, %d), want (1, 1, 1)", suppressed, refused, allowed)
	}
}

// TestSuppressionRequiresClassification: refusing every call is safe and
// useless, so starting that way is an error rather than a silent no-op.
func TestSuppressionRequiresClassification(t *testing.T) {
	_, err := recorder.NewServer(recorder.Config{
		ModelUpstream:  "http://example.invalid",
		Listen:         "127.0.0.1:0",
		SuppressWrites: true,
	})
	if err == nil {
		t.Fatal("write suppression started with no tool classifications")
	}
	if !strings.Contains(err.Error(), "nothing would be observed") {
		t.Errorf("err = %v", err)
	}
}

// TestSuppressionIsWiredIntoTheToolPlane end to end through the server.
func TestSuppressionIsWiredIntoTheToolPlane(t *testing.T) {
	var reached []string
	upstream := httptest.NewServer(upstreamCounting(t, &reached))
	defer upstream.Close()

	srv, err := recorder.NewServer(recorder.Config{
		Listen:         "127.0.0.1:0",
		ToolUpstreams:  map[string]string{"jira": upstream.URL},
		SuppressWrites: true,
		ToolEffects:    effects(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/mcp/jira", "application/json",
		strings.NewReader(rpc(1, "tools/call", "jira.create_issue")))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if len(reached) != 0 {
		t.Errorf("a write reached the MCP server through the recorder: %v", reached)
	}
	if resp.Header.Get("X-Waveoff-Suppressed") != "true" {
		t.Error("the recorder did not suppress the write")
	}

	_ = context.Background()
}

// callWithArgs builds a tools/call carrying arbitrary arguments.
func callWithArgs(id int, tool, args string) string {
	return `{"jsonrpc":"2.0","id":` + itoaTest(id) + `,"method":"tools/call",` +
		`"params":{"name":"` + tool + `","arguments":` + args + `}}`
}

func sessionPost(t *testing.T, ts *httptest.Server, session, body string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(recorder.SessionHeader, session)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

// TestASuppressedWriteReturnsSomethingToWorkWith.
//
// An agent that has just created a thing uses its id — to comment on it, to
// link to it, to read it back. A result with no id sends a well-built agent
// straight down an error path, which is then measured as a regression the
// candidate did not cause.
func TestASuppressedWriteReturnsSomethingToWorkWith(t *testing.T) {
	var reached []string
	s := recorder.NewSuppressor(effects(), upstreamCounting(t, &reached))
	ts := httptest.NewServer(s)
	defer ts.Close()

	_, body := sessionPost(t, ts, "sess-1", callWithArgs(1, "jira.create_issue", `{"summary":"x"}`))

	var resp struct {
		Result struct {
			ID string `json:"id"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("the suppressed write did not return valid JSON: %v\n%s", err, body)
	}
	if resp.Result.ID == "" {
		t.Fatalf("a suppressed write returned no identifier; an agent cannot refer to what it just "+
			"created:\n%s", body)
	}
}

// TestAnAgentCanReadBackItsOwnSuppressedWrite is the property that makes shadow
// measurable at all for an agent that verifies its work. Without it the agent
// watches its own write vanish and reports a failure the candidate never had.
func TestAnAgentCanReadBackItsOwnSuppressedWrite(t *testing.T) {
	var reached []string
	s := recorder.NewSuppressor(map[string]v1alpha1.ToolEffect{
		"jira.create_issue": v1alpha1.EffectWrite,
		"jira.get_issue":    v1alpha1.EffectRead,
	}, upstreamCounting(t, &reached))
	ts := httptest.NewServer(s)
	defer ts.Close()

	_, created := sessionPost(t, ts, "sess-1", callWithArgs(1, "jira.create_issue", `{"summary":"x"}`))
	var write struct {
		Result struct {
			ID string `json:"id"`
		} `json:"result"`
	}
	json.Unmarshal([]byte(created), &write)

	// The agent now reads back the thing it thinks it made.
	_, read := sessionPost(t, ts, "sess-1",
		callWithArgs(2, "jira.get_issue", `{"issueKey":"`+write.Result.ID+`"}`))

	if len(reached) != 0 {
		t.Errorf("a read for a synthetic object reached the live server: %v", reached)
	}
	if !strings.Contains(read, write.Result.ID) {
		t.Errorf("the read did not return the object the write invented:\n%s", read)
	}
	if _, _, _, replayed := s.Stats(); replayed != 1 {
		t.Errorf("replayed = %d, want 1", replayed)
	}
}

// TestSyntheticObjectsAreScopedToASession: two concurrent sessions must not be
// able to see each other's invented objects.
func TestSyntheticObjectsAreScopedToASession(t *testing.T) {
	var reached []string
	s := recorder.NewSuppressor(map[string]v1alpha1.ToolEffect{
		"jira.create_issue": v1alpha1.EffectWrite,
		"jira.get_issue":    v1alpha1.EffectRead,
	}, upstreamCounting(t, &reached))
	ts := httptest.NewServer(s)
	defer ts.Close()

	_, created := sessionPost(t, ts, "sess-1", callWithArgs(1, "jira.create_issue", `{"summary":"x"}`))
	var write struct {
		Result struct {
			ID string `json:"id"`
		} `json:"result"`
	}
	json.Unmarshal([]byte(created), &write)

	// A different session asks about it. It is not theirs, so the call goes to
	// the real server, which is the honest answer.
	sessionPost(t, ts, "sess-2", callWithArgs(2, "jira.get_issue", `{"issueKey":"`+write.Result.ID+`"}`))
	if len(reached) != 1 {
		t.Errorf("another session's synthetic object was served from the registry: reached = %v", reached)
	}
}

// TestARetriedWriteYieldsTheSameObject: an agent that retries a write it
// believes failed should find the thing it already made, not a second copy.
func TestARetriedWriteYieldsTheSameObject(t *testing.T) {
	var reached []string
	s := recorder.NewSuppressor(effects(), upstreamCounting(t, &reached))
	ts := httptest.NewServer(s)
	defer ts.Close()

	ids := make([]string, 2)
	for i := range ids {
		_, body := sessionPost(t, ts, "sess-1", callWithArgs(i+1, "jira.create_issue", `{"summary":"x"}`))
		var resp struct {
			Result struct {
				ID string `json:"id"`
			} `json:"result"`
		}
		json.Unmarshal([]byte(body), &resp)
		ids[i] = resp.Result.ID
	}
	if ids[0] != ids[1] {
		t.Errorf("a retried write invented a second object: %s then %s", ids[0], ids[1])
	}
}

// TestWriteAttemptsAreCounted: the set and count of attempted writes is the
// most informative thing a shadow stage produces, and needs no judge.
func TestWriteAttemptsAreCounted(t *testing.T) {
	var reached []string
	s := recorder.NewSuppressor(effects(), upstreamCounting(t, &reached))
	ts := httptest.NewServer(s)
	defer ts.Close()

	sessionPost(t, ts, "s", callWithArgs(1, "jira.create_issue", `{"a":1}`))
	sessionPost(t, ts, "s", callWithArgs(2, "jira.create_issue", `{"a":2}`))
	sessionPost(t, ts, "s", callWithArgs(3, "cache.warm", `{}`))
	sessionPost(t, ts, "s", callWithArgs(4, "docs.search", `{}`))

	attempts := s.Attempts()
	if attempts["jira.create_issue"] != 2 {
		t.Errorf("jira.create_issue attempts = %d, want 2", attempts["jira.create_issue"])
	}
	if attempts["cache.warm"] != 1 {
		t.Errorf("cache.warm attempts = %d, want 1", attempts["cache.warm"])
	}
	if _, ok := attempts["docs.search"]; ok {
		t.Error("a read was counted as a write attempt")
	}
}

// collectSink keeps every record handed to it.
type collectSink struct {
	mu      sync.Mutex
	records []*recorder.Record
}

func (c *collectSink) Record(r *recorder.Record) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, r)
}

func (c *collectSink) all() []*recorder.Record {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*recorder.Record(nil), c.records...)
}

// TestASuppressedWriteIsStillRecorded.
//
// A suppressed call never reaches the proxy, so without an explicit recording
// it would be absent from the cassette entirely — and the session would read as
// though the candidate never tried to write. That is exactly backwards: the
// attempts are the most informative thing a shadow stage produces, because
// they are the only evidence of writes that did not happen.
func TestASuppressedWriteIsStillRecorded(t *testing.T) {
	var reached []string
	sink := &collectSink{}
	s := recorder.NewSuppressor(effects(), upstreamCounting(t, &reached))
	s.Sink = sink
	ts := httptest.NewServer(s)
	defer ts.Close()

	sessionPost(t, ts, "sess-1", callWithArgs(1, "jira.create_issue", `{"summary":"x"}`))

	records := sink.all()
	if len(records) != 1 {
		t.Fatalf("recorded %d calls, want 1", len(records))
	}
	rec := records[0]
	if !rec.Suppressed {
		t.Error("the record does not say the call was suppressed, so a reader would take the " +
			"placeholder for an effect that happened")
	}
	if rec.ToolName != "jira.create_issue" {
		t.Errorf("ToolName = %q", rec.ToolName)
	}
	if rec.ToolEffect != "write" {
		t.Errorf("ToolEffect = %q, want the manifest's classification", rec.ToolEffect)
	}
	if rec.Session != "sess-1" {
		t.Errorf("Session = %q", rec.Session)
	}
}

// TestAnUnclassifiedToolIsRecordedAsARefusal.
//
// Distinguishable from a suppressed write on purpose: one is a candidate trying
// to do something, the other is a manifest that does not describe the tools the
// agent actually has. Those need different fixes.
func TestAnUnclassifiedToolIsRecordedAsARefusal(t *testing.T) {
	var reached []string
	sink := &collectSink{}
	s := recorder.NewSuppressor(effects(), upstreamCounting(t, &reached))
	s.Sink = sink
	ts := httptest.NewServer(s)
	defer ts.Close()

	sessionPost(t, ts, "sess-1", callWithArgs(1, "unknown.tool", `{}`))

	records := sink.all()
	if len(records) != 1 {
		t.Fatalf("recorded %d calls, want 1", len(records))
	}
	if records[0].ToolEffect != "" {
		t.Errorf("a refused tool was recorded with effect %q; it has none", records[0].ToolEffect)
	}
	if !records[0].Refused || records[0].Suppressed {
		t.Error("a refusal was recorded as a suppressed write. One is a candidate trying to do " +
			"something and the other is a manifest that does not describe the agent's tools, and " +
			"they need different fixes")
	}
	attrs := recorder.SpanAttributes(records[0])
	if attrs[cassette.AttrToolRefused] != true {
		t.Error("the span does not mark the call refused")
	}
	if attrs[cassette.AttrToolSuppressed] == true {
		t.Error("the span marks a refusal as a suppression")
	}
	if !strings.Contains(string(records[0].RespBody), "no asserted effect") {
		t.Errorf("the recorded response does not say why it was refused: %s", records[0].RespBody)
	}
}

// TestSuppressedCallsCarryTheContractDigest.
//
// Recording through the same annotator the proxies use means a suppressed write
// arrives in the cassette with the contract the server advertised. Write tools
// are the ones contract drift matters most on, and they are precisely the ones
// a shadow stage never executes — so without this the corpus would be blind to
// drift exactly where it counts.
func TestSuppressedCallsCarryTheContractDigest(t *testing.T) {
	var reached []string
	sink := &collectSink{}
	annotator := recorder.NewMCPAnnotator(sink)

	// Learn a contract the way the proxy would: a tools/list flowing through.
	annotator.Record(&recorder.Record{
		Plane:    recorder.PlaneTool,
		ReqBody:  []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`),
		RespBody: []byte(`{"result":{"tools":[{"name":"jira.create_issue","description":"file a ticket","inputSchema":{"type":"object"}}]}}`),
	})

	s := recorder.NewSuppressor(effects(), upstreamCounting(t, &reached))
	s.Sink = annotator
	ts := httptest.NewServer(s)
	defer ts.Close()

	sessionPost(t, ts, "sess-1", callWithArgs(1, "jira.create_issue", `{"summary":"x"}`))

	records := sink.all()
	if len(records) != 2 {
		t.Fatalf("recorded %d calls, want 2", len(records))
	}
	call := records[1]
	if call.ToolContractDigest == "" {
		t.Fatal("a suppressed write carries no contract digest, so drift detection cannot see the " +
			"one class of tool a shadow stage never runs")
	}
	attrs := recorder.SpanAttributes(call)
	if attrs[cassette.AttrToolSuppressed] != true {
		t.Error("the span does not mark the call suppressed")
	}
	if attrs[cassette.AttrToolContractDigest] != call.ToolContractDigest {
		t.Error("the contract digest did not reach the span")
	}
}
