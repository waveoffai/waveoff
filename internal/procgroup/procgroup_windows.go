// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package procgroup

import "os/exec"

// isolate kills the direct child only.
//
// Windows has no process group to signal. A wrapper that spawns its own child
// can leave that child running, which is exactly the case the Unix path exists
// to handle — so cancellation here is weaker, and the WaitDelay that Isolate
// sets is what keeps a hung grandchild from blocking forever.
//
// Killing a whole tree on Windows means a Job Object, which is worth adding if
// anyone runs a scorer on Windows in earnest. Until then this is stated rather
// than pretended.
func isolate(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return cmd.Process.Kill()
	}
}
