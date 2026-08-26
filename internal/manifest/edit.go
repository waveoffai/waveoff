// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"bytes"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Editor rewrites individual scalars in a YAML file without reformatting it.
//
// A manifest is a git-committed artifact that people read in review, so
// `waveoff verify --write` must not reflow it. Re-encoding through yaml.v3
// preserves comments but drops every blank line, which turns a three-value
// repair into a whole-file diff. So the node tree is used only to locate the
// values, and the edit is applied to the original bytes.
type Editor struct {
	lines []string
	eol   string
	docs  []*Document
}

// Document is one YAML document in the file.
type Document struct {
	node *yaml.Node
	// IsManifest reports whether this document is an AgentManifest.
	IsManifest bool
	// Name is metadata.name as it currently reads.
	Name string
}

// NewEditor parses a file for editing.
func NewEditor(src []byte) (*Editor, error) {
	eol := "\n"
	if bytes.Contains(src, []byte("\r\n")) {
		eol = "\r\n"
	}
	e := &Editor{
		lines: strings.Split(strings.ReplaceAll(string(src), "\r\n", "\n"), "\n"),
		eol:   eol,
	}

	dec := yaml.NewDecoder(bytes.NewReader(src))
	for {
		var n yaml.Node
		err := dec.Decode(&n)
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			return nil, err
		}
		d := &Document{node: &n}
		if root := contentRoot(&n); root != nil {
			d.IsManifest = scalarAt(root, "kind") == "AgentManifest"
			if meta := mappingAt(root, "metadata"); meta != nil {
				d.Name = scalarAt(meta, "name")
			}
		}
		e.docs = append(e.docs, d)
	}
	return e, nil
}

// Documents returns the parsed documents in file order.
func (e *Editor) Documents() []*Document { return e.docs }

// Bytes renders the file back out.
func (e *Editor) Bytes() []byte {
	return []byte(strings.Join(e.lines, e.eol))
}

// SetScalar replaces the value at a dotted path, or inserts the key if it is
// absent. Only the affected line changes.
func (e *Editor) SetScalar(d *Document, path string, value string) error {
	parts := strings.Split(path, ".")
	root := contentRoot(d.node)
	if root == nil {
		return fmt.Errorf("document is not a mapping")
	}

	parent := root
	for _, key := range parts[:len(parts)-1] {
		next := mappingAt(parent, key)
		if next == nil {
			return fmt.Errorf("no %q section to write %s into", key, path)
		}
		parent = next
	}
	leaf := parts[len(parts)-1]

	keyNode, valNode := entry(parent, leaf)
	if keyNode == nil {
		return e.insert(parent, leaf, value)
	}
	return e.replace(valNode, keyNode, value)
}

// replace rewrites the value in place, keeping indentation and any trailing
// comment. A TODO marker next to a blank digest is information the operator put
// there; the repair should not silently delete it.
func (e *Editor) replace(val, key *yaml.Node, value string) error {
	idx := val.Line - 1
	if idx < 0 || idx >= len(e.lines) {
		return fmt.Errorf("line %d is out of range", val.Line)
	}
	line := e.lines[idx]
	start := val.Column - 1
	if start > len(line) {
		return fmt.Errorf("column %d is past the end of line %d", val.Column, val.Line)
	}
	// A multi-line value is not something this tool writes, and rewriting one
	// by line surgery would corrupt the file.
	if val.Style == yaml.LiteralStyle || val.Style == yaml.FoldedStyle {
		return fmt.Errorf("%s is a block scalar; edit it by hand", key.Value)
	}

	tail := ""
	if c := strings.Index(line[start:], " #"); c >= 0 {
		tail = line[start+c:]
	}
	// A TODO next to a field this command exists to fill in is resolved by
	// filling it in. Leaving it behind means the next reader cannot tell which
	// TODOs are still outstanding.
	if isResolvedTODO(tail) {
		tail = ""
	}
	e.lines[idx] = strings.TrimRight(line[:start]+value+tail, " ")
	return nil
}

// insert adds a key that was not there. It goes immediately after the parent's
// last existing entry so that surrounding lines keep their line numbers, which
// keeps every other node's recorded position valid.
func (e *Editor) insert(parent *yaml.Node, key, value string) error {
	if len(parent.Content) == 0 {
		return fmt.Errorf("cannot add %q to an empty mapping", key)
	}
	last := parent.Content[len(parent.Content)-1]
	indent := parent.Content[0].Column - 1
	at := lastLine(last)
	if at < 0 || at > len(e.lines) {
		return fmt.Errorf("cannot locate where to insert %q", key)
	}
	line := strings.Repeat(" ", indent) + key + ": " + value
	e.lines = append(e.lines[:at], append([]string{line}, e.lines[at:]...)...)

	// Every node below the insertion point has shifted down by one line, so the
	// cached positions are now wrong. Re-parsing is cheaper than maintaining an
	// offset map, and inserts are rare.
	return e.reparse()
}

func (e *Editor) reparse() error {
	next, err := NewEditor(e.Bytes())
	if err != nil {
		return err
	}
	// Carry the freshly-parsed nodes onto the existing Document values so that
	// callers holding a *Document keep working across an insert.
	if len(next.docs) != len(e.docs) {
		return fmt.Errorf("document count changed during edit")
	}
	for i := range e.docs {
		e.docs[i].node = next.docs[i].node
		e.docs[i].Name = next.docs[i].Name
		e.docs[i].IsManifest = next.docs[i].IsManifest
	}
	e.lines = next.lines
	return nil
}

// lastLine finds the final source line a node occupies, so an insert lands
// after it.
func lastLine(n *yaml.Node) int {
	end := n.Line
	var walk func(*yaml.Node)
	walk = func(x *yaml.Node) {
		if x.Line > end {
			end = x.Line
		}
		for _, c := range x.Content {
			walk(c)
		}
	}
	walk(n)
	return end
}

// isResolvedTODO reports whether a trailing comment is a note to fill in the
// value that was just filled in. Any other comment is the author's and stays.
func isResolvedTODO(tail string) bool {
	c := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(tail), "#"))
	return strings.HasPrefix(strings.ToLower(c), "todo")
}

func contentRoot(n *yaml.Node) *yaml.Node {
	if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
		n = n.Content[0]
	}
	if n.Kind != yaml.MappingNode {
		return nil
	}
	return n
}

func entry(m *yaml.Node, key string) (keyNode, valNode *yaml.Node) {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil, nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i], m.Content[i+1]
		}
	}
	return nil, nil
}

func mappingAt(m *yaml.Node, key string) *yaml.Node {
	_, v := entry(m, key)
	if v != nil && v.Kind == yaml.MappingNode {
		return v
	}
	return nil
}

func scalarAt(m *yaml.Node, key string) string {
	_, v := entry(m, key)
	if v == nil {
		return ""
	}
	return v.Value
}
