// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/waveoffai/waveoff/api/v1alpha1"
	"github.com/waveoffai/waveoff/internal/diff"
	"github.com/waveoffai/waveoff/internal/digest"
	"github.com/waveoffai/waveoff/internal/manifest"
)

func newVerifyCmd() *cobra.Command {
	var (
		write   bool
		explain bool
	)

	cmd := &cobra.Command{
		Use:   "verify <file>...",
		Short: "Check that a manifest's digests and name match its contents",
		Long: `Recompute behaviorDigest and contentDigest from a manifest's spec and compare
them against what the file claims, then check that metadata.name is derived from
the agent name and the behaviour digest.

No cluster is required. This is what CI and a pre-commit hook should call: the
admission webhook verifies digests but never computes them, so a manifest whose
digests are stale is rejected at apply time rather than at review time.

  --write    repair the digests and name in place, preserving comments
  --explain  print the exact bytes each digest is computed over

Exits non-zero if any manifest is wrong and --write was not given.`,
		Example: `  waveoff verify manifest.yaml
  waveoff verify --write manifest.yaml
  waveoff verify --explain manifest.yaml`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			bad := 0
			for _, path := range args {
				n, err := verifyFile(out, path, write, explain)
				if err != nil {
					return exitWith(diff.ExitUsage, err)
				}
				bad += n
			}
			if bad > 0 && !write {
				_, _ = fmt.Fprintf(out, "\n%d manifest(s) need repair. Run: waveoff verify --write %s\n",
					bad, args[0])
				return VerdictExit{Code: 1}
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&write, "write", false, "repair digests and name in place")
	cmd.Flags().BoolVar(&explain, "explain", false, "print the canonical bytes each digest covers")
	return cmd
}

func verifyFile(out io.Writer, path string, write, explain bool) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}

	// Two passes over the same bytes: the typed decode gives us something to
	// hash, the node decode gives us something to edit without reformatting.
	typed, err := manifest.ReadAll(bytes.NewReader(raw))
	if err != nil {
		return 0, fmt.Errorf("%s: %w", path, err)
	}
	ed, err := manifest.NewEditor(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", path, err)
	}

	byName := map[string]*manifest.Document{}
	for _, d := range ed.Documents() {
		if d.IsManifest {
			byName[d.Name] = d
		}
	}

	bad, repaired := 0, 0
	for _, m := range typed {
		problems, wantB, wantC := check(m)
		if len(problems) == 0 {
			_, _ = fmt.Fprintf(out, "ok       %s  %s\n", path, m.Name)
			if explain {
				explainDigests(out, &m.Spec)
			}
			continue
		}

		if write {
			d, ok := byName[m.Name]
			if !ok {
				return 0, fmt.Errorf("%s: cannot locate document %q to repair", path, m.Name)
			}
			name := digest.Name(m.Spec.Agent, wantB)
			if isDisambiguated(m.Name, m.Spec.Agent, wantB) {
				// Keep an operator's deliberate disambiguation rather than
				// collapsing it back onto a name that is already taken.
				name = digest.DisambiguatedName(m.Spec.Agent, wantB, wantC)
			}
			for _, edit := range []struct{ path, value string }{
				{"spec.behaviorDigest", wantB},
				{"spec.contentDigest", wantC},
				{"metadata.name", name},
			} {
				if err := ed.SetScalar(d, edit.path, edit.value); err != nil {
					return 0, fmt.Errorf("%s: %s: %w", path, edit.path, err)
				}
			}
			repaired++
			_, _ = fmt.Fprintf(out, "repaired %s  %s\n", path, name)
			continue
		}

		bad++
		_, _ = fmt.Fprintf(out, "FAILED   %s  %s\n", path, m.Name)
		for _, p := range problems {
			_, _ = fmt.Fprintf(out, "           %s\n", p)
		}
		if explain {
			explainDigests(out, &m.Spec)
		}
	}

	if repaired > 0 {
		// Write through a temporary file in the same directory so an
		// interrupted repair cannot leave a half-written manifest behind.
		info, err := os.Stat(path)
		if err != nil {
			return 0, err
		}
		f, err := os.CreateTemp(filepath.Dir(path), ".waveoff-*")
		if err != nil {
			return 0, err
		}
		tmp := f.Name()
		defer os.Remove(tmp)
		if _, err := f.Write(ed.Bytes()); err != nil {
			f.Close()
			return 0, err
		}
		if err := f.Close(); err != nil {
			return 0, err
		}
		if err := os.Chmod(tmp, info.Mode().Perm()); err != nil {
			return 0, err
		}
		if err := os.Rename(tmp, path); err != nil {
			return 0, err
		}
	}
	return bad, nil
}

// check recomputes everything the admission webhook will recompute, so that a
// manifest passing `waveoff verify` is a manifest the cluster will accept.
func check(m *v1alpha1.AgentManifest) (problems []string, wantB, wantC string) {
	wantB, wantC, err := digest.Both(&m.Spec)
	if err != nil {
		return []string{err.Error()}, "", ""
	}
	if m.Spec.BehaviorDigest != wantB {
		problems = append(problems, fmt.Sprintf("behaviorDigest\n             states   %s\n             computed %s",
			orUnset(m.Spec.BehaviorDigest), wantB))
	}
	if m.Spec.ContentDigest != wantC {
		problems = append(problems, fmt.Sprintf("contentDigest\n             states   %s\n             computed %s",
			orUnset(m.Spec.ContentDigest), wantC))
	}
	if want := digest.Name(m.Spec.Agent, wantB); m.Name != want && !isDisambiguated(m.Name, m.Spec.Agent, wantB) {
		problems = append(problems, fmt.Sprintf("metadata.name\n             states   %s\n             expected %s",
			orUnset(m.Name), want))
	}
	return problems, wantB, wantC
}

// isDisambiguated allows the second legal name form, used when two manifests
// share a behaviorDigest but differ in content — the registry-migration case,
// where both objects must exist under different names.
func isDisambiguated(name, agent, behavior string) bool {
	prefix := digest.Name(agent, behavior) + "."
	return len(name) > len(prefix) && name[:len(prefix)] == prefix
}

func explainDigests(out io.Writer, spec *v1alpha1.AgentManifestSpec) {
	for _, scope := range []digest.Scope{digest.ScopeBehavior, digest.ScopeContent} {
		canonical, err := digest.CanonicalJSON(spec, scope)
		if err != nil {
			_, _ = fmt.Fprintf(out, "           %s: %v\n", scope, err)
			continue
		}
		_, _ = fmt.Fprintf(out, "           %sDigest covers:\n           %s\n", scope, canonical)
	}
}

func orUnset(s string) string {
	if s == "" {
		return "(unset)"
	}
	return s
}
