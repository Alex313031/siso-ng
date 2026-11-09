// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package hashfs

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"sync"
	"testing"

	"go.chromium.org/build/hashigo/digest"

	"go.chromium.org/build/siso/blob"
	"go.chromium.org/build/siso/path"
)

// TestEntries_CanceledDigestDoesNotPoisonEntry verifies that canceling a
// caller's context while digest computation is in flight (such as when a
// racing step finishes first and cancels its per-step context) does not
// poison the shared entry for subsequent Entries, Stat, or ReadDir calls.
func TestEntries_CanceledDigestDoesNotPoisonEntry(t *testing.T) {
	ctx := t.Context()
	fix := setupBlockingInput(ctx, t)

	raceCtx, raceCancel := context.WithCancel(ctx)
	defer raceCancel()

	raceDone := make(chan error, 1)
	go func() {
		_, err := fix.hfs.Entries(raceCtx, fix.dir, []path.Path{"in.txt"})
		raceDone <- err
	}()

	<-fix.started
	raceCancel()
	if err := <-raceDone; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Entries(raceCtx, [in.txt]) error = %v, want nil or %v", err, context.Canceled)
	}

	fix.release()

	ents, err := fix.hfs.Entries(ctx, fix.dir, []path.Path{"in.txt"})
	if err != nil {
		t.Fatalf("Entries(ctx, [in.txt]) unexpected error: %v", err)
	}
	if len(ents) != 1 {
		t.Errorf("len(Entries(ctx, [in.txt])) = %d, want 1", len(ents))
	} else if got := ents[0].Data.Digest(); got != fix.want {
		t.Errorf("Entries(ctx, [in.txt])[0].Data.Digest() = %v, want %v", got, fix.want)
	}

	if _, err := fix.hfs.Stat(ctx, fix.dir, "in.txt"); err != nil {
		t.Errorf("Stat(ctx, %q) error = %v, want nil", "in.txt", err)
	}

	dents, err := fix.hfs.ReadDir(ctx, fix.dir, ".")
	if err != nil {
		t.Fatalf("ReadDir(ctx, %q) unexpected error: %v", ".", err)
	}
	if len(dents) != 1 {
		t.Errorf("len(ReadDir(ctx, %q)) = %d, want 1", ".", len(dents))
	}
}

// TestEntryCompute_CanceledCallerDoesNotFailWaiter verifies that when the
// first caller of entry.compute cancels its context while localDigest is
// blocked on disk I/O, the canceled caller returns context.Canceled promptly
// while concurrent and subsequent callers with active contexts still succeed
// once the read completes.
func TestEntryCompute_CanceledCallerDoesNotFailWaiter(t *testing.T) {
	ctx := t.Context()
	fix := setupBlockingInput(ctx, t)

	raceCtx, raceCancel := context.WithCancel(ctx)
	defer raceCancel()

	err1Ch := make(chan error, 1)
	go func() {
		err1Ch <- fix.entry.compute(raceCtx, digest.SHA256, fix.fname)
	}()

	<-fix.started

	err2Ch := make(chan error, 1)
	go func() {
		err2Ch <- fix.entry.compute(ctx, digest.SHA256, fix.fname)
	}()

	raceCancel()
	if err := <-err1Ch; !errors.Is(err, context.Canceled) {
		t.Fatalf("entry.compute(raceCtx, %q) error = %v, want %v", fix.fname, err, context.Canceled)
	}

	err3Ch := make(chan error, 1)
	go func() {
		err3Ch <- fix.entry.compute(ctx, digest.SHA256, fix.fname)
	}()

	fix.release()

	if err := <-err2Ch; err != nil {
		t.Errorf("concurrent entry.compute(ctx, %q) error = %v, want nil", fix.fname, err)
	}
	if err := <-err3Ch; err != nil {
		t.Errorf("subsequent entry.compute(ctx, %q) error = %v, want nil", fix.fname, err)
	}
	if got := fix.entry.digest(); got != fix.want {
		t.Errorf("entry.digest() = %v, want %v", got, fix.want)
	}
}

type blockingInputFixture struct {
	hfs     *HashFS
	dir     string
	fname   string
	entry   *entry
	want    digest.Digest
	started <-chan struct{}
	release func()
}

func setupBlockingInput(ctx context.Context, t *testing.T) *blockingInputFixture {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("filepath.EvalSymlinks(t.TempDir()) failed: %v", err)
	}
	fname := filepath.Join(dir, "in.txt")
	want := writePastFile(t, fname, "shared input content")

	hfs, err := New(ctx, Option{})
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	started := make(chan struct{})
	releaseCh := make(chan struct{})
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() { close(releaseCh) })
	}

	cleanupCtx := context.WithoutCancel(ctx)
	t.Cleanup(func() {
		release()
		if err := hfs.Close(cleanupCtx); err != nil {
			t.Errorf("hfs.Close() error = %v, want nil", err)
		}
	})

	// StatMtime with DeferDigest=false populates the directory entry without
	// queueing background lazyCompute work, keeping digest timing deterministic.
	if _, err := hfs.StatMtime(ctx, dir, "in.txt"); err != nil {
		t.Fatalf("StatMtime(%q, %q) failed: %v", dir, "in.txt", err)
	}
	e, _, _, ok := hfs.directory.lookup(ctx, makeFullpath(dir, "in.txt"))
	if !ok {
		t.Fatalf("directory.lookup(%q) = false, want true", fname)
	}
	lsrc, ok := e.src.(blob.LocalFileSource)
	if !ok {
		t.Fatalf("e.src = %T, want blob.LocalFileSource", e.src)
	}
	e.mu.Lock()
	e.src = &blockingLocalFileSource{
		LocalFileSource: lsrc,
		started:         started,
		release:         releaseCh,
	}
	e.mu.Unlock()

	return &blockingInputFixture{
		hfs:     hfs,
		dir:     dir,
		fname:   fname,
		entry:   e,
		want:    want,
		started: started,
		release: release,
	}
}

type blockingLocalFileSource struct {
	blob.LocalFileSource
	startedOnce sync.Once
	started     chan<- struct{}
	release     <-chan struct{}
}

func (s *blockingLocalFileSource) Open(ctx context.Context) (io.ReadCloser, error) {
	s.startedOnce.Do(func() { close(s.started) })
	<-s.release
	return s.LocalFileSource.Open(ctx)
}
