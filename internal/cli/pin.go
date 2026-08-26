// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/waveoffai/waveoff/internal/diff"
	"github.com/waveoffai/waveoff/internal/manifest"
	"github.com/waveoffai/waveoff/internal/pin"
)

func newPinCmd() *cobra.Command {
	var (
		out          string
		container    string
		agent        string
		mcpServers   []string
		setValues    []string
		allowSecrets bool
	)

	cmd := &cobra.Command{
		Use:   "pin deployment/<name>",
		Short: "Build a manifest from a running Deployment",
		Long: `Introspect a running Deployment and emit a best-effort AgentManifest.

pin resolves the image digest from what is actually running rather than from the
pod template, reads model configuration from the container environment, hashes
prompt bodies out of mounted ConfigMaps, and — given --mcp-server — connects to
each MCP server to pin the tool contracts it advertises.

It does not guess a tool's effect. The server's readOnlyHint and
destructiveHint are its own claims about itself, and the server is the untrusted
party in the tool-poisoning threat model this manifest exists to detect, so
those hints are rendered as a comment for you to accept or reject.

Anything pin cannot determine is marked TODO. Resolve the TODOs, then run
waveoff verify --write to compute the digests and the object name.`,
		Example: `  waveoff pin deployment/support-agent -n prod \
    --mcp-server jira=https://jira-gw.internal/mcp \
    -o manifest.yaml`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ns, _ := cmd.Flags().GetString("namespace")
			if ns == "" {
				ns = "default"
			}
			name := strings.TrimPrefix(args[0], "deployment/")
			name = strings.TrimPrefix(name, "deploy/")

			servers, err := parseKV(mcpServers, "--mcp-server", "<label>=<url>")
			if err != nil {
				return exitWith(diff.ExitUsage, err)
			}
			sets, err := parseKV(setValues, "--set", "<field>=<value>")
			if err != nil {
				return exitWith(diff.ExitUsage, err)
			}

			cfg, err := config.GetConfig()
			if err != nil {
				return exitWith(diff.ExitUnavailable, fmt.Errorf("%w: %v", manifest.ErrNoCluster, err))
			}
			c, err := client.New(cfg, client.Options{Scheme: manifest.Scheme})
			if err != nil {
				return exitWith(diff.ExitUnavailable, err)
			}

			res, err := pin.Pin(cmd.Context(), c, pin.LiveTools, pin.Options{
				Namespace:    ns,
				Deployment:   name,
				Container:    container,
				Agent:        agent,
				MCPServers:   servers,
				Set:          sets,
				AllowSecrets: allowSecrets,
			})
			if err != nil {
				return exitWith(diff.ExitUsage, err)
			}

			w := cmd.OutOrStdout()
			if out != "" && out != "-" {
				f, err := os.Create(out)
				if err != nil {
					return exitWith(diff.ExitUsage, err)
				}
				defer f.Close()
				w = f
			}
			if err := res.Emit(w); err != nil {
				return exitWith(diff.ExitInternal, err)
			}

			// Notes go to stderr so that `waveoff pin ... > manifest.yaml`
			// produces a clean file while the operator still sees what was
			// left undecided.
			reportNotes(res, out)
			return nil
		},
	}

	cmd.Flags().StringVarP(&out, "output", "o", "-", "write the manifest here ('-' for stdout)")
	cmd.Flags().StringVar(&container, "container", "", "container running the agent (required if the pod has several)")
	cmd.Flags().StringVar(&agent, "agent", "", "logical agent name (defaults to the Deployment name)")
	cmd.Flags().StringArrayVar(&mcpServers, "mcp-server", nil, "MCP server to introspect, as <label>=<url> (repeatable)")
	cmd.Flags().StringArrayVar(&setValues, "set", nil, "override an inferred field, as <field>=<value> (repeatable)")
	cmd.Flags().BoolVar(&allowSecrets, "allow-secrets", false, "read prompt bodies out of mounted Secrets")
	return cmd
}

func reportNotes(res *pin.Result, out string) {
	if len(res.Notes) > 0 {
		fmt.Fprintln(os.Stderr)
		for _, n := range res.Notes {
			fmt.Fprintf(os.Stderr, "  note: %s\n", n)
		}
	}
	fmt.Fprintln(os.Stderr)
	if res.TODOs == 0 {
		fmt.Fprintf(os.Stderr, "  manifest is complete: %s\n", res.Manifest.Name)
		return
	}
	target := out
	if target == "" || target == "-" {
		target = "<file>"
	}
	fmt.Fprintf(os.Stderr,
		"  %d field(s) marked TODO. Resolve them, then run:\n\n      waveoff verify --write %s\n\n"+
			"  which computes both digests and the object name.\n", res.TODOs, target)
}

func parseKV(in []string, flag, shape string) (map[string]string, error) {
	out := map[string]string{}
	for _, s := range in {
		k, v, ok := strings.Cut(s, "=")
		if !ok || k == "" || v == "" {
			return nil, fmt.Errorf("%s %q is not %s", flag, s, shape)
		}
		out[k] = v
	}
	return out, nil
}
