// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package scandeps

import (
	"context"
	"errors"
	"hash/maphash"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/path"
)

// TestScanFileDoesNotCacheContextCancel: a scan canceled while waiting on
// CPPScanSema must not store the context error on the shared scanResult and
// mark it done, or other scans adopt it and miss the header's deps.
func TestScanFileDoesNotCacheContextCancel(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	hfs, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}
	fsys := &filesystem{hashfs: hfs, seed: maphash.MakeSeed()}
	if err := os.WriteFile(filepath.Join(dir, "h.h"), []byte("#include <stdio.h>\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := hfs.Stat(ctx, dir, "h.h"); err != nil {
		t.Fatal(err)
	}
	fv := &fsview{
		visited: map[path.Path]bool{},
		dirs:    map[path.Path]bool{},
		files:   map[path.Path]*scanResult{},
		topEnts: map[path.Path]*sync.Map{},
	}
	fv.reset(fsys, dir, nil, nil)

	// Acquire CPPScanSema's full capacity so scanFile blocks in WaitAcquire;
	// canceling then makes CPPScanSema.Do return Canceled.
	var releases []func(error)
	for range CPPScanSema.Capacity() {
		_, rel, err := CPPScanSema.WaitAcquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, rel)
	}

	cctx, cancel := context.WithCancel(ctx)
	type result struct {
		sr  *scanResult
		err error
	}
	ch := make(chan result, 1)
	go func() {
		sr, err := fv.scanFile(cctx, "h.h")
		ch <- result{sr, err}
	}()
	// Wait until scanFile parks in WaitAcquire (one waiter) instead of sleeping.
	deadline := time.Now().Add(5 * time.Second)
	for CPPScanSema.NumWaits() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("scanFile never reached CPPScanSema")
		}
		runtime.Gosched()
	}
	cancel()
	got := <-ch
	for _, rel := range releases {
		rel(nil)
	}

	if got.sr == nil {
		t.Fatalf("scanFile returned nil sr (err=%v); did not reach the CPPScan stage", got.err)
	}
	got.sr.mu.Lock()
	done, srErr := got.sr.done, got.sr.err
	got.sr.mu.Unlock()
	if done && errors.Is(srErr, context.Canceled) {
		t.Fatal("scanFile cached a context-canceled error in the shared scanResult; later scans would treat the header as failed and miss deps")
	}
}

func TestFilesystemUpdate(t *testing.T) {
	dir := t.TempDir()
	ctx := t.Context()
	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}

	fsys := &filesystem{
		hashfs: hashFS,
		seed:   maphash.MakeSeed(),
	}
	hashFS.Notify(fsys.update)

	done := make(chan struct{})
	go func() {
		err := hashFS.Mkdir(ctx, dir, path.New("out/siso/gen"), nil, nil)
		if err != nil {
			t.Errorf("hashFS.Mkdir(ctx, %q, %q)=%v; want nil err", dir, "out/siso/gen", err)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatalf("too slow hashFS.Mkdir")
	}
}

// TestSetFileNegativeDoesNotWinOverRegular: setFile caches nil for not-found
// paths. A regular-file scan that adopts the shared pointer must not get a
// previously-cached nil, which would nil-deref in scanFile.
func TestSetFileNegativeDoesNotWinOverRegular(t *testing.T) {
	fsys := &filesystem{}
	const root, fname = "/work", "out/gen/header.h"
	// A not-found / non-regular scan caches a negative entry first.
	fsys.setFile(root, fname, nil)
	// A later scan stats the file as regular and adopts the shared pointer.
	got := fsys.setFile(root, fname, &scanResult{})
	if got == nil {
		t.Fatal("setFile returned nil after a negative entry was cached; scanFile would nil-deref on sr.mu.Lock()")
	}
}
