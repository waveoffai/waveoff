// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/waveoffai/waveoff/api/v1alpha1"
	"github.com/waveoffai/waveoff/internal/diff"
	"github.com/waveoffai/waveoff/internal/manifest"
)

func newDiffCmd() *cobra.Command {
	var (
		output  string
		noColor bool
		force   bool
	)

	cmd := &cobra.Command{
		Use:   "diff <a> <b>",
		Short: "Show which of an agent's behavioural inputs changed",
		Long: `Compare two AgentManifests and report what moved, grouped by behavioural
plane, with the planes that did not change named explicitly.

Each side may be an object in the cluster, a file, one document out of a
multi-document file (file.yaml#name), or "-" for stdin.

Exit codes:
   0  identical            both digests match
   1  provenance only      behaviourally identical, promotes without a canary
   2  behavioural change   this needs a canary
  64  usage error, including a refused comparison across two different agents
  69  cluster unreachable
  70  internal error`,
		Example: `  waveoff diff support-agent-6b1d4e8f2a01 support-agent-7f3a9c2b4d10
  waveoff diff old.yaml new.yaml
  waveoff diff support-agent-6b1d4e8f2a01 candidate.yaml -o json`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ns, _ := cmd.Flags().GetString("namespace")
			ctx := cmd.Context()

			refA := manifest.ParseRef(args[0], ns)
			refB := manifest.ParseRef(args[1], ns)

			a, err := manifest.Load(ctx, refA)
			if err != nil {
				return loadErr(err)
			}
			b, err := manifest.Load(ctx, refB)
			if err != nil {
				return loadErr(err)
			}

			r, err := diff.Compare(&a.Spec, &b.Spec, diff.Options{
				NameA:           displayName(a, refA),
				NameB:           displayName(b, refB),
				Bodies:          localBodies(refA, refB),
				AllowCrossAgent: force,
			})
			if err != nil {
				return exitWith(diff.ExitInternal, err)
			}

			switch output {
			case "json":
				if err := diff.RenderJSON(cmd.OutOrStdout(), r); err != nil {
					return exitWith(diff.ExitInternal, err)
				}
			case "text":
				if err := diff.RenderText(cmd.OutOrStdout(), r, palette(noColor)); err != nil {
					return exitWith(diff.ExitInternal, err)
				}
			default:
				return fmt.Errorf("unknown output format %q: use text or json", output)
			}
			return VerdictExit{Code: r.Verdict.ExitCode()}
		},
	}

	cmd.Flags().StringVarP(&output, "output", "o", "text", "output format: text or json")
	cmd.Flags().BoolVar(&noColor, "no-color", false, "disable colour (also honours NO_COLOR)")
	cmd.Flags().BoolVar(&force, "force", false, "compare manifests of two different agents anyway")
	return cmd
}

func displayName(m *v1alpha1.AgentManifest, r manifest.Ref) string {
	if m.Name != "" {
		return m.Name
	}
	return r.Raw
}

// localBodies resolves prompt text when — and only when — both prompts point at
// files reachable from here. A diff must never fail because a git remote is
// down, so anything else degrades to showing digests.
func localBodies(refs ...manifest.Ref) diff.BodyLookup {
	var dirs []string
	for _, r := range refs {
		if r.IsFile() && r.Path != "-" {
			dirs = append(dirs, filepath.Dir(r.Path))
		}
	}
	if len(dirs) == 0 {
		return nil
	}
	return func(p v1alpha1.PromptRef) (string, bool) {
		src := strings.TrimPrefix(p.Source, "file://")
		if src == "" || strings.Contains(src, "://") {
			return "", false
		}
		candidates := []string{src}
		if !filepath.IsAbs(src) {
			candidates = nil
			for _, d := range dirs {
				candidates = append(candidates, filepath.Join(d, src))
			}
		}
		for _, c := range candidates {
			if data, err := os.ReadFile(c); err == nil {
				return string(data), true
			}
		}
		return "", false
	}
}

func loadErr(err error) error {
	if strings.Contains(err.Error(), manifest.ErrNoCluster.Error()) {
		return exitWith(diff.ExitUnavailable, err)
	}
	return exitWith(diff.ExitUsage, err)
}

// exitWith prints the message and selects a non-verdict exit code.
func exitWith(code int, err error) error {
	fmt.Fprintln(os.Stderr, "error:", err)
	return VerdictExit{Code: code}
}
