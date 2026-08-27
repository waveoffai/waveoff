// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

// Package procgroup runs a child process in a way that it can actually be
// killed.
//
// Both places this matters — a scorer and a replayed agent — are usually a
// shell wrapper around something slower. Killing only the direct child leaves
// the grandchild alive holding the output pipes, so Wait blocks until it
// finishes anyway and the timeout is decorative. A hanging judge would still
// hang the rollout it was supposed to protect, which is the failure this
// package exists to prevent.
//
// The mechanism is platform-specific and the guarantee is not the same on every
// platform. See Isolate.
package procgroup

import (
	"os/exec"
	"time"
)

// KillDelay is how long a process gets after the kill before its pipes are
// abandoned. A backstop for anything that survives the signal and still holds
// one.
const KillDelay = 5 * time.Second

// Isolate arranges for cmd to be killable as a whole, and sets cmd.Cancel to
// do it.
//
// On Unix the child gets its own process group and the group is signalled, so
// grandchildren die with it — the guarantee this package is named for.
//
// On Windows there is no process group to signal. Only the direct child is
// killed, so a wrapper that spawns its own child can leave that child running
// and holding a pipe. Cancellation is therefore best-effort there, and the
// WaitDelay below is what stops a hung grandchild blocking forever: the pipes
// are abandoned rather than waited on. A full guarantee needs a Job Object,
// which is worth adding if anyone runs a scorer on Windows in earnest.
func Isolate(cmd *exec.Cmd) {
	isolate(cmd)
	cmd.WaitDelay = KillDelay
}
