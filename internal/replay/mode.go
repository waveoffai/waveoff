// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package replay

import (
	"fmt"
	"strings"

	"github.com/waveoffai/waveoff/api/v1alpha1"
	"github.com/waveoffai/waveoff/internal/cassette"
)

// Mode selects how much a candidate is allowed to diverge from the recording.
type Mode string

const (
	// ModeFail replays both planes and treats a miss as a finding.
	//
	// This is pure divergence detection: nothing is allowed to be different,
	// so the first request that is not in the cassette is the answer. Use it
	// when nothing behavioural was supposed to change.
	ModeFail Mode = "fail"

	// ModeModelLiveToolsReplayed is the primary evaluation mode.
	//
	// The model runs live against the candidate's prompts, so it can diverge —
	// which is the point, since a prompt change that produced identical output
	// would not need evaluating. Every tool call is served from the recording
	// for reads and refused for writes, so the evaluation is safe to run
	// against a corpus recorded from production without touching production.
	ModeModelLiveToolsReplayed Mode = "model-live-tools-replayed"

	// ModeHybrid forks named tools to the live server and replays the rest.
	//
	// For the case where one tool's snapshot has gone stale but the rest of the
	// corpus is good. Every forked tool is a hole in the safety argument, so
	// they are named explicitly rather than selected by a pattern.
	ModeHybrid Mode = "hybrid"
)

// Modes lists the valid values, for error messages and flag validation.
var Modes = []Mode{ModeFail, ModeModelLiveToolsReplayed, ModeHybrid}

// ParseMode validates a mode name.
func ParseMode(s string) (Mode, error) {
	for _, m := range Modes {
		if Mode(s) == m {
			return m, nil
		}
	}
	names := make([]string, len(Modes))
	for i, m := range Modes {
		names[i] = string(m)
	}
	return "", fmt.Errorf("unknown replay mode %q: use one of %s", s, strings.Join(names, ", "))
}

// Action is what the replayer does with a request.
type Action string

const (
	// ActionServe answers from the cassette.
	ActionServe Action = "serve"
	// ActionForward sends the request to the real upstream.
	ActionForward Action = "forward"
	// ActionNoOp refuses the call and synthesises a success.
	ActionNoOp Action = "no-op"
	// ActionRefuse fails the call outright.
	ActionRefuse Action = "refuse"
)

// Decision is what to do with one request, and why. The reason is carried
// because it ends up in the replay report, where "this tool was no-op'd
// because it writes" is the difference between a trustworthy result and an
// unexplained one.
type Decision struct {
	Action Action
	Reason string
}

// Policy decides, per request, how a replay handles it.
type Policy struct {
	Mode Mode
	// Effects classifies each tool, taken from the AgentManifest. A tool that
	// is not here has no asserted effect, which fails closed.
	Effects map[string]v1alpha1.ToolEffect
	// LiveTools are the tool names forked to the real server in hybrid mode.
	LiveTools map[string]bool
}

// NewPolicy builds a policy from a manifest.
func NewPolicy(mode Mode, spec *v1alpha1.AgentManifestSpec, liveTools []string) *Policy {
	p := &Policy{Mode: mode, Effects: map[string]v1alpha1.ToolEffect{}, LiveTools: map[string]bool{}}
	if spec != nil {
		for _, t := range spec.Tools {
			p.Effects[t.Name] = t.Effect
		}
	}
	for _, name := range liveTools {
		p.LiveTools[name] = true
	}
	return p
}

// Decide chooses what to do with a request that has already been looked up.
func (p *Policy) Decide(req Request, match Match) Decision {
	if req.Kind == cassette.KindModel {
		return p.decideModel(match)
	}
	return p.decideTool(req, match)
}

func (p *Policy) decideModel(match Match) Decision {
	switch p.Mode {
	case ModeModelLiveToolsReplayed:
		// The model always runs live in this mode, matched or not. Serving a
		// recorded completion would test the cassette rather than the
		// candidate's prompts.
		return Decision{ActionForward, "model runs live so the candidate can diverge"}
	case ModeHybrid:
		if match.Found() {
			return Decision{ActionServe, "replayed from the cassette"}
		}
		return Decision{ActionForward, "not in the cassette; forwarded to the live model"}
	default: // ModeFail
		if match.Found() {
			return Decision{ActionServe, "replayed from the cassette"}
		}
		return Decision{ActionRefuse, "no recording for this request, and the mode forbids diverging"}
	}
}

func (p *Policy) decideTool(req Request, match Match) Decision {
	effect, classified := p.Effects[req.Tool]

	// Fail closed. An unclassified tool is refused rather than passed through:
	// letting a tool nobody has classified run during an evaluation is exactly
	// what the mandatory effect field exists to prevent.
	if !classified || effect == "" {
		return Decision{ActionRefuse, fmt.Sprintf(
			"tool %q has no asserted effect in the manifest, so it cannot be run during a replay", req.Tool)}
	}

	if p.Mode == ModeHybrid && p.LiveTools[req.Tool] {
		if effect == v1alpha1.EffectRead {
			return Decision{ActionForward, "forked to the live server by --live-tool"}
		}
		// Forking a write to a live server during an evaluation would mutate
		// real state from a replay of production traffic.
		return Decision{ActionRefuse, fmt.Sprintf(
			"tool %q is effect=%s and cannot be forked live; only reads may be", req.Tool, effect)}
	}

	if effect != v1alpha1.EffectRead {
		// Writes never execute during a replay, matched or not. This is the
		// property that makes evaluating against production traffic safe, and
		// it is the one Mastra's open issue describes as unsolved.
		return Decision{ActionNoOp, fmt.Sprintf("effect=%s, so the call was refused and a success synthesised", effect)}
	}

	if match.Found() {
		return Decision{ActionServe, "read served from the recording"}
	}
	if p.Mode == ModeFail {
		return Decision{ActionRefuse, "no recording for this read, and the mode forbids diverging"}
	}
	// A read the cassette does not cover. Refusing is better than forwarding:
	// a live read during what is supposed to be an offline evaluation makes the
	// result depend on the state of the world at the moment it ran.
	return Decision{ActionRefuse, fmt.Sprintf(
		"no recording for this call to %q; the corpus does not cover the path the candidate took", req.Tool)}
}
