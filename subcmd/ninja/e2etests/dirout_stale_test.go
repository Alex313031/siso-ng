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

// TestBuild_DirOutputStaleRemovedOnRerun verifies that re-running a step which
// produces a directory output wipes the prior tree first, so a run that emits
// fewer files does not leave stale members behind. gendir.py only creates the
// files listed in its manifest and never clears the directory itself, so the
// stale files survive unless siso deletes the directory output before exec.
func TestBuild_DirOutputStaleRemovedOnRerun(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	requirePython3(t)
	ctx := t.Context()
	dir := tempDir(t)
	setupFiles(t, dir, t.Name(), nil)
	gen := filepath.Join(dir, "out/siso/gen")

	run := func() build.Stats {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:   ".siso_fs_state",
			OutputLocal: func(context.Context, string) bool { return true },
		})
		defer cleanup()
		stats, err := ninjabuild.Run(ctx, graph, opt, []string{"all"}, ninjabuild.RunNinjaOpts{})
		if err != nil {
			t.Fatalf("ninja err: %v", err)
		}
		return stats
	}

	t.Logf("-- build 1: gen/ holds a, b, sub/c")
	run()
	for _, name := range []string{"a", "b", "sub/c"} {
		if _, err := os.Stat(filepath.Join(gen, name)); err != nil {
			t.Fatalf("after build1: gen/%s missing: %v", name, err)
		}
	}

	t.Logf("-- shrink manifest so the rerun produces only gen/a")
	modifyFile(t, dir, "base/names", func([]byte) []byte { return []byte("a\n") })

	t.Logf("-- build 2: rerun must drop the stale members")
	stats := run()
	if stats.Skipped == stats.Total {
		t.Errorf("build2 was a no-op (skipped=%d total=%d); gendir should have rerun", stats.Skipped, stats.Total)
	}
	if _, err := os.Stat(filepath.Join(gen, "a")); err != nil {
		t.Errorf("after build2: gen/a missing: %v", err)
	}
	for _, name := range []string{"b", "sub/c", "sub"} {
		_, err := os.Stat(filepath.Join(gen, name))
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("after build2: stale gen/%s still present (stat err=%v); the directory output was not wiped before the rerun", name, err)
		}
	}
}
