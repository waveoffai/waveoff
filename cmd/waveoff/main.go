// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

// Command waveoff is the Waveoff command line.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/waveoffai/waveoff/internal/cli"
	"github.com/waveoffai/waveoff/internal/diff"
)

func main() {
	if err := cli.Root().Execute(); err != nil {
		// A verdict is not an error: `waveoff diff` exits 1 or 2 to report what
		// it found, and cobra must not print those as failures.
		var verdict cli.VerdictExit
		if errors.As(err, &verdict) {
			os.Exit(verdict.Code)
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(diff.ExitUsage)
	}
}
