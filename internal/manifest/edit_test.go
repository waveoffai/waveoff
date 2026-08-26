// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"os"
	"strings"
	"testing"
)

const src = `apiVersion: waveoff.ai/v1alpha1
kind: AgentManifest
metadata:
  name: PLACEHOLDER          # filled in by waveoff verify --write
  namespace: default
spec:
  agent: support-agent

  # Both digests are authored, not computed at admission.
  behaviorDigest: ""         # TODO: waveoff verify --write
  contentDigest: ""

  code:
    image: registry.internal/support-agent@sha256:aaa

  tools:
    - name: docs.search
      effect: read

    - name: jira.create
      effect: write
`

func editorFor(t *testing.T, s string) (*Editor, *Document) {
	t.Helper()
	e, err := NewEditor([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	docs := e.Documents()
	if len(docs) != 1 || !docs[0].IsManifest {
		t.Fatalf("expected one AgentManifest, got %d documents", len(docs))
	}
	return e, docs[0]
}

// TestEditTouchesOnlyTargetLines is the whole reason this editor exists. A
// manifest is a git-committed artifact people read in review, so repairing
// three values must not reflow the file — and re-encoding through yaml.v3
// silently drops every blank line.
func TestEditTouchesOnlyTargetLines(t *testing.T) {
	e, d := editorFor(t, src)
	for _, edit := range []struct{ path, value string }{
		{"spec.behaviorDigest", "sha256:bbb"},
		{"spec.contentDigest", "sha256:ccc"},
		{"metadata.name", "support-agent-000000000000"},
	} {
		if err := e.SetScalar(d, edit.path, edit.value); err != nil {
			t.Fatalf("%s: %v", edit.path, err)
		}
	}

	before := strings.Split(src, "\n")
	after := strings.Split(string(e.Bytes()), "\n")
	if len(before) != len(after) {
		t.Fatalf("line count changed: %d → %d", len(before), len(after))
	}
	changed := 0
	for i := range before {
		if before[i] != after[i] {
			changed++
		}
	}
	if changed != 3 {
		t.Errorf("%d lines changed, want exactly 3:\n%s", changed, e.Bytes())
	}
}

func TestEditPreservesCommentsAndBlankLines(t *testing.T) {
	e, d := editorFor(t, src)
	if err := e.SetScalar(d, "metadata.name", "support-agent-000000000000"); err != nil {
		t.Fatal(err)
	}
	out := string(e.Bytes())

	if !strings.Contains(out, "# Both digests are authored") {
		t.Error("a standalone comment was lost")
	}
	if !strings.Contains(out, "support-agent-000000000000 # filled in by waveoff verify --write") {
		t.Errorf("the trailing comment on an edited line was lost:\n%s", out)
	}
	if !strings.Contains(out, "spec:\n  agent: support-agent\n\n  #") {
		t.Error("a blank line was lost")
	}
}

// TestResolvedTODOIsDropped: a TODO next to a field this command exists to fill
// in is resolved by filling it in. Leaving it behind means the next reader
// cannot tell which TODOs are still outstanding.
func TestResolvedTODOIsDropped(t *testing.T) {
	e, d := editorFor(t, src)
	if err := e.SetScalar(d, "spec.behaviorDigest", "sha256:bbb"); err != nil {
		t.Fatal(err)
	}
	out := string(e.Bytes())
	if strings.Contains(out, "TODO: waveoff verify --write") {
		t.Errorf("a resolved TODO survived:\n%s", out)
	}
	// A comment that is not a TODO is the author's and must stay.
	if !strings.Contains(out, "# filled in by waveoff verify --write") {
		t.Error("a non-TODO comment was dropped")
	}
}

func TestInsertsAbsentKey(t *testing.T) {
	missing := strings.ReplaceAll(src, `  contentDigest: ""`+"\n", "")
	e, d := editorFor(t, missing)
	if err := e.SetScalar(d, "spec.contentDigest", "sha256:ccc"); err != nil {
		t.Fatal(err)
	}
	out := string(e.Bytes())
	if !strings.Contains(out, "contentDigest: sha256:ccc") {
		t.Errorf("the key was not inserted:\n%s", out)
	}
	// And the file must still parse, with the other values intact.
	e2, d2 := editorFor(t, out)
	if err := e2.SetScalar(d2, "spec.behaviorDigest", "sha256:zzz"); err != nil {
		t.Fatalf("file did not survive the insert: %v", err)
	}
}

func TestEditsSurviveAnInsert(t *testing.T) {
	// After an insert every cached node position below it shifts, so a
	// subsequent edit through the same Document must still land correctly.
	missing := strings.ReplaceAll(src, `  behaviorDigest: ""         # TODO: waveoff verify --write`+"\n", "")
	e, d := editorFor(t, missing)
	if err := e.SetScalar(d, "spec.behaviorDigest", "sha256:bbb"); err != nil {
		t.Fatal(err)
	}
	if err := e.SetScalar(d, "spec.contentDigest", "sha256:ccc"); err != nil {
		t.Fatal(err)
	}
	out := string(e.Bytes())
	if !strings.Contains(out, "contentDigest: sha256:ccc") {
		t.Errorf("the edit after an insert landed in the wrong place:\n%s", out)
	}
	if strings.Count(out, "sha256:bbb") != 1 {
		t.Errorf("inserted value appears %d times:\n%s", strings.Count(out, "sha256:bbb"), out)
	}
}

func TestPreservesCRLF(t *testing.T) {
	e, d := editorFor(t, strings.ReplaceAll(src, "\n", "\r\n"))
	if err := e.SetScalar(d, "metadata.name", "x-000000000000"); err != nil {
		t.Fatal(err)
	}
	out := string(e.Bytes())
	if strings.Contains(strings.ReplaceAll(out, "\r\n", ""), "\n") {
		t.Error("CRLF line endings were not preserved")
	}
}

func TestRefusesBlockScalar(t *testing.T) {
	block := `kind: AgentManifest
metadata:
  name: |
    multi
    line
spec:
  agent: x
`
	e, d := editorFor(t, block)
	// Rewriting a block scalar by line surgery would corrupt the file, so this
	// must fail loudly rather than produce something that half-parses.
	if err := e.SetScalar(d, "metadata.name", "x"); err == nil {
		t.Error("a block scalar was rewritten by line surgery")
	}
}

func TestMultiDocument(t *testing.T) {
	two := src + "---\n" + strings.ReplaceAll(src, "support-agent", "billing-agent")
	e, err := NewEditor([]byte(two))
	if err != nil {
		t.Fatal(err)
	}
	docs := e.Documents()
	if len(docs) != 2 {
		t.Fatalf("got %d documents, want 2", len(docs))
	}
	if err := e.SetScalar(docs[1], "spec.behaviorDigest", "sha256:second"); err != nil {
		t.Fatal(err)
	}
	out := string(e.Bytes())
	if strings.Count(out, "sha256:second") != 1 {
		t.Errorf("the edit hit the wrong document:\n%s", out)
	}
	if !strings.Contains(strings.SplitN(out, "---", 2)[1], "sha256:second") {
		t.Error("the edit landed in the first document instead of the second")
	}
}

func TestIgnoresOtherKinds(t *testing.T) {
	mixed := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm\n---\n" + src
	e, err := NewEditor([]byte(mixed))
	if err != nil {
		t.Fatal(err)
	}
	manifests := 0
	for _, d := range e.Documents() {
		if d.IsManifest {
			manifests++
		}
	}
	if manifests != 1 {
		t.Errorf("found %d manifests among mixed kinds, want 1", manifests)
	}
}

func TestParseRef(t *testing.T) {
	dir := t.TempDir()
	file := dir + "/manifest.yaml"
	if err := writeFile(file, src); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		arg      string
		wantPath string
		wantName string
	}{
		{"-", "-", ""},
		{file, file, ""},
		{file + "#support-agent-abc", file, "support-agent-abc"},
		{"support-agent-a875c0c15289", "", "support-agent-a875c0c15289"},
		// Looks like a path but is not on disk: say so, rather than searching
		// the cluster for an object named "./missing.yaml".
		{"./missing.yaml", "./missing.yaml", ""},
	}
	for _, tc := range cases {
		got := ParseRef(tc.arg, "")
		if got.Path != tc.wantPath || got.Name != tc.wantName {
			t.Errorf("ParseRef(%q) = {Path:%q Name:%q}, want {Path:%q Name:%q}",
				tc.arg, got.Path, got.Name, tc.wantPath, tc.wantName)
		}
	}
}

func TestReadAllSkipsForeignKinds(t *testing.T) {
	mixed := "apiVersion: v1\nkind: Service\nmetadata:\n  name: s\n---\n" + src
	got, err := ReadAll(strings.NewReader(mixed))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Spec.Agent != "support-agent" {
		t.Fatalf("got %d manifests: %+v", len(got), got)
	}
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}
