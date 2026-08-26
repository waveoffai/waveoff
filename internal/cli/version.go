// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"runtime"
	"runtime/debug"

	"github.com/spf13/cobra"
)

// Version is stamped at build time via -ldflags. It falls back to the module
// version Go records for `go install`ed binaries, and to "dev" for a plain
// `go build`.
var Version = ""

// Commit is stamped at build time. Otherwise it comes from the VCS stamp Go
// embeds automatically.
var Commit = ""

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Long: `Print version, commit and build details.

This command performs no network access. Nothing in waveoff phones home, checks
for updates, or reports usage — see the licence section of the README.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			v, c := buildInfo()
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "waveoff %s\n  commit  %s\n  go      %s\n  platform %s/%s\n",
				v, c, runtime.Version(), runtime.GOOS, runtime.GOARCH)
			return err
		},
	}
}

func buildInfo() (version, commit string) {
	version, commit = Version, Commit
	info, ok := debug.ReadBuildInfo()
	if ok {
		if version == "" && info.Main.Version != "" && info.Main.Version != "(devel)" {
			version = info.Main.Version
		}
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" && commit == "" {
				commit = s.Value
			}
			if s.Key == "vcs.modified" && s.Value == "true" && commit != "" {
				commit += "-dirty"
			}
		}
	}
	if version == "" {
		version = "dev"
	}
	if commit == "" {
		commit = "unknown"
	}
	return version, commit
}
