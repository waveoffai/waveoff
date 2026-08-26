// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

	"github.com/waveoffai/waveoff/api/v1alpha1"
	"github.com/waveoffai/waveoff/internal/analysis"
	"github.com/waveoffai/waveoff/internal/cas"
	"github.com/waveoffai/waveoff/internal/corpus"
	"github.com/waveoffai/waveoff/internal/diff"
	"github.com/waveoffai/waveoff/internal/replay"
	"github.com/waveoffai/waveoff/internal/rollout"
	"github.com/waveoffai/waveoff/internal/score"
)

func newRolloutCmd() *cobra.Command {
	var (
		corpusDir string
		blobDir   string
		outputDir string
		modelUp   string
		output    string
		noColor   bool
		skipDrift bool
		agentCmd  []string
	)

	cmd := &cobra.Command{
		Use:   "rollout <rollout.yaml>",
		Short: "Run a rollout's gates without a cluster",
		Long: `Run the stages of an AgentRollout locally and report the verdict.

The same file works here and in a cluster. incumbentRef and candidateRef are
resolved the way every other command resolves a manifest — as a local file, one
document from a multi-document file, or an object in the cluster — so a gate can
be tried on a laptop before anything is applied.

Nothing here touches production. Every session comes from a recording, every
tool read is served from that recording, and every write is refused.

Exit codes:
   0  promoted
   2  waved off
   3  held — the gate could not decide, or could not run
  64  usage error`,
		Example: `  waveoff rollout rollout.yaml \
    --corpus ./corpus --blobs ./blobs \
    --model-upstream https://api.anthropic.com \
    -- python agent.py`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			spec, err := loadRollout(args[0])
			if err != nil {
				return exitWith(diff.ExitUsage, err)
			}

			ns, _ := cmd.Flags().GetString("namespace")
			incumbent, err := loadCandidate(cmd.Context(), refIn(spec.IncumbentRef, ns))
			if err != nil {
				return exitWith(diff.ExitUsage, fmt.Errorf("incumbent: %w", err))
			}
			candidate, err := loadCandidate(cmd.Context(), refIn(spec.CandidateRef, ns))
			if err != nil {
				return exitWith(diff.ExitUsage, fmt.Errorf("candidate: %w", err))
			}

			recordings, err := corpus.NewFS(corpusDir)
			if err != nil {
				return exitWith(diff.ExitUsage, err)
			}
			var blobs cas.Store
			if blobDir != "" {
				b, err := cas.NewFS(blobDir)
				if err != nil {
					return exitWith(diff.ExitUsage, err)
				}
				blobs = b
			}
			if outputDir == "" {
				outputDir, err = os.MkdirTemp("", "waveoff-replays-")
				if err != nil {
					return exitWith(diff.ExitInternal, err)
				}
			}
			outputs, err := corpus.NewFS(outputDir)
			if err != nil {
				return exitWith(diff.ExitUsage, err)
			}

			var results []*rollout.StageResult
			for _, stage := range spec.Stages {
				if stage.Mode != v1alpha1.StageOfflineReplay {
					return exitWith(diff.ExitUsage,
						fmt.Errorf("stage %q uses mode %q, which is not implemented", stage.Name, stage.Mode))
				}
				scorer, err := scorerFor(stage.Scorer)
				if err != nil {
					return exitWith(diff.ExitUsage, err)
				}

				runner := &rollout.Runner{
					Corpus: recordings,
					Replayer: &replay.Driver{
						Corpus: recordings, Blobs: blobs, Outputs: outputs,
						Mode:          replay.ModeModelLiveToolsReplayed,
						ModelUpstream: modelUp,
						SkipDrift:     skipDrift,
						Agent:         agentCmd,
					},
					Scorer:       scorer,
					Analyzer:     analyzerFor(spec.Analyzer),
					OutputCorpus: outputDir,
					BlobDir:      blobDir,
				}

				result, err := runner.RunOfflineReplay(cmd.Context(), stage, incumbent, candidate)
				if err != nil {
					// A stage that could not run is not a stage that failed,
					// and the difference decides whether a human is needed.
					fmt.Fprintf(os.Stderr, "\n  stage %q could not run: %v\n", stage.Name, err)
					return VerdictExit{Code: 3}
				}
				results = append(results, result)
				if result.Verdict.Outcome != analysis.OutcomePromote {
					break
				}
			}

			switch output {
			case "json":
				if err := json.NewEncoder(cmd.OutOrStdout()).Encode(results); err != nil {
					return exitWith(diff.ExitInternal, err)
				}
			case "text":
				renderRollout(cmd.OutOrStdout(), spec, results, palette(noColor))
			default:
				return exitWith(diff.ExitUsage, fmt.Errorf("unknown output format %q", output))
			}
			return VerdictExit{Code: rolloutExitCode(results)}
		},
	}

	cmd.Flags().StringVar(&corpusDir, "corpus", "", "directory holding recorded cassettes (required)")
	cmd.Flags().StringVar(&blobDir, "blobs", "", "content-addressed blob store")
	cmd.Flags().StringVar(&outputDir, "output-corpus", "", "where replay outputs are written (default: a temporary directory)")
	cmd.Flags().StringVar(&modelUp, "model-upstream", "", "model provider the replays run against (required)")
	cmd.Flags().StringArrayVar(&agentCmd, "agent", nil, "command that drives the agent during replay")
	cmd.Flags().StringVarP(&output, "output", "o", "text", "output format: text or json")
	cmd.Flags().BoolVar(&noColor, "no-color", false, "disable colour (also honours NO_COLOR)")
	cmd.Flags().BoolVar(&skipDrift, "skip-drift-check", false,
		"do not check recorded tool contracts against their live servers")
	_ = cmd.MarkFlagRequired("corpus")
	_ = cmd.MarkFlagRequired("model-upstream")
	return cmd
}

