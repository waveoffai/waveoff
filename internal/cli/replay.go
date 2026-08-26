// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/waveoffai/waveoff/api/v1alpha1"
	"github.com/waveoffai/waveoff/internal/cas"
	"github.com/waveoffai/waveoff/internal/corpus"
	"github.com/waveoffai/waveoff/internal/diff"
	"github.com/waveoffai/waveoff/internal/manifest"
	"github.com/waveoffai/waveoff/internal/replay"
)

func newReplayCmd() *cobra.Command {
	var (
		corpusDir   string
		blobDir     string
		manifestRef string
		modeName    string
		modelUp     string
		liveTools   []string
		sessions    []string
		limit       int
		listen      string
		output      string
		noColor     bool
		skipDrift   bool
		outputDir   string
		arm         string
	)

	cmd := &cobra.Command{
		Use:   "replay [-- command args...]",
		Short: "Replay recorded sessions against a candidate manifest",
		Long: `Serve recorded sessions back to an agent and report where it departed from
what was recorded.

The replayer presents exactly the surface the recorder does, so an agent pointed
at it cannot tell the difference — which is what makes this a test of the agent
rather than of the harness.

Modes:

  model-live-tools-replayed   the primary evaluation mode. The model runs live
                              against the candidate's prompts so it can diverge,
                              which is the point. Every tool read is served from
                              the recording and every write is refused, so this
                              is safe to run against a corpus recorded from
                              production.

  fail                        replay everything; the first request not in the
                              cassette is the finding. Use it when nothing
                              behavioural was supposed to change.

  hybrid                      fork named tools to their live servers with
                              --live-tool, replay the rest. Every forked tool is
                              a hole in the safety argument, so they are named
                              rather than pattern-matched.

Before scoring anything, replay checks each recorded tool contract against what
its server advertises now. A session whose contracts have moved is excluded
rather than scored, because the recorded responses answer questions the tools no
longer ask.

Exit codes:
   0  no divergence
   2  the candidate departed from the recording
   3  nothing usable was measured (every session stale or incomplete)
  64  usage error`,
		Example: `  # Replay one session and report divergence.
  waveoff replay --corpus ./corpus --manifest candidate.yaml --session 4bf92f35...

  # Run the agent against the replayer.
  waveoff replay --corpus ./corpus --manifest candidate.yaml \
    --model-upstream https://api.anthropic.com -- python agent.py

  # Produce scorable output for one side of a comparison.
  waveoff replay --corpus ./corpus --manifest candidate.yaml \
    --output-corpus ./replays --arm candidate -- python agent.py`,
		RunE: func(cmd *cobra.Command, args []string) error {
			mode, err := replay.ParseMode(modeName)
			if err != nil {
				return exitWith(diff.ExitUsage, err)
			}

			spec, err := loadCandidate(cmd.Context(), manifestRef)
			if err != nil {
				return exitWith(diff.ExitUsage, err)
			}

			store, err := corpus.NewFS(corpusDir)
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

			targets, err := selectSessions(cmd.Context(), store, spec, sessions, limit)
			if err != nil {
				return exitWith(diff.ExitUsage, err)
			}
			if len(targets) == 0 {
				return exitWith(diff.ExitUsage, fmt.Errorf(
					"no sessions in %s match; record some traffic first, or widen the selection", corpusDir))
			}

			var outputs corpus.Store
			if outputDir != "" {
				o, err := corpus.NewFS(outputDir)
				if err != nil {
					return exitWith(diff.ExitUsage, err)
				}
				outputs = o
			}
			armLabel := replay.ArmLabel(arm)
			if armLabel != replay.ArmIncumbent && armLabel != replay.ArmCandidate {
				return exitWith(diff.ExitUsage, fmt.Errorf(
					"--arm is %q: use incumbent or candidate, since a paired comparison has exactly two sides", arm))
			}

			var reports []*replay.Report
			for _, sessionID := range targets {
				report, err := replayOne(cmd, replayOneOpts{
					store: store, blobs: blobs, spec: spec, mode: mode,
					sessionID: sessionID, modelUpstream: modelUp,
					liveTools: liveTools, listen: listen,
					skipDrift: skipDrift, command: args,
					outputs: outputs, arm: armLabel,
				})
				if err != nil {
					return exitWith(diff.ExitUsage, err)
				}
				reports = append(reports, report)
			}

			switch output {
			case "json":
				if err := replay.RenderJSON(cmd.OutOrStdout(), reports); err != nil {
					return exitWith(diff.ExitInternal, err)
				}
			case "text":
				if err := replay.RenderText(cmd.OutOrStdout(), reports, palette(noColor)); err != nil {
					return exitWith(diff.ExitInternal, err)
				}
			default:
				return exitWith(diff.ExitUsage, fmt.Errorf("unknown output format %q: use text or json", output))
			}
			return VerdictExit{Code: replay.ExitCode(reports)}
		},
	}

	cmd.Flags().StringVar(&corpusDir, "corpus", "", "directory holding recorded cassettes (required)")
	cmd.Flags().StringVar(&blobDir, "blobs", "", "content-addressed blob store for offloaded payloads")
	cmd.Flags().StringVar(&manifestRef, "manifest", "", "candidate AgentManifest, as a file or a cluster object (required)")
	cmd.Flags().StringVar(&modeName, "mode", string(replay.ModeModelLiveToolsReplayed), "replay mode")
	cmd.Flags().StringVar(&modelUp, "model-upstream", "", "real model provider, for modes that run the model live")
	cmd.Flags().StringArrayVar(&liveTools, "live-tool", nil, "tool forked to its live server in hybrid mode (repeatable)")
	cmd.Flags().StringArrayVar(&sessions, "session", nil, "session to replay (repeatable; default is every matching session)")
	cmd.Flags().IntVar(&limit, "limit", 0, "replay at most this many sessions")
	cmd.Flags().StringVar(&listen, "listen", "127.0.0.1:0", "address the replayer binds (loopback only)")
	cmd.Flags().StringVarP(&output, "output", "o", "text", "output format: text or json")
	cmd.Flags().BoolVar(&noColor, "no-color", false, "disable colour (also honours NO_COLOR)")
	cmd.Flags().BoolVar(&skipDrift, "skip-drift-check", false,
		"do not check recorded tool contracts against their live servers")
	cmd.Flags().StringVar(&outputDir, "output-corpus", "",
		"write what the candidate produced here, as cassettes a scorer can read")
	cmd.Flags().StringVar(&arm, "arm", string(replay.ArmCandidate),
		"which side of a comparison this run is: incumbent or candidate")

	_ = cmd.MarkFlagRequired("corpus")
	_ = cmd.MarkFlagRequired("manifest")
	return cmd
}

