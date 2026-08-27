// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package procgroup

import (
	"os/exec"
	"syscall"
)

func isolate(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Negative pid signals the whole group, which is the point: the
		// grandchild holding the pipes dies with its parent.
		if pgid, err := syscall.Getpgid(cmd.Process.Pid); err == nil {
			return syscall.Kill(-pgid, syscall.SIGKILL)
		}
		return cmd.Process.Kill()
	}
}