func loadRollout(path string) (*v1alpha1.AgentRolloutSpec, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r v1alpha1.AgentRollout
	if err := yaml.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(r.Spec.Stages) == 0 {
		return nil, fmt.Errorf("%s: no stages; a rollout with nothing to run would report success "+
			"having measured nothing", path)
	}
	return &r.Spec, nil
}

// refIn keeps a manifest reference usable both as a local path and as a cluster
// object name, so one rollout file works in both places.
func refIn(ref, namespace string) string {
	_ = namespace
	return ref
}

func scorerFor(spec v1alpha1.ScorerSpec) (score.Scorer, error) {
	timeout := time.Duration(spec.TimeoutSeconds) * time.Second
	switch {
	case spec.Exec != nil:
		return &score.ExecScorer{Command: spec.Exec.Command, Args: spec.Exec.Args, Timeout: timeout}, nil
	case spec.HTTP != nil:
		return &score.HTTPScorer{Endpoint: spec.HTTP.Endpoint, Timeout: timeout}, nil
	}
	return nil, fmt.Errorf("no scorer configured: this gate has no source of measurements")
}

func analyzerFor(spec v1alpha1.AnalyzerSpec) analysis.Analyzer {
	if spec.Endpoint == "" {
		return &analysis.PairedBootstrap{}
	}
	return &analysis.Remote{Endpoint: spec.Endpoint, Timeout: time.Duration(spec.TimeoutSeconds) * time.Second}
}

func rolloutExitCode(results []*rollout.StageResult) int {
	if len(results) == 0 {
		return 3
	}
	for _, r := range results {
		switch r.Verdict.Outcome {
		case analysis.OutcomeWaveOff:
			return 2
		case analysis.OutcomeInconclusive:
			// Not a pass and not a failure: somebody has to look.
			return 3
		}
	}
	return 0
}

func renderRollout(w io.Writer, spec *v1alpha1.AgentRolloutSpec,
	results []*rollout.StageResult, p diff.Palette) {

	b := &strings.Builder{}
	fmt.Fprintf(b, "\n  %s%s → %s%s\n\n", p.Bold, spec.IncumbentRef, spec.CandidateRef, p.Reset)

	for _, r := range results {
		marker := "  "
		if r.Verdict.Outcome != analysis.OutcomePromote {
			marker = p.Red + "!" + p.Reset + " "
		}
		fmt.Fprintf(b, "%s%s%s%s   %s%d scored · %d excluded of %d%s\n",
			marker, p.Bold, r.Name, p.Reset, p.Dim, r.Scored, r.Excluded, r.Sessions, p.Reset)

		if r.JudgeCheck.Reason != "" {
			fmt.Fprintf(b, "      %s↳ %s%s\n", p.Dim, r.JudgeCheck.Reason, p.Reset)
		}
		fmt.Fprintf(b, "      %s%s%s  %s\n", p.Bold, r.Verdict.Outcome, p.Reset, r.Verdict.Reason)

		if pr := r.Verdict.Primary; pr.Name != "" {
			fmt.Fprintf(b, "      %s%s: %.4f → %.4f  (Δ %+.4f, %.0f%% CI [%+.4f, %+.4f], n=%d)%s\n",
				p.Dim, pr.Name, pr.IncumbentMean, pr.CandidateMean, pr.Delta,
				(1-pr.Alpha)*100, pr.CILower, pr.CIUpper, pr.N, p.Reset)
		}
		for _, g := range r.Verdict.Guardrails {
			state := "ok"
			if !g.Pass {
				state = "FAILED"
			}
			fmt.Fprintf(b, "      %sguardrail %s: %s — %s%s\n", p.Dim, g.Name, state, g.Detail, p.Reset)
		}
		// How much of the corpus was actually measured, because a gate that
		// scored a tenth of it looks identical to one that scored all of it.
		if m := r.Verdict.Missing; m.Attempted > 0 {
			fmt.Fprintf(b, "      %sscoring failed on %d item(s): %d candidate-only, %d incumbent-only%s\n",
				p.Dim, m.Dropped(), m.CandidateOnlyFailed, m.IncumbentOnlyFailed, p.Reset)
		}
		b.WriteString("\n")
	}

	switch rolloutExitCode(results) {
	case 0:
		fmt.Fprintf(b, "  %spromote%s — every stage passed\n\n", p.Bold, p.Reset)
	case 2:
		fmt.Fprintf(b, "  %swaved off%s — the candidate goes around again\n\n", p.Bold, p.Reset)
	default:
		fmt.Fprintf(b, "  %sheld%s — the gate could not decide; a human has to look\n\n", p.Bold, p.Reset)
	}
	_, _ = io.WriteString(w, b.String())
}
