// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

// Package cli implements the waveoff command line.
package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/waveoffai/waveoff/internal/diff"
)

// VerdictExit carries a diff verdict out through cobra's error return so that
// main can exit with it. It is not a failure.
type VerdictExit struct{ Code int }

func (v VerdictExit) Error() string { return fmt.Sprintf("exit status %d", v.Code) }

// Root builds the command tree.
func Root() *cobra.Command {
	root := &cobra.Command{
		Use:   "waveoff",
		Short: "Pin, compare and verify agent releases",
		Long: `Waveoff pins everything that determines an agent's behaviour — code, prompts,
model, tool contracts, retrieval, policy and judges — into one immutable,
content-addressed manifest, so that rolling an agent back is unambiguous.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringP("namespace", "n", "", "Kubernetes namespace for cluster lookups")
	root.AddCommand(newDiffCmd(), newVerifyCmd(), newPinCmd(), newReplayCmd(), newRolloutCmd(), newVersionCmd())
	return root
}

// palette decides whether to colour the output. Colour is only ever an accent:
// every severity marker is also a literal "!", so piping into a ticket loses
// nothing.
func palette(noColor bool) diff.Palette {
	if noColor || os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" || !isTerminal(os.Stdout) {
		return diff.NoColor
	}
	return diff.Color
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
