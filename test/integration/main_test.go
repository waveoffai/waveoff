// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package integration

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	code := m.Run()
	stopReferenceMCP()
	os.Exit(code)
}
