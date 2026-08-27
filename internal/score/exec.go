// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package score

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/waveoffai/waveoff/internal/procgroup"
)

// ExecScorer runs a scorer as a subprocess.
//
// This is the universal adapter. Refs arrive on stdin as one JSON document,
// Results leave on stdout the same way, and anything that can read and write
// JSON is a scorer — a Python script driving a vendor SDK, a shell pipeline, a
// binary. No vendor appears in this repository's dependency graph, and nobody
// has to wait for us to write an adapter before they can gate on their own
// evals.
type ExecScorer struct {
	// Command and Args are the scorer to run.
	Command string
	Args    []string
	// Dir is the working directory. Empty means the caller's.
	Dir string
	// Env is appended to the caller's environment.
	Env []string
	// Timeout bounds the run. A scorer that hangs must not hang a rollout.
	Timeout time.Duration
}

var _ Scorer = (*ExecScorer)(nil)

// request is the document a scorer reads on stdin.
type request struct {
	SchemaVersion string `json:"schemaVersion"`
	Refs          []Ref  `json:"refs"`
}

// response is the document a scorer writes on stdout.
type response struct {
	Results []Result `json:"results"`
}

// Score runs the subprocess.
func (e *ExecScorer) Score(ctx context.Context, refs []Ref) ([]Result, error) {
	if e.Command == "" {
		return nil, fmt.Errorf("scorer: no command configured")
	}
	timeout := e.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	payload, err := json.Marshal(request{SchemaVersion: SchemaVersion, Refs: refs})
	if err != nil {
		return nil, err
	}

	cmd := exec.CommandContext(ctx, e.Command, e.Args...)
	cmd.Dir = e.Dir
	cmd.Env = append(os.Environ(), e.Env...)
	cmd.Stdin = bytes.NewReader(payload)

	// Killable as a whole, not just at the top.
	//
	// A scorer is usually a shell wrapper around something slower. Killing only
	// the direct child leaves the grandchild alive holding the output pipes, so
	// Run blocks until it finishes anyway and the timeout is decorative — a
	// hanging judge would still hang the rollout it was supposed to protect.
	procgroup.Isolate(cmd)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("scorer %s timed out after %s", e.Command, timeout)
		}
		// The scorer's own diagnostics are the useful part of this failure, so
		// they are carried out rather than swallowed.
		return nil, fmt.Errorf("scorer %s failed: %w\n%s", e.Command, err, trim(stderr.String()))
	}

	var resp response
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		return nil, fmt.Errorf("scorer %s did not return the expected JSON: %w\n%s",
			e.Command, err, trim(stdout.String()))
	}
	if err := Validate(resp.Results); err != nil {
		return nil, fmt.Errorf("scorer %s: %w", e.Command, err)
	}
	return resp.Results, nil
}

func trim(s string) string {
	const max = 2000
	if len(s) > max {
		return s[:max] + "\n… (truncated)"
	}
	return s
}
