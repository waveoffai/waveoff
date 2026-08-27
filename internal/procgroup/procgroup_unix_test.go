// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package procgroup_test

import (
	"bytes"
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/waveoffai/waveoff/internal/procgroup"
)

// TestCancellingKillsTheGrandchildToo is the bug this package was written for.
//
// A scorer is usually a shell wrapper around something slower. Killing only the
// direct child leaves the grandchild alive holding the output pipes, so Run
// blocks until it finishes anyway — the timeout is decorative and a hanging
// judge still hangs the rollout it was supposed to protect.
//
// The threshold below is deliberate. Isolate also sets a WaitDelay, so even the
// broken behaviour eventually returns once the pipes are abandoned; a test that
// only asserted "returns eventually" would pass on both. Returning promptly is
// what distinguishes killing the group from killing the parent and waiting.
func TestCancellingKillsTheGrandchildToo(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// `sleep` is the grandchild; it inherits stdout and outlives the shell.
	cmd := exec.CommandContext(ctx, "sh", "-c", "sleep 60 & wait")
	var out bytes.Buffer
	cmd.Stdout = &out
	procgroup.Isolate(cmd)

	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	cancel()
	_ = cmd.Wait()

	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Wait took %v after cancellation; the grandchild survived and kept the pipe "+
			"open, so the timeout only worked because WaitDelay eventually gave up", elapsed)
	}
}

// TestIsolateAlwaysSetsAWaitDelay: the backstop for anything that survives the
// signal and still holds a pipe. Without it a stuck grandchild blocks forever
// on every platform, which is the failure this package exists to rule out.
func TestIsolateAlwaysSetsAWaitDelay(t *testing.T) {
	cmd := exec.Command("true")
	procgroup.Isolate(cmd)
	if cmd.WaitDelay == 0 {
		t.Error("no WaitDelay was set")
	}
	if cmd.Cancel == nil {
		t.Error("no Cancel was set, so a context deadline would not kill anything")
	}
}
