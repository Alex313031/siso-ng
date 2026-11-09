// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package e2etests

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/build/ninjabuild"
	"go.chromium.org/build/siso/hashfs"
)

// Chromium's split-DWARF ThinLTO link: __clang_link attaches <output_file>-dwo/
// to the outputs, LLD never clears it, and dump_app_syms.py fails the build on
// an orphan .dwo left by a relink with fewer units.
func TestBuild_SplitDwarfStaleDwoDir(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	testSplitDwarfStaleDwoDir(t, "TestBuild_SplitDwarfStaleDwoDir")
}

// Same scenario, dwo dir declared in build.ninja instead of by a handler.
func TestBuild_SplitDwarfStaleDwoDirNinjaDecl(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	testSplitDwarfStaleDwoDir(t, "TestBuild_SplitDwarfStaleDwoDirNinjaDecl")
}

func testSplitDwarfStaleDwoDir(t *testing.T, testdata string) {
	t.Helper()
	requirePython3(t)
	ctx := t.Context()
	dir := tempDir(t)
	setupFiles(t, dir, testdata, nil)
	dwoDir := filepath.Join(dir, "out/siso/libfoo.so-dwo")

	run := func() (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:   ".siso_fs_state",
			OutputLocal: func(context.Context, string) bool { return true },
		})
		defer cleanup()
		return ninjabuild.Run(ctx, graph, opt, []string{"all"}, ninjabuild.RunNinjaOpts{})
	}

	t.Logf("-- link 1: ThinLTO emits units a, b, c")
	if _, err := run(); err != nil {
		t.Fatalf("build1: ninja err: %v", err)
	}
	for _, name := range []string{"a.dwo", "b.dwo", "c.dwo"} {
		if _, err := os.Stat(filepath.Join(dwoDir, name)); err != nil {
			t.Fatalf("after build1: %s missing: %v", name, err)
		}
	}

	t.Logf("-- shrink the unit list so the relink emits only a.dwo")
	modifyFile(t, dir, "base/units", func([]byte) []byte { return []byte("a\n") })

	t.Logf("-- link 2: dump_syms must not see an orphan .dwo")
	stats, err := run()
	if err != nil {
		t.Errorf("build2: ninja err: %v; a stale .dwo from the previous link survived the relink", err)
	}
	if stats.Skipped == stats.Total {
		t.Errorf("build2 was a no-op (skipped=%d total=%d); the link should have rerun", stats.Skipped, stats.Total)
	}
	if _, err := os.Stat(filepath.Join(dwoDir, "a.dwo")); err != nil {
		t.Errorf("after build2: a.dwo missing: %v", err)
	}
	for _, name := range []string{"b.dwo", "c.dwo"} {
		_, err := os.Stat(filepath.Join(dwoDir, name))
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("after build2: stale %s still present (stat err=%v); the dwo directory output was not reset before the relink", name, err)
		}
	}
}
