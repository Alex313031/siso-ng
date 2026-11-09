// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package e2etests

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"go.chromium.org/build/siso/build/ninjabuild"
	"go.chromium.org/build/siso/hashfs"
)

// gen/bla as a directory output, produced by gendir.
const kindFlipDirNinja = `rule gendir
  command = python3 ../../tools/gendir.py --out_dir=${out_dir} ${in}

build gen/bla/: gendir ../../base/input | ../../tools/gendir.py
  out_dir = gen/bla

build all: phony gen/bla/

build build.ninja: phony
`

// gen/bla as a file output, produced by genfile (same path as the dir variant, so switching flips the target's kind).
// The rule clears its own output: siso does not auto-remove a stale directory for a non-slash output (it cannot tell a kind flip from a legitimate directory-valued output), as Chromium's copy_bundle_data already does. genfile.py does the clearing itself (a cross-platform stand-in for a `rm -rf ${out} &&` prefix, which has no Windows equivalent).
const kindFlipFileNinja = `rule genfile
  command = python3 ../../tools/genfile.py --out=${out} ${in}

build gen/bla: genfile ../../base/input | ../../tools/genfile.py

build all: phony gen/bla

build build.ninja: phony
`

// base/input holds "hello\n"; both gendir and genfile copy it verbatim, so every produced file must contain exactly this.
const kindFlipContent = "hello\n"

func writeKindFlipNinja(t *testing.T, dir, content string) {
	t.Helper()
	p := filepath.Join(dir, "out/siso/build.ninja")
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func runKindFlipBuild(ctx context.Context, t *testing.T, dir string) error {
	t.Helper()
	opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
		StateFile:   ".siso_fs_state",
		OutputLocal: func(context.Context, string) bool { return true },
	})
	defer cleanup()
	_, err := ninjabuild.Run(ctx, graph, opt, nil, ninjabuild.RunNinjaOpts{})
	return err
}

// TestBuild_DirOutputToFileOutput covers an incremental flip where a directory output (gen/bla/) becomes a file output (gen/bla): the producing rule clears the stale directory (rm -rf), then the build succeeds and the hashfs dir entry is replaced by a file entry.
func TestBuild_DirOutputToFileOutput(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	requirePython3(t)
	ctx := t.Context()
	dir := tempDir(t)
	setupFiles(t, dir, "TestBuild_KindFlip", nil)
	gen := filepath.Join(dir, "out/siso/gen/bla")

	t.Logf("-- build 1: gen/bla/ as a directory output")
	writeKindFlipNinja(t, dir, kindFlipDirNinja)
	if err := runKindFlipBuild(ctx, t, dir); err != nil {
		t.Fatalf("build1 (dir output): %v", err)
	}
	if fi, err := os.Stat(gen); err != nil || !fi.IsDir() {
		t.Fatalf("after build1: gen/bla should be a directory: fi=%v err=%v", fi, err)
	}
	// The directory members must hold the produced content, not just exist.
	if got := readFile(t, filepath.Join(gen, "sub", "nested")); got != kindFlipContent {
		t.Errorf("%s = %q, want %q", filepath.Join(gen, "sub", "nested"), got, kindFlipContent)
	}
	if got := readFile(t, filepath.Join(gen, "data")); got != kindFlipContent {
		t.Errorf("%s = %q, want %q", filepath.Join(gen, "data"), got, kindFlipContent)
	}

	t.Logf("-- build 2: gen/bla as a file output (kind flip dir->file)")
	writeKindFlipNinja(t, dir, kindFlipFileNinja)
	if err := runKindFlipBuild(ctx, t, dir); err != nil {
		t.Fatalf("build2 (file output after dir output): %v\n(stale directory not reconciled before the action ran?)", err)
	}
	fi, err := os.Stat(gen)
	if err != nil {
		t.Fatalf("after build2: gen/bla missing: %v", err)
	}
	if fi.IsDir() {
		t.Errorf("after build2: gen/bla is still a directory; want a regular file")
	}
	// The file output must hold the produced content.
	if got := readFile(t, gen); got != kindFlipContent {
		t.Errorf("%s = %q, want %q", gen, got, kindFlipContent)
	}
	if _, err := os.Stat(filepath.Join(gen, "sub")); err == nil {
		t.Errorf("after build2: stale gen/bla/sub still present; the old directory tree was not removed")
	}
}

