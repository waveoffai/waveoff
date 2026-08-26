// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package cas_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/waveoffai/waveoff/internal/cas"
	"github.com/waveoffai/waveoff/internal/cas/castest"
)

func TestFSConformance(t *testing.T) {
	castest.RunConformance(t, func(t *testing.T) cas.Store {
		s, err := cas.NewFS(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
}

// TestFSStoresContentOnce is the dedup property checked on disk rather than
// through the interface: the same bytes must occupy one file.
func TestFSStoresContentOnce(t *testing.T) {
	dir := t.TempDir()
	s, err := cas.NewFS(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	body := []byte("a document retrieved in every session")

	for i := 0; i < 25; i++ {
		if _, err := s.Put(ctx, bytes.NewReader(body)); err != nil {
			t.Fatal(err)
		}
	}

	var blobs int
	err = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			blobs++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if blobs != 1 {
		t.Errorf("25 identical writes produced %d files, want 1", blobs)
	}
}

// TestFSLeavesNoTempFiles: a store that accumulates .incoming-* files fills the
// disk the corpus needs.
func TestFSLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	s, _ := cas.NewFS(dir)
	ctx := context.Background()

	for i := 0; i < 10; i++ {
		if _, err := s.Put(ctx, strings.NewReader("payload")); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".incoming-") {
			t.Errorf("temporary file left behind: %s", e.Name())
		}
	}
}

// TestFSPathTraversalIsRefused: a digest arriving from a cassette is untrusted.
func TestFSPathTraversalIsRefused(t *testing.T) {
	dir := t.TempDir()
	s, _ := cas.NewFS(dir)
	secret := filepath.Join(dir, "..", "secret.txt")
	if err := os.WriteFile(secret, []byte("do not read me"), 0o600); err != nil {
		t.Skip("cannot stage the traversal target")
	}
	defer os.Remove(secret)

	if _, err := s.Get(context.Background(), cas.Digest("sha256:../secret.txt")); err == nil {
		t.Error("a traversal digest was accepted")
	}
}

func TestDigestHelpers(t *testing.T) {
	d := cas.Compute([]byte("hello"))
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(d.Hex()) != 64 {
		t.Errorf("Hex() = %q", d.Hex())
	}
	if got := d.Short(8); len(got) != 8 || !strings.HasPrefix(d.Hex(), got) {
		t.Errorf("Short(8) = %q", got)
	}
	if got := d.Short(200); got != d.Hex() {
		t.Error("Short must clamp rather than panic")
	}
}
