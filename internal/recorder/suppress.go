// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package recorder

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/waveoffai/waveoff/api/v1alpha1"
	"github.com/waveoffai/waveoff/internal/jsonrpc"
)

// Suppressor refuses tool calls that would change the world.
//
// This is what makes a shadow stage safe. A candidate receiving mirrored
// production traffic will do everything it would normally do — file the ticket,
// send the email, charge the card — and mirroring alone does nothing to stop
// it. Without this, "shadow" means "we hope the candidate behaves", and the
// first time it does not, a shadow deployment writes to production twice.
//
// It sits in front of the tool-plane proxy rather than inside it, because the
// decision has to be made before anything is forwarded. A call that has already
// reached the server cannot be un-made.
type Suppressor struct {
	// Effects classifies each tool, taken from the AgentManifest the candidate
	// is running.
	Effects map[string]v1alpha1.ToolEffect

	// Next handles calls that are allowed through.
	Next http.Handler

	// Sessions identifies which agent run a call belongs to, so synthetic
	// objects are visible to the session that created them and to nothing else.
	Sessions *Sessions

	// synthetic remembers what suppressed writes pretended to create.
	synthetic *syntheticRegistry

	// Sink records suppressed and refused calls.
	//
	// A suppressed write never reaches the proxy, so without this it would be
	// absent from the cassette entirely — and the session would read as though
	// the candidate never tried to write at all. Recording it through the same
	// sink as everything else means the annotator attaches the contract digest
	// the server advertised, so contract-drift detection covers write tools
	// too. Those are the tools drift matters most on and the ones a shadow
	// stage otherwise never exercises.
	Sink Sink

	suppressed atomic.Int64
	refused    atomic.Int64
	allowed    atomic.Int64
	replayed   atomic.Int64

	// attempts counts write attempts by tool.
	//
	// This is the most informative thing a shadow stage produces and it costs
	// nothing to collect: a candidate attempting three times as many writes as
	// the incumbent, or attempting a write class the incumbent never does, has
	// changed in a way that matters — and no judge is needed to see it.
	//
	// The counts are also in the cassette, which is the durable copy; this is
	// the live one, for the sidecar's own status endpoint.
	attemptsMu sync.Mutex
	attempts   map[string]int
	steps      map[string]int
}

// NewSuppressor wraps a handler.
func NewSuppressor(effects map[string]v1alpha1.ToolEffect, next http.Handler) *Suppressor {
	if effects == nil {
		effects = map[string]v1alpha1.ToolEffect{}
	}
	return &Suppressor{
		Effects:   effects,
		Next:      next,
		Sessions:  NewSessions(0),
		synthetic: newSyntheticRegistry(0),
		Sink:      Discard,
	}
}

func (s *Suppressor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Anything that is not a POST is transport machinery — the server-to-client
	// stream, a session teardown — and carries no tool call to suppress.
	if r.Method != http.MethodPost {
		s.Next.ServeHTTP(w, r)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))

	call := jsonrpc.Parse(body)
	session, _ := sessionID(r)
	if session == "" {
		session = "unattributed"
	}

	if call.Method != "tools/call" {
		// Protocol chatter: initialize, discovery, notifications. None of it
		// changes anything outside the conversation.
		s.allowed.Add(1)
		s.Next.ServeHTTP(w, r)
		return
	}

	// A read that refers to something a suppressed write invented is answered
	// from the registry rather than forwarded. Letting it reach the live server
	// makes the agent watch its own work disappear.
	if object, _, found := s.synthetic.lookup(session, call.Params.Arguments); found {
		s.replayed.Add(1)
		s.write(w, http.StatusOK, call, object, nil)
		return
	}

	effect, classified := s.Effects[call.Params.Name]
	switch {
	case !classified || effect == "":
		// Fail closed. A tool nobody has classified is exactly the one that
		// might write, and letting it through on the grounds that we do not
		// know is the reverse of what the mandatory effect field is for.
		s.refused.Add(1)
		s.emit(session, call, "", body, refusal(call.Params.Name), true)
		s.refuse(w, call, fmt.Sprintf(
			"tool %q has no asserted effect in the manifest, so it cannot run against mirrored traffic",
			call.Params.Name))
		return

	case effect != v1alpha1.EffectRead:
		s.suppressed.Add(1)
		s.record(session, call)
		s.suppress(w, session, call, effect, body)
		return
	}

	s.allowed.Add(1)
	s.Next.ServeHTTP(w, r)
}