// TestBuild_FileOutputToDirOutput_NoState covers the file->dir flip when the prior siso state is gone but the stale file remains on disk: ClearStaleFileForDirOutput must consult disk, not only the in-memory hashfs entry, or MkdirAll fails ENOTDIR and aborts the build.
func TestBuild_FileOutputToDirOutput_NoState(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	requirePython3(t)
	ctx := t.Context()
	dir := tempDir(t)
	setupFiles(t, dir, "TestBuild_KindFlip", nil)
	gen := filepath.Join(dir, "out/siso/gen/bla")

	t.Logf("-- build 1: gen/bla as a file output")
	writeKindFlipNinja(t, dir, kindFlipFileNinja)
	if err := runKindFlipBuild(ctx, t, dir); err != nil {
		t.Fatalf("build1 (file output): %v", err)
	}
	if fi, err := os.Stat(gen); err != nil || fi.IsDir() {
		t.Fatalf("after build1: gen/bla should be a regular file: fi=%v err=%v", fi, err)
	}
	if got := readFile(t, gen); got != kindFlipContent {
		t.Errorf("%s = %q, want %q", gen, got, kindFlipContent)
	}

	// Simulate state loss: the on-disk file survives but siso has no record of it.
	if err := os.Remove(filepath.Join(dir, "out/siso/.siso_fs_state")); err != nil {
		t.Fatalf("remove siso state: %v", err)
	}

	t.Logf("-- build 2: gen/bla/ as a directory output, with no prior state")
	writeKindFlipNinja(t, dir, kindFlipDirNinja)
	if err := runKindFlipBuild(ctx, t, dir); err != nil {
		t.Fatalf("build2 (dir output after file output, no state): %v\n(stale on-disk file not cleared before MkdirAll -> ENOTDIR?)", err)
	}
	fi, err := os.Stat(gen)
	if err != nil {
		t.Fatalf("after build2: gen/bla missing: %v", err)
	}
	if !fi.IsDir() {
		t.Errorf("after build2: gen/bla is not a directory; want a directory output")
	}
	if _, err := os.Stat(filepath.Join(gen, "sub", "nested")); err != nil {
		t.Errorf("after build2: gen/bla/sub/nested missing: %v (directory output not produced)", err)
	} else {
		if got := readFile(t, filepath.Join(gen, "sub", "nested")); got != kindFlipContent {
			t.Errorf("%s = %q, want %q", filepath.Join(gen, "sub", "nested"), got, kindFlipContent)
		}
		if got := readFile(t, filepath.Join(gen, "data")); got != kindFlipContent {
			t.Errorf("%s = %q, want %q", filepath.Join(gen, "data"), got, kindFlipContent)
		}
	}
}

// TestBuild_FileOutputToDirOutput covers the reverse flip: a file output (gen/bla) becomes a directory output (gen/bla/), so the stale file must be removed before MkdirAll, which would otherwise fail ENOTDIR.
func TestBuild_FileOutputToDirOutput(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	requirePython3(t)
	ctx := t.Context()
	dir := tempDir(t)
	setupFiles(t, dir, "TestBuild_KindFlip", nil)
	gen := filepath.Join(dir, "out/siso/gen/bla")

	t.Logf("-- build 1: gen/bla as a file output")
	writeKindFlipNinja(t, dir, kindFlipFileNinja)
	if err := runKindFlipBuild(ctx, t, dir); err != nil {
		t.Fatalf("build1 (file output): %v", err)
	}
	if fi, err := os.Stat(gen); err != nil || fi.IsDir() {
		t.Fatalf("after build1: gen/bla should be a regular file: fi=%v err=%v", fi, err)
	}
	if got := readFile(t, gen); got != kindFlipContent {
		t.Errorf("%s = %q, want %q", gen, got, kindFlipContent)
	}

	t.Logf("-- build 2: gen/bla/ as a directory output (kind flip file->dir)")
	writeKindFlipNinja(t, dir, kindFlipDirNinja)
	if err := runKindFlipBuild(ctx, t, dir); err != nil {
		t.Fatalf("build2 (dir output after file output): %v\n(stale file not reconciled before MkdirAll?)", err)
	}
	fi, err := os.Stat(gen)
	if err != nil {
		t.Fatalf("after build2: gen/bla missing: %v", err)
	}
	if !fi.IsDir() {
		t.Errorf("after build2: gen/bla is not a directory; want a directory output")
	}
	if _, err := os.Stat(filepath.Join(gen, "sub", "nested")); err != nil {
		t.Errorf("after build2: gen/bla/sub/nested missing: %v (directory output not produced)", err)
	} else {
		if got := readFile(t, filepath.Join(gen, "sub", "nested")); got != kindFlipContent {
			t.Errorf("%s = %q, want %q", filepath.Join(gen, "sub", "nested"), got, kindFlipContent)
		}
		if got := readFile(t, filepath.Join(gen, "data")); got != kindFlipContent {
			t.Errorf("%s = %q, want %q", filepath.Join(gen, "data"), got, kindFlipContent)
		}
	}
}
