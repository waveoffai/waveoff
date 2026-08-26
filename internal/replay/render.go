// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package replay

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/waveoffai/waveoff/internal/diff"
)

// SchemaVersion identifies the JSON report shape. CI will branch on it, so
// fields may be added but never removed or repurposed without a bump.
const SchemaVersion = "waveoff.ai/replay/v1alpha1"

// RenderJSON writes the machine-readable report.
func RenderJSON(w io.Writer, reports []*Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(map[string]any{
		"schemaVersion": SchemaVersion,
		"sessions":      reports,
		"summary":       summarise(reports),
	})
}

// Summary is the aggregate an operator reads first.
type Summary struct {
	Sessions int `json:"sessions"`
	Diverged int `json:"diverged"`
	Stale    int `json:"stale"`
	Unusable int `json:"unusable"`
	Refused  int `json:"refused"`
	NoOps    int `json:"noOps"`
}

func summarise(reports []*Report) Summary {
	s := Summary{Sessions: len(reports)}
	for _, r := range reports {
		if r.Diverged() {
			s.Diverged++
		}
		if r.Drift != nil && r.Drift.Stale() {
			s.Stale++
		}
		if ok, _ := r.Usable(); !ok {
			s.Unusable++
		}
		s.Refused += r.Refused
		s.NoOps += r.NoOps
	}
	return s
}

// RenderText writes the human-readable report.
//
// It is built for the same reader as waveoff diff: someone who needs to know
// whether a candidate did something different, and if so where. The first
// divergence is what leads, because everything after it is downstream.
func RenderText(w io.Writer, reports []*Report, p diff.Palette) error {
	b := &strings.Builder{}
	sum := summarise(reports)

	fmt.Fprintf(b, "\n  %sreplayed %d session(s)%s", p.Bold, sum.Sessions, p.Reset)
	if sum.Sessions > 0 {
		fmt.Fprintf(b, "      %s%d diverged · %d stale · %d unusable%s",
			p.Dim, sum.Diverged, sum.Stale, sum.Unusable, p.Reset)
	}
	b.WriteString("\n\n")

	for _, r := range reports {
		renderSession(b, r, p)
	}
	renderVerdict(b, sum, p)

	_, err := io.WriteString(w, b.String())
	return err
}

func renderSession(b *strings.Builder, r *Report, p diff.Palette) {
	marker, name := "  ", r.Session
	if r.Diverged() || (r.Drift != nil && r.Drift.Stale()) {
		marker = p.Red + "!" + p.Reset + " "
		name = p.Bold + name + p.Reset
	}
	fmt.Fprintf(b, "%s%s   %s%d steps · mode %s%s\n", marker, name, p.Dim, len(r.Steps), r.Mode, p.Reset)

	// Contract drift first: it decides whether anything below can be believed.
	if r.Drift != nil {
		for _, d := range r.Drift.Tools {
			switch d.Status {
			case DriftChanged, DriftRemoved:
				fmt.Fprintf(b, "      %s! contract %s  %s%s\n", p.Red, d.Status, d.Tool, p.Reset)
				fmt.Fprintf(b, "        %s↳ %s%s\n", p.Dim, d.Detail, p.Reset)
			case DriftUnknown:
				fmt.Fprintf(b, "      %s? contract unchecked  %s%s\n", p.Yellow, d.Tool, p.Reset)
				fmt.Fprintf(b, "        %s↳ %s%s\n", p.Dim, d.Detail, p.Reset)
			}
		}
	}

	if d := r.FirstDivergence(); d != nil {
		fmt.Fprintf(b, "      %sdiverged at step %d%s  %s\n", p.Bold, d.Step, p.Reset, d.Kind)
		if d.Recorded != "" {
			fmt.Fprintf(b, "        recorded  %s\n", d.Recorded)
		}
		if d.Replayed != "" {
			fmt.Fprintf(b, "        replayed  %s\n", d.Replayed)
		}
		if d.Detail != "" {
			fmt.Fprintf(b, "        %s↳ %s%s\n", p.Dim, d.Detail, p.Reset)
		}
		if n := len(r.Divergences) - 1; n > 0 {
			// Everything after the first departure is a consequence of it, so
			// it is counted rather than listed.
			fmt.Fprintf(b, "        %s↳ %d further difference(s), downstream of this one%s\n", p.Dim, n, p.Reset)
		}
	}

	if r.Refused > 0 {
		fmt.Fprintf(b, "      %s%d call(s) refused%s — the candidate could not complete this session\n",
			p.Yellow, r.Refused, p.Reset)
		for _, s := range r.Steps {
			if s.Decision.Action == ActionRefuse {
				fmt.Fprintf(b, "        %s↳ %s%s\n", p.Dim, s.Decision.Reason, p.Reset)
			}
		}
	}
	if r.NoOps > 0 {
		fmt.Fprintf(b, "      %s%d write(s) refused and synthesised%s\n", p.Dim, r.NoOps, p.Reset)
	}
	if !r.Diverged() && r.Refused == 0 {
		fmt.Fprintf(b, "      %sfollowed the recorded path%s\n", p.Dim, p.Reset)
	}
	b.WriteString("\n")
}

func renderVerdict(b *strings.Builder, s Summary, p diff.Palette) {
	switch {
	case s.Sessions == 0:
		fmt.Fprintf(b, "  %sno sessions matched%s\n\n", p.Dim, p.Reset)
	case s.Unusable == s.Sessions:
		fmt.Fprintf(b, "  %sno usable result%s — every session was stale or incomplete, "+
			"so nothing here can be gated on\n\n", p.Bold, p.Reset)
	case s.Diverged == 0:
		fmt.Fprintf(b, "  %sno divergence%s — the candidate followed the recorded path in "+
			"every usable session\n\n", p.Bold, p.Reset)
	default:
		fmt.Fprintf(b, "  %sdiverged%s — %d of %d session(s) departed from the recording\n\n",
			p.Bold, p.Reset, s.Diverged, s.Sessions)
	}
	if s.Stale > 0 {
		fmt.Fprintf(b, "  %s%d session(s) excluded: tool contracts changed since they were recorded, "+
			"so scoring them would measure a fiction.%s\n\n", p.Dim, s.Stale, p.Reset)
	}
}

// ExitCode maps a run onto a process exit status, on the same three-valued
// pattern as waveoff diff so CI can branch without parsing anything.
func ExitCode(reports []*Report) int {
	s := summarise(reports)
	switch {
	case s.Sessions == 0:
		return diff.ExitUsage
	case s.Unusable == s.Sessions:
		// Not "everything passed": nothing was actually measured.
		return 3
	case s.Diverged > 0:
		return 2
	default:
		return 0
	}
}