// suppress answers a write without performing it.
//
// The response carries an identifier as well as an explanation, because an
// agent that just created something will use its id: to comment on it, to link
// to it, to read it back. A result with no id sends a well-built agent straight
// down an error path, which is then measured as a regression the candidate did
// not cause.
func (s *Suppressor) suppress(w http.ResponseWriter, session string, call jsonrpc.Call,
	effect v1alpha1.ToolEffect, body []byte) {

	explanation := fmt.Sprintf("[waveoff] %s was not executed: this is shadow traffic and the tool "+
		"is classified effect=%s", call.Params.Name, effect)

	result := map[string]any{
		"content": []any{map[string]any{"type": "text", "text": explanation}},
		"isError": false,
	}
	id := s.synthetic.mint(session, call.Params.Name, call.Params.Arguments, result)

	// Both a structured field and a mention in the text, since tools differ in
	// where a caller looks for what was created.
	result["id"] = id
	result["content"] = []any{map[string]any{
		"type": "text",
		"text": explanation + " (a placeholder object " + id + " has been recorded for this session)",
	}}
	// Re-record with the id present, so a read back returns the same shape the
	// write returned.
	s.synthetic.mint(session, call.Params.Name, call.Params.Arguments, result)

	s.emit(session, call, string(effect), body, result, false)
	s.write(w, http.StatusOK, call, result, nil)
}

func (s *Suppressor) refuse(w http.ResponseWriter, call jsonrpc.Call, reason string) {
	s.write(w, http.StatusOK, call, nil, map[string]any{
		"code": -32000, "message": reason,
	})
}

func (s *Suppressor) write(w http.ResponseWriter, status int, call jsonrpc.Call,
	result map[string]any, rpcErr map[string]any) {

	response := map[string]any{"jsonrpc": "2.0"}
	if len(call.ID) > 0 {
		// The client correlates by id. Answering without one leaves it waiting
		// for a reply that never comes.
		response["id"] = call.ID
	}
	if rpcErr != nil {
		response["error"] = rpcErr
	} else {
		response["result"] = result
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Waveoff-Suppressed", "true")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(response)
}

// record counts a write attempt by tool name.
func (s *Suppressor) record(_ string, call jsonrpc.Call) {
	s.attemptsMu.Lock()
	defer s.attemptsMu.Unlock()
	if s.attempts == nil {
		s.attempts = map[string]int{}
	}
	s.attempts[call.Params.Name]++
}

// emit puts a suppressed or refused call into the cassette.
//
// The step is real traffic with a synthesised answer, and it is labelled as
// such: waveoff.tool.suppressed distinguishes it from a call that ran, so
// nothing downstream mistakes a placeholder for an effect that happened.
func (s *Suppressor) emit(session string, call jsonrpc.Call, effect string, req []byte,
	result map[string]any, refused bool) {

	if s.Sink == nil {
		return
	}
	resp, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "result": result})
	if err != nil {
		return
	}
	now := time.Now()
	s.attemptsMu.Lock()
	if s.steps == nil {
		s.steps = map[string]int{}
	}
	step := s.steps[session]
	s.steps[session]++
	s.attemptsMu.Unlock()

	s.Sink.Record(&Record{
		Session:    session,
		Plane:      PlaneTool,
		Step:       step,
		Method:     http.MethodPost,
		ReqBody:    req,
		Status:     http.StatusOK,
		RespBody:   resp,
		ToolName:   call.Params.Name,
		ToolEffect: effect,
		Suppressed: !refused,
		Refused:    refused,
		Start:      now,
		End:        now,
	})
}

// refusal is the body recorded for an unclassified tool. It is distinguishable
// from a suppressed write: one is a candidate trying to do something, the other
// is a manifest that does not describe the tools the agent actually has.
func refusal(tool string) map[string]any {
	return map[string]any{
		"isError": true,
		"content": []any{map[string]any{
			"type": "text",
			"text": "[waveoff] " + tool + " has no asserted effect in the manifest and was refused",
		}},
	}
}

// Attempts returns the write attempts seen, by tool.
//
// The set of tool names matters as much as the counts: a candidate reaching for
// a write the incumbent never makes is a finding on its own, deterministic and
// available without a scorer.
func (s *Suppressor) Attempts() map[string]int {
	s.attemptsMu.Lock()
	defer s.attemptsMu.Unlock()
	out := make(map[string]int, len(s.attempts))
	for k, v := range s.attempts {
		out[k] = v
	}
	return out
}

// Stats reports what the suppressor has done: writes refused and synthesised,
// unclassified tools refused, and calls allowed through.
//
// Worth surfacing. A shadow stage where nothing was ever suppressed either had
// no write tools or was not actually suppressing, and those look identical
// from outside.
func (s *Suppressor) Stats() (suppressed, refused, allowed, replayed int64) {
	return s.suppressed.Load(), s.refused.Load(), s.allowed.Load(), s.replayed.Load()
}
