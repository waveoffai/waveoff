// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package diff

import (
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// Palette lets the renderer be colourless without threading a flag through
// every call. Colour is an accent on severity, never the only carrier of it —
// every marked line also carries a literal "!" so the output survives being
// piped into a ticket.
type Palette struct {
	Red, Yellow, Dim, Bold, Reset string
}

// NoColor is the palette used when stdout is not a terminal, when NO_COLOR is
// set, or when --no-color is passed.
var NoColor = Palette{}

// Color is the default terminal palette.
var Color = Palette{
	Red:    "\x1b[31m",
	Yellow: "\x1b[33m",
	Dim:    "\x1b[2m",
	Bold:   "\x1b[1m",
	Reset:  "\x1b[0m",
}

const (
	markerCol = 2  // width of the "! " severity gutter
	planeCol  = 11 // width of the plane-name column
	fieldCol  = 13 // width of the field-name column
)

// group is one element's worth of changes — a single tool, prompt or judge, or
// on planes without elements, the whole plane.
type group struct {
	element string
	changes []Change
}

func groupChanges(in []Change) []group {
	var out []group
	for _, c := range in {
		if n := len(out); n > 0 && out[n-1].element == c.Element {
			out[n-1].changes = append(out[n-1].changes, c)
			continue
		}
		out = append(out, group{element: c.Element, changes: []Change{c}})
	}
	return out
}

func renderPlane(b *strings.Builder, r *Result, plane Plane, p Palette) {
	var mine []Change
	severe := false
	for _, c := range r.Changes {
		if c.Plane == plane {
			mine = append(mine, c)
			severe = severe || c.Severity
		}
	}

	gutter, name := "  ", pad(plane.String(), planeCol)
	if severe {
		gutter = p.Red + "!" + p.Reset + " "
		name = p.Bold + plane.String() + p.Reset + strings.Repeat(" ", max(1, planeCol-len(plane.String())))
	}
	b.WriteString(gutter + name)

	planeIndent := strings.Repeat(" ", markerCol+planeCol)
	firstLine := true

	for _, g := range groupChanges(mine) {
		if g.element == "" {
			// Scalar plane: fields hang directly off the plane name, and the
			// consequence annotation is written once for the whole plane
			// rather than repeated after every field.
			for _, c := range g.changes {
				if !firstLine {
					b.WriteString(planeIndent)
				}
				firstLine = false
				fmt.Fprintf(b, "%s %s → %s\n", pad(c.Field, fieldCol), c.From, c.To)
			}
			writeAnnotations(b, g.changes, planeIndent+strings.Repeat(" ", fieldCol+1), p)
			continue
		}

		if !firstLine {
			b.WriteString(planeIndent)
		}
		firstLine = false

		// A group whose single change carries no field name is an element
		// whose whole value moved — a prompt is its body — so it reads better
		// on one line than split across two.
		if len(g.changes) == 1 && g.changes[0].Field == "" && g.changes[0].Op == OpChanged {
			c := g.changes[0]
			fmt.Fprintf(b, "%s %s → %s\n", pad(c.element(), fieldCol+8), c.From, c.To)
			writeAnnotations(b, g.changes, planeIndent+"  ", p)
			continue
		}

		fmt.Fprintf(b, "%s%s\n", g.element, opSuffix(g.changes[0], p))
		inner := planeIndent + "  "
		for _, c := range g.changes {
			if c.Op != OpChanged || (c.From == "" && c.To == "") {
				continue
			}
			fmt.Fprintf(b, "%s%s %s → %s\n", inner, pad(c.Field, fieldCol), c.From, c.To)
		}
		writeAnnotations(b, g.changes, inner, p)
	}
	b.WriteString("\n")
}

// writeAnnotations emits the details and one consequence line for a group. The
// tags are unioned across the group's changes: repeating "affects model output"
// after every parameter turns the signal into wallpaper.
func writeAnnotations(b *strings.Builder, changes []Change, indent string, p Palette) {
	worst := Change{}
	seen := map[Tag]bool{}
	for _, c := range changes {
		for _, d := range c.Detail {
			fmt.Fprintf(b, "%s%s↳ %s%s\n", indent, dimOrRed(c, p), d, p.Reset)
		}
		for _, t := range c.Tags {
			if !seen[t] {
				seen[t] = true
				worst.Tags = append(worst.Tags, t)
			}
		}
	}
	if line := tagLine(worst, p); line != "" {
		fmt.Fprintf(b, "%s%s↳ %s%s\n", indent, dimOrRed(worst, p), line, p.Reset)
	}
}

func opSuffix(c Change, p Palette) string {
	switch c.Op {
	case OpAdded:
		return "  " + p.Bold + "ADDED" + p.Reset
	case OpRemoved:
		return "  " + p.Bold + "REMOVED" + p.Reset
	}
	return ""
}

func dimOrRed(c Change, p Palette) string {
	if c.has(TagSecurity) {
		return p.Red
	}
	if c.has(TagGate) {
		return p.Yellow
	}
	return p.Dim
}

// tagLine renders the consequence annotation. Tags are ordered by how much they
// should alarm the reader, not alphabetically.
func tagLine(c Change, p Palette) string {
	order := []Tag{TagInput, TagModel, TagExec, TagGate, TagSecurity}
	var parts []string
	for _, t := range order {
		if c.has(t) {
			parts = append(parts, t.Explain())
		}
	}
	return strings.Join(parts, " · ")
}

func renderVerdict(b *strings.Builder, r *Result, p Palette) {
	switch r.Verdict {
	case VerdictIdentical:
		fmt.Fprintf(b, "  %sidentical%s — both digests match\n\n", p.Bold, p.Reset)
	case VerdictProvenanceOnly:
		// The case the dual digest exists to serve. Say the consequence, not
		// the mechanism: the reader wants to know whether this needs a canary.
		fmt.Fprintf(b, "  %sprovenance only%s — behaviourally identical, promotes without a canary\n\n",
			p.Bold, p.Reset)
	default:
		reach, gate := r.ReachModel(), r.ChangeGate()
		fmt.Fprintf(b, "  %sbehavioural change%s — %s, %s\n\n", p.Bold, p.Reset,
			plural(reach, "change reaches the model", "changes reach the model"),
			plural(gate, "change moves the gate itself", "changes move the gate itself"))
	}
}

func renderCrossAgent(w io.Writer, r *Result, p Palette) error {
	// Refusing is the useful behaviour. A cross-agent comparison produces a
	// verdict-shaped answer that means nothing, and someone will act on it.
	_, err := fmt.Fprintf(w, "\n  %s%srefusing to diff across agents%s\n\n"+
		"  left  is a version of %q\n  right is a version of %q\n\n"+
		"  These are different agents, so \"what changed\" has no meaningful answer\n"+
		"  and neither does the verdict. Pass --force if you meant to compare them\n"+
		"  anyway.\n\n", p.Red, p.Bold, p.Reset, r.AgentA, r.AgentB)
	return err
}

func shortName(r *Result, name, digest string) string {
	if name != "" {
		return name
	}
	if strings.HasPrefix(digest, "sha256:") && len(digest) > 19 {
		return digest[7:19]
	}
	return digest
}

// pad is rune-aware: "κ" is two bytes but one column, and byte-based padding
// silently misaligns every table that mentions it.
func pad(s string, n int) string {
	w := utf8.RuneCountInString(s)
	if w >= n {
		return s
	}
	return s + strings.Repeat(" ", n-w)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// RenderText writes the plane-grouped view: a fixed plane order so the eye
// lands in the same place every time, one consequence annotation per element
// rather than per field, and a verdict line at the bottom that says whether
// this needs a canary.
func RenderText(w io.Writer, r *Result, p Palette) error {
	if r.Verdict == VerdictCrossAgent {
		return renderCrossAgent(w, r, p)
	}

	b := &strings.Builder{}
	fmt.Fprintf(b, "\n  %s%s → %s%s", p.Bold, shortName(r, r.NameA, r.BehaviorDigest.A),
		shortName(r, r.NameB, r.BehaviorDigest.B), p.Reset)

	changed, unchanged := r.ChangedPlanes(), r.UnchangedPlanes()
	fmt.Fprintf(b, "      %s%d planes changed · %d unchanged%s\n\n",
		p.Dim, len(changed), len(unchanged), p.Reset)

	if len(r.Changes) == 0 {
		fmt.Fprintf(b, "  %sno differences in any behavioural input%s\n\n", p.Dim, p.Reset)
	}
	for _, plane := range changed {
		renderPlane(b, r, plane, p)
	}

	if len(unchanged) > 0 {
		names := make([]string, 0, len(unchanged))
		for _, pl := range unchanged {
			names = append(names, pl.String())
		}
		// Naming what was checked and found equal is not padding. "retrieval is
		// unchanged" is a different fact from "retrieval is missing from this
		// output", and at 2am the difference matters.
		fmt.Fprintf(b, "  %s%s%s%s\n\n", pad("unchanged", planeCol), p.Dim,
			strings.Join(names, " · "), p.Reset)
	}

	renderVerdict(b, r, p)
	_, err := io.WriteString(w, b.String())
	return err
}
