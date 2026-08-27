// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package replay

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"time"

	"github.com/waveoffai/waveoff/api/v1alpha1"
	"github.com/waveoffai/waveoff/internal/cas"
	"github.com/waveoffai/waveoff/internal/cassette"
	"github.com/waveoffai/waveoff/internal/corpus"
	"github.com/waveoffai/waveoff/internal/procgroup"
)

// Driver replays one recorded session against one manifest.
//
// It exists so the command line and the rollout controller drive a replay the
// same way. Two implementations of "run a replay" would diverge, and the one
// that diverged would be the one making promotion decisions.
type Driver struct {
	// Corpus holds the recordings, Blobs their offloaded payloads.
	Corpus corpus.Store
	Blobs  cas.Store

	// Outputs, when set, receives what the candidate produced — the artifact a
	// scorer reads.
	Outputs corpus.Store

	Mode          Mode
	ModelUpstream string
	LiveTools     []string

	// Listen is the address the replayer binds. Loopback only.
	Listen string

	// SkipDrift disables checking recorded tool contracts against their live
	// servers. Off by default: a session scored against contracts that have
	// moved is scored against a fiction.
	SkipDrift  bool
	ToolLister ToolLister

	// Agent is the command that drives the replay. Empty means the caller
	// drives it themselves against the returned address.
	Agent []string
	// AgentEnv is extra environment for the agent, for whatever configuration
	// it needs that is not a model base URL.
	AgentEnv []string
	// AgentTimeout bounds the agent run. A replayed request the agent never
	// correlates leaves it waiting forever, which without a deadline looks
	// like a slow gate rather than a broken one.
	AgentTimeout time.Duration
}

// Replay runs one session and returns the report.
func (d *Driver) Replay(ctx context.Context, session string,
	spec *v1alpha1.AgentManifestSpec, arm ArmLabel) (report *Report, err error) {

	idx, serving, header, err := d.load(ctx, session)
	if err != nil {
		return nil, err
	}
	defer func() { _ = serving.Close() }()

	tracker := NewTracker(idx, d.Mode, spec.BehaviorDigest)

	if !d.SkipDrift {
		lister := d.ToolLister
		if lister == nil {
			lister = LiveTools
		}
		drift, err := CheckDrift(ctx, idx, lister)
		if err != nil {
			return nil, fmt.Errorf("checking contract drift for %s: %w", session, err)
		}
		tracker.AttachDrift(drift)
	}

	var output *Output
	if d.Outputs != nil {
		output, err = NewOutput(ctx, d.Outputs, header, session, arm, d.Blobs)
		if err != nil {
			return nil, err
		}
		// Not a bare deferred close. This cassette is what the candidate
		// produced and what the scorer will read; a failed close can mean the
		// tail of it never reached disk, and a truncated output reads
		// downstream as a candidate that did less work rather than as a
		// recording that lost some. Better to fail the replay.
		defer func() {
			if cerr := output.Close(); cerr != nil && err == nil {
				err = fmt.Errorf("closing the replay output for %s: %w", session, cerr)
			}
		}()
	}

	srv, err := NewServer(ServerConfig{
		Index: idx, Reader: serving.Reader,
		Policy:        NewPolicy(d.Mode, spec, d.LiveTools),
		Tracker:       tracker,
		ModelUpstream: d.ModelUpstream,
		Output:        output,
	})
	if err != nil {
		return nil, err
	}

	addr, stop, err := d.serve(srv)
	if err != nil {
		return nil, err
	}
	defer stop()

	if len(d.Agent) > 0 {
		if err := d.runAgent(ctx, addr); err != nil {
			// An agent that exits badly is a finding about the candidate, not a
			// failure of the harness. The report records what it managed to do
			// before it stopped, and Usable decides whether that is enough.
			_ = err
		}
	}
	// Named returns, so the deferred close above can turn a lost tail into a
	// failed replay rather than a quiet truncation.
	report, err = srv.Finish(), nil
	return report, err
}

// Address starts a replay server and hands back its address without driving an
// agent, for a caller that runs one itself.
func (d *Driver) load(ctx context.Context, session string) (*Index, closableReader, cassette.Header, error) {
	f, err := d.Corpus.Open(ctx, session)
	if err != nil {
		return nil, closableReader{}, cassette.Header{}, err
	}
	defer f.Close()

	r, err := cassette.NewReader(f, d.Blobs)
	if err != nil {
		return nil, closableReader{}, cassette.Header{}, fmt.Errorf("session %s: %w", session, err)
	}
	spans, err := r.All()
	if err != nil {
		return nil, closableReader{}, cassette.Header{}, err
	}

	// A second handle for serving: reading the spans consumed the first.
	f2, err := d.Corpus.Open(ctx, session)
	if err != nil {
		return nil, closableReader{}, cassette.Header{}, err
	}
	serving, err := cassette.NewReader(f2, d.Blobs)
	if err != nil {
		_ = f2.Close()
		return nil, closableReader{}, cassette.Header{}, err
	}
	return NewIndex(r.Header(), spans), closableReader{Reader: serving, closer: f2}, r.Header(), nil
}

type closableReader struct {
	*cassette.Reader
	closer interface{ Close() error }
}

func (c closableReader) Close() error {
	if c.closer != nil {
		return c.closer.Close()
	}
	return nil
}

func (d *Driver) serve(srv *Server) (addr string, stop func(), err error) {
	listen := d.Listen
	if listen == "" {
		listen = "127.0.0.1:0"
	}
	ln, err := listenLoopback(listen)
	if err != nil {
		return "", nil, err
	}
	httpSrv := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 30 * time.Second}
	go func() { _ = httpSrv.Serve(ln) }()

	return ln.Addr().String(), func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}, nil
}

// runAgent drives the agent at the replayer, pointing its base URLs here
// exactly as the injection webhook would in a cluster.
func (d *Driver) runAgent(ctx context.Context, addr string) error {
	timeout := d.AgentTimeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, d.Agent[0], d.Agent[1:]...)
	// Only the model base URLs are ours to rewrite, plus the replayer's own
	// address for anything that wants it.
	//
	// Tool endpoints are deliberately not set here. Which MCP servers an agent
	// talks to, and under what variable names, is the agent's configuration —
	// in a cluster the injection webhook rewrites those per server. Pointing
	// an agent at a tool endpoint the corpus never recorded makes it fail on
	// the handshake, which reads as the candidate misbehaving.
	cmd.Env = append(os.Environ(),
		"ANTHROPIC_BASE_URL=http://"+addr,
		"OPENAI_BASE_URL=http://"+addr,
		"WAVEOFF_REPLAY_ADDR="+addr,
	)
	cmd.Env = append(cmd.Env, d.AgentEnv...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	// Killable as a whole. An agent is usually a wrapper around something
	// slower, and killing only the direct child leaves the grandchild holding
	// the pipes so the timeout does nothing.
	procgroup.Isolate(cmd)
	return cmd.Run()
}

// listenLoopback binds an address, refusing anything but loopback.
//
// A replayer serves recorded production traffic and, in some modes, proxies to
// a live model with real credentials. Neither belongs on a socket anything else
// can reach.
func listenLoopback(addr string) (net.Listener, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("listen address %q: %w", addr, err)
	}
	switch host {
	case "127.0.0.1", "localhost", "::1", "":
	default:
		return nil, fmt.Errorf("refusing to listen on %q: a replayer serves recorded production "+
			"traffic, so it must stay on loopback", addr)
	}
	return net.Listen("tcp", addr)
}
