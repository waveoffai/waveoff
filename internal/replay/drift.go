// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package replay

import (
	"context"
	"fmt"
	"sort"

	"github.com/waveoffai/waveoff/internal/cassette"
	"github.com/waveoffai/waveoff/internal/mcp"
)

// DriftStatus classifies one tool's contract against the live server.
type DriftStatus string

const (
	// DriftNone: the contract is what it was when the session was recorded.
	DriftNone DriftStatus = "current"
	// DriftChanged: the server advertises a different contract now. The
	// recording is scored against a fiction.
	DriftChanged DriftStatus = "changed"
	// DriftRemoved: the tool no longer exists.
	DriftRemoved DriftStatus = "removed"
	// DriftUnknown: the server could not be reached, so nothing is known. This
	// is not the same as "no drift", and must not be reported as if it were.
	DriftUnknown DriftStatus = "unknown"
)

// ToolDrift is the verdict for one tool.
type ToolDrift struct {
	Tool     string      `json:"tool"`
	Status   DriftStatus `json:"status"`
	Recorded string      `json:"recordedContract,omitempty"`
	Live     string      `json:"liveContract,omitempty"`
	Detail   string      `json:"detail,omitempty"`
}

// Stale reports whether this drift disqualifies a cassette from a gate.
func (d ToolDrift) Stale() bool {
	return d.Status == DriftChanged || d.Status == DriftRemoved
}

// DriftReport is the result of checking a cassette against live servers.
type DriftReport struct {
	Session string      `json:"session"`
	Tools   []ToolDrift `json:"tools"`
}

// Stale reports whether any tool in the session has drifted.
//
// A cassette whose contracts have moved must be excluded from a gate rather
// than scored: the recorded responses answer questions the tools no longer
// ask. A corpus that rots invisibly is worse than no corpus, because the
// numbers keep coming and nobody knows they mean nothing.
func (r *DriftReport) Stale() bool {
	for _, t := range r.Tools {
		if t.Stale() {
			return true
		}
	}
	return false
}

// Unknown reports whether any contract could not be checked. Distinguished
// from clean on purpose: "we could not reach the server" and "nothing changed"
// are different facts and only one of them justifies gating on the result.
func (r *DriftReport) Unknown() bool {
	for _, t := range r.Tools {
		if t.Status == DriftUnknown {
			return true
		}
	}
	return false
}

// ToolLister introspects an MCP server. Injected so drift checking is testable
// without a live server and so a different transport can be substituted.
type ToolLister func(ctx context.Context, endpoint string) ([]mcp.Tool, error)

// LiveTools is the default ToolLister.
func LiveTools(ctx context.Context, endpoint string) ([]mcp.Tool, error) {
	return mcp.New(endpoint).ListTools(ctx)
}

// CheckDrift compares the tool contracts a cassette recorded against what the
// servers advertise now.
//
// Endpoints are read from the spans themselves rather than from configuration,
// so the check asks the same servers the session actually used.
func CheckDrift(ctx context.Context, idx *Index, list ToolLister) (*DriftReport, error) {
	report := &DriftReport{Session: idx.Header().SessionID}

	// What the cassette recorded: tool -> (contract, endpoint).
	type recorded struct{ contract, endpoint string }
	seen := map[string]recorded{}
	for _, s := range idx.Spans() {
		if s.Kind != cassette.KindTool {
			continue
		}
		tool := s.String("mcp.tool.name")
		if tool == "" {
			continue
		}
		if _, ok := seen[tool]; !ok {
			seen[tool] = recorded{
				contract: s.String(cassette.AttrToolContractDigest),
				endpoint: s.String("server.address"),
			}
		}
	}
	if len(seen) == 0 {
		return report, nil
	}

	// One introspection per endpoint, not per tool.
	liveByEndpoint := map[string]map[string]string{}
	errByEndpoint := map[string]error{}
	for _, r := range seen {
		if r.endpoint == "" {
			continue
		}
		if _, done := liveByEndpoint[r.endpoint]; done {
			continue
		}
		if _, failed := errByEndpoint[r.endpoint]; failed {
			continue
		}
		tools, err := list(ctx, r.endpoint)
		if err != nil {
			errByEndpoint[r.endpoint] = err
			continue
		}
		contracts := map[string]string{}
		for _, t := range tools {
			d, err := t.ContractDigest()
			if err != nil {
				continue
			}
			contracts[t.Name] = d
		}
		liveByEndpoint[r.endpoint] = contracts
	}

	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		rec := seen[name]
		drift := ToolDrift{Tool: name, Recorded: rec.contract}

		switch {
		case rec.endpoint == "":
			drift.Status = DriftUnknown
			drift.Detail = "the recording does not say which server served this tool"
		case errByEndpoint[rec.endpoint] != nil:
			drift.Status = DriftUnknown
			drift.Detail = fmt.Sprintf("could not reach %s: %v", rec.endpoint, errByEndpoint[rec.endpoint])
		case rec.contract == "":
			drift.Status = DriftUnknown
			drift.Detail = "no contract was recorded for this tool, so there is nothing to compare"
		default:
			live, ok := liveByEndpoint[rec.endpoint][name]
			switch {
			case !ok:
				drift.Status = DriftRemoved
				drift.Detail = fmt.Sprintf("%s no longer advertises this tool", rec.endpoint)
			case live != rec.contract:
				drift.Status = DriftChanged
				drift.Live = live
				// The contract digest covers the description text as well as
				// the schema, so this is both a staleness signal and a
				// security one.
				drift.Detail = "the description or schema changed since this session was recorded"
			default:
				drift.Status = DriftNone
				drift.Live = live
			}
		}
		report.Tools = append(report.Tools, drift)
	}
	return report, nil
}
