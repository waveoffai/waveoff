// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package integration

import (
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// referenceMCPVersion pins the server the tool plane is tested against.
//
// Pinned rather than floating: an unpinned reference server turns an upstream
// release into a failing build here, and pins are the whole subject of this
// project.
const referenceMCPVersion = "2026.8.18"

// One reference server is shared by the whole test binary.
//
// It binds a fixed port, so a server per test would collide; and npx resolves
// the package on first run, so starting several would multiply a slow step by
// the number of tests for no gain.
var (
	mcpOnce     sync.Once
	mcpEndpoint string
	mcpSkip     string
	mcpCmd      *exec.Cmd
)

// startReferenceMCP returns the endpoint of the official MCP reference server,
// starting it on first use.
//
// This is what makes the tool plane real. A hand-written MCP stub would prove
// the recorder matches our reading of the protocol; the reference server proves
// it matches the protocol — including the parts we got wrong first time, like
// session headers, SSE-framed responses and path handling.
func startReferenceMCP(t *testing.T) string {
	t.Helper()
	mcpOnce.Do(func() {
		if _, err := exec.LookPath("npx"); err != nil {
			mcpSkip = "npx not available"
			return
		}

		mcpCmd = exec.Command("npx", "-y",
			"@modelcontextprotocol/server-everything@"+referenceMCPVersion, "streamableHttp")
		// Its own process group. npx execs a node child, and killing only npx
		// leaves that child alive holding the output pipe, so Wait never
		// returns and the test binary hangs after every test has passed.
		mcpCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

		var out strings.Builder
		mcpCmd.Stdout = &out
		mcpCmd.Stderr = &out
		if err := mcpCmd.Start(); err != nil {
			mcpSkip = "could not start the reference MCP server: " + err.Error()
			return
		}

		// The server binds 3001 regardless of PORT.
		endpoint := "http://127.0.0.1:3001/mcp"
		if !waitForMCP(endpoint, 180*time.Second) {
			mcpSkip = "the reference MCP server never became ready:\n" + out.String()
			return
		}
		mcpEndpoint = endpoint
	})

	if mcpSkip != "" {
		t.Skip(mcpSkip)
	}
	return mcpEndpoint
}

// stopReferenceMCP is called from TestMain. It signals the whole process
// group, so the node process npx spawned goes with it.
func stopReferenceMCP() {
	if mcpCmd == nil || mcpCmd.Process == nil {
		return
	}
	if pgid, err := syscall.Getpgid(mcpCmd.Process.Pid); err == nil {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	} else {
		_ = mcpCmd.Process.Kill()
	}
	// Do not Wait: the pipe may still be held briefly, and a teardown that can
	// block is a teardown that hides a passing run behind a timeout.
	go func() { _ = mcpCmd.Wait() }()
}

// waitForMCP polls until the server answers an initialize.
func waitForMCP(endpoint string, timeout time.Duration) bool {
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":` +
		`{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"probe","version":"1"}}}`

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(body))
		if err != nil {
			return false
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 400 {
				return true
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}
