// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

// Package corpus is where cassettes live.
//
// A corpus is the asset this product is built on, so the store is deliberately
// dull: cassettes are files, named by session, listed by reading headers. There
// is no index to corrupt, no database to migrate, and no metering — §13.4
// forbids gating recording volume, so the only limit is the disk the operator
// provided.
package corpus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/waveoffai/waveoff/internal/cassette"
)

// ErrNotFound reports that no cassette exists for a session.
var ErrNotFound = errors.New("cassette not found")

// Extension is the suffix every cassette file carries.
const Extension = ".cassette.ndjson"

// Store holds recorded sessions.
type Store interface {
	// Create opens a cassette for writing and writes its header. Creating a
	// session that already exists is an error rather than an overwrite: a
	// recorded session is evidence, and silently replacing one loses it.
	Create(ctx context.Context, h cassette.Header) (io.WriteCloser, error)

	// Open reads a recorded session.
	Open(ctx context.Context, sessionID string) (io.ReadCloser, error)

	// List returns the headers of stored cassettes, newest first.
	List(ctx context.Context, filter Filter) ([]cassette.Header, error)
}

// Filter narrows a listing. Zero values mean "no constraint".
type Filter struct {
	Agent string
	// BehaviorDigest selects sessions recorded against one manifest identity,
	// which is what makes a corpus a regression suite rather than a pile of
	// traffic.
	BehaviorDigest string
	Since          time.Time
	Limit          int
}

func (f Filter) matches(h cassette.Header) bool {
	if f.Agent != "" && h.Agent != f.Agent {
		return false
	}
	if f.BehaviorDigest != "" && h.BehaviorDigest != f.BehaviorDigest {
		return false
	}
	if !f.Since.IsZero() && h.RecordedAt.Before(f.Since) {
		return false
	}
	return true
}

// FS stores cassettes in a directory tree.
type FS struct{ root string }

var _ Store = (*FS)(nil)

// NewFS opens a corpus directory, creating it if needed.
func NewFS(dir string) (*FS, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create corpus at %s: %w", dir, err)
	}
	return &FS{root: dir}, nil
}

// safeName keeps a session ID from escaping the corpus directory. Session IDs
// come from a request header, so they are attacker-controlled in the same sense
// any request header is.
func safeName(sessionID string) (string, error) {
	if sessionID == "" {
		return "", errors.New("corpus: empty session id")
	}
	if len(sessionID) > 128 {
		return "", fmt.Errorf("corpus: session id is %d characters, over the 128 limit", len(sessionID))
	}
	for _, r := range sessionID {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.'
		if !ok {
			return "", fmt.Errorf("corpus: session id %q contains %q; only letters, digits, dash, underscore and dot are allowed", sessionID, r)
		}
	}
	if strings.HasPrefix(sessionID, ".") {
		return "", fmt.Errorf("corpus: session id %q may not start with a dot", sessionID)
	}
	return sessionID + Extension, nil
}

func (f *FS) Create(ctx context.Context, h cassette.Header) (io.WriteCloser, error) {
	name, err := safeName(h.SessionID)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(f.root, name)

	// O_EXCL: a session recorded twice is a bug worth surfacing, not an
	// overwrite that quietly discards the first recording.
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("corpus: session %q is already recorded", h.SessionID)
	}
	if err != nil {
		return nil, err
	}

	w := cassette.NewWriter(file, nil)
	if err := w.WriteHeader(h); err != nil {
		_ = file.Close()
		os.Remove(path)
		return nil, err
	}
	return file, nil
}

func (f *FS) Open(ctx context.Context, sessionID string) (io.ReadCloser, error) {
	name, err := safeName(sessionID)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(filepath.Join(f.root, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, sessionID)
	}
	return file, err
}

func (f *FS) List(ctx context.Context, filter Filter) ([]cassette.Header, error) {
	entries, err := os.ReadDir(f.root)
	if err != nil {
		return nil, err
	}

	var out []cassette.Header
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if e.IsDir() || !strings.HasSuffix(e.Name(), Extension) {
			continue
		}
		h, err := readHeader(filepath.Join(f.root, e.Name()))
		if err != nil {
			// One unreadable cassette must not hide the rest of the corpus.
			// A partially written file is the normal result of a recorder
			// being killed mid-session.
			continue
		}
		if filter.matches(h) {
			out = append(out, h)
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].RecordedAt.After(out[j].RecordedAt) })
	if filter.Limit > 0 && len(out) > filter.Limit {
		out = out[:filter.Limit]
	}
	return out, nil
}

// readHeader reads just the first line, so listing a large corpus does not read
// every cassette in full.
func readHeader(path string) (cassette.Header, error) {
	file, err := os.Open(path)
	if err != nil {
		return cassette.Header{}, err
	}
	defer file.Close()

	dec := json.NewDecoder(io.LimitReader(file, 64<<10))
	var h cassette.Header
	if err := dec.Decode(&h); err != nil {
		return cassette.Header{}, err
	}
	if h.SchemaVersion == "" {
		return cassette.Header{}, errors.New("corpus: no schema version in header")
	}
	return h, nil
}
