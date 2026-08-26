// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package cas_test

import (
	"os"
	"os/exec"
	"sync"
	"testing"
)

// One MinIO container is shared by the whole test binary and removed at the
// end. Starting one per subtest would dominate the runtime of a suite that is
// otherwise milliseconds.
var (
	minioOnce     sync.Once
	minioName     string
	minioEndpoint string
	minioErr      error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if minioName != "" {
		_ = exec.Command("docker", "rm", "-f", minioName).Run()
	}
	os.Exit(code)
}