// loadCandidate resolves the manifest the replay is testing. Its tool effects
// are what decide whether a call may run, so a replay without one cannot be
// safe and is refused rather than defaulted.
func loadCandidate(ctx context.Context, ref string) (*v1alpha1.AgentManifestSpec, error) {
	m, err := manifest.Load(ctx, manifest.ParseRef(ref, ""))
	if err != nil {
		return nil, fmt.Errorf("candidate manifest: %w", err)
	}
	return &m.Spec, nil
}

func selectSessions(ctx context.Context, store corpus.Store, spec *v1alpha1.AgentManifestSpec,
	explicit []string, limit int) ([]string, error) {

	if len(explicit) > 0 {
		return explicit, nil
	}
	headers, err := store.List(ctx, corpus.Filter{Agent: spec.Agent, Limit: limit})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(headers))
	for _, h := range headers {
		out = append(out, h.SessionID)
	}
	return out, nil
}

type replayOneOpts struct {
	store         corpus.Store
	blobs         cas.Store
	spec          *v1alpha1.AgentManifestSpec
	mode          replay.Mode
	sessionID     string
	modelUpstream string
	liveTools     []string
	listen        string
	skipDrift     bool
	command       []string
	outputs       corpus.Store
	arm           replay.ArmLabel
}

func replayOne(cmd *cobra.Command, o replayOneOpts) (*replay.Report, error) {
	// One implementation of "run a replay", shared with the rollout
	// controller. Two would diverge, and the one that diverged would be the
	// one making promotion decisions.
	driver := &replay.Driver{
		Corpus: o.store, Blobs: o.blobs, Outputs: o.outputs,
		Mode: o.mode, ModelUpstream: o.modelUpstream, LiveTools: o.liveTools,
		Listen: o.listen, SkipDrift: o.skipDrift,
		Agent: o.command,
	}
	return driver.Replay(cmd.Context(), o.sessionID, o.spec, o.arm)
}
