// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package corpus_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/waveoffai/waveoff/internal/cassette"
	"github.com/waveoffai/waveoff/internal/corpus"
)

func newStore(t *testing.T) *corpus.FS {
	t.Helper()
	s, err := corpus.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func header(session, agent, behavior string, at time.Time) cassette.Header {
	return cassette.Header{
		SessionID: session, Agent: agent,
		BehaviorDigest: behavior, RecordedAt: at,
	}
}

func TestCreateAndOpen(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	w, err := s.Create(ctx, header("sess-1", "support-agent", "sha256:"+strings.Repeat("a", 64), time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	w.Close()

	f, err := s.Open(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	r, err := cassette.NewReader(f, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Header().Agent != "support-agent" {
		t.Errorf("header = %+v", r.Header())
	}
}

// TestRecordingTwiceIsRefused: a recorded session is evidence, and silently
// replacing one loses it.
func TestRecordingTwiceIsRefused(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	h := header("dup", "a", "", time.Now())

	w, err := s.Create(ctx, h)
	if err != nil {
		t.Fatal(err)
	}
	w.Close()

	if _, err := s.Create(ctx, h); err == nil {
		t.Fatal("a session was recorded over an existing one")
	}
}

func TestOpenMissingSession(t *testing.T) {
	if _, err := newStore(t).Open(context.Background(), "nope"); !errors.Is(err, corpus.ErrNotFound) {
		t.Errorf("err = %v, want corpus.ErrNotFound", err)
	}
}

// TestSessionIDsCannotEscape: session IDs come from a request header, so they
// are attacker-controlled in the same sense any request header is.
func TestSessionIDsCannotEscape(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	for _, id := range []string{
		"../escape", "/etc/passwd", "a/b", "..", ".hidden", "",
		strings.Repeat("x", 200), "with space", "semi;colon",
	} {
		if _, err := s.Create(ctx, header(id, "a", "", time.Now())); err == nil {
			t.Errorf("Create accepted the session id %q", id)
		}
		if _, err := s.Open(ctx, id); err == nil {
			t.Errorf("Open accepted the session id %q", id)
		}
	}
}

func TestListFiltersAndOrders(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	base := time.Date(2026, 8, 26, 9, 0, 0, 0, time.UTC)
	digestA := "sha256:" + strings.Repeat("a", 64)
	digestB := "sha256:" + strings.Repeat("b", 64)

	for _, spec := range []struct {
		id, agent, behavior string
		at                  time.Time
	}{
		{"s1", "support-agent", digestA, base},
		{"s2", "support-agent", digestA, base.Add(time.Hour)},
		{"s3", "support-agent", digestB, base.Add(2 * time.Hour)},
		{"s4", "billing-agent", digestA, base.Add(3 * time.Hour)},
	} {
		w, err := s.Create(ctx, header(spec.id, spec.agent, spec.behavior, spec.at))
		if err != nil {
			t.Fatal(err)
		}
		w.Close()
	}

	all, err := s.List(ctx, corpus.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Fatalf("got %d, want 4", len(all))
	}
	// Newest first.
	if all[0].SessionID != "s4" {
		t.Errorf("first = %q, want the newest", all[0].SessionID)
	}

	byAgent, _ := s.List(ctx, corpus.Filter{Agent: "support-agent"})
	if len(byAgent) != 3 {
		t.Errorf("agent filter returned %d, want 3", len(byAgent))
	}

	// Selecting by manifest identity is what makes a corpus a regression suite
	// rather than a pile of traffic.
	byDigest, _ := s.List(ctx, corpus.Filter{BehaviorDigest: digestA})
	if len(byDigest) != 3 {
		t.Errorf("behaviour digest filter returned %d, want 3", len(byDigest))
	}

	since, _ := s.List(ctx, corpus.Filter{Since: base.Add(90 * time.Minute)})
	if len(since) != 2 {
		t.Errorf("since filter returned %d, want 2", len(since))
	}

	limited, _ := s.List(ctx, corpus.Filter{Limit: 2})
	if len(limited) != 2 {
		t.Errorf("limit returned %d", len(limited))
	}
}

// TestUnreadableCassetteDoesNotHideTheCorpus: a recorder killed mid-session
// leaves a partial file, and that must not make the rest unlistable.
func TestUnreadableCassetteDoesNotHideTheCorpus(t *testing.T) {
	dir := t.TempDir()
	s, err := corpus.NewFS(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	w, _ := s.Create(ctx, header("good", "a", "", time.Now()))
	w.Close()

	// A half-written cassette.
	if err := writeFile(dir+"/partial"+corpus.Extension, `{"schemaVer`); err != nil {
		t.Fatal(err)
	}

	got, err := s.List(ctx, corpus.Filter{})
	if err != nil {
		t.Fatalf("listing failed because of one bad file: %v", err)
	}
	if len(got) != 1 || got[0].SessionID != "good" {
		t.Errorf("got %+v", got)
	}
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}
