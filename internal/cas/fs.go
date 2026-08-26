// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package cas

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// FS stores blobs on a filesystem.
//
// This is the development and single-node implementation, and it is a real one:
// §13.4 forbids metering or gating corpus size, so the only limit here is the
// disk the operator gave it.
type FS struct {
	root string
}

var _ Store = (*FS)(nil)

// NewFS opens (and creates) a store rooted at dir.
func NewFS(dir string) (*FS, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create blob store at %s: %w", dir, err)
	}
	return &FS{root: dir}, nil
}

// path shards by the first two bytes of the hash. A single flat directory with
// a million entries is slow to list and unpleasant on several filesystems;
// two levels of 256 keeps directories small without deep nesting.
func (f *FS) path(d Digest) (string, error) {
	if err := d.Validate(); err != nil {
		return "", err
	}
	h := d.Hex()
	return filepath.Join(f.root, h[0:2], h[2:4], h), nil
}

func (f *FS) Put(ctx context.Context, r io.Reader) (Digest, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	// Stream to a temporary file while hashing, so a blob larger than memory
	// still works and a crash mid-write cannot leave a truncated blob under a
	// digest that claims to describe complete content.
	tmp, err := os.CreateTemp(f.root, ".incoming-*")
	if err != nil {
		return "", fmt.Errorf("create temp blob: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()

	hasher := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, hasher), r); err != nil {
		return "", fmt.Errorf("write blob: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return "", fmt.Errorf("sync blob: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close blob: %w", err)
	}

	digest := Digest("sha256:" + hex.EncodeToString(hasher.Sum(nil)))
	final, err := f.path(digest)
	if err != nil {
		return "", err
	}

	// Already present: content addressing means the existing blob is byte
	// identical, so there is nothing to do and nothing to race over.
	if _, err := os.Stat(final); err == nil {
		return digest, nil
	}

	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return "", fmt.Errorf("create blob directory: %w", err)
	}
	// Rename is atomic within a filesystem, so a reader never observes a
	// partially written blob. Two writers racing on the same content both
	// succeed and the result is identical either way.
	if err := os.Rename(tmpName, final); err != nil {
		if _, statErr := os.Stat(final); statErr == nil {
			return digest, nil
		}
		return "", fmt.Errorf("commit blob: %w", err)
	}
	return digest, nil
}

func (f *FS) Get(ctx context.Context, d Digest) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p, err := f.path(d)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, d)
	}
	if err != nil {
		return nil, err
	}
	return file, nil
}

func (f *FS) Has(ctx context.Context, d Digest) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	p, err := f.path(d)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(p)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func (f *FS) Stat(ctx context.Context, d Digest) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	p, err := f.path(d)
	if err != nil {
		return 0, err
	}
	info, err := os.Stat(p)
	if errors.Is(err, os.ErrNotExist) {
		return 0, fmt.Errorf("%w: %s", ErrNotFound, d)
	}
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}
