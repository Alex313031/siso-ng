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

	"go.chromium.org/build/siso/build/ninjabuild"
	"go.chromium.org/build/siso/hashfs"
)

// TestBuild_DirOutputActionCreatesDir verifies that siso does not pre-create a
// directory output: the action creates its own output directory, the same way
// it creates a file output. This matches the REAPI worker, which makes only the
// directories leading up to an output and never the output directory itself, so
// local execution behaves identically to remote.
//
// The copytree action uses shutil.copytree, which fails if the destination
// already exists. With the pre-created-empty-directory behavior it would raise
// FileExistsError (build failure); it would also have nested the copy under
// copytree/src for a cp -r style command. Both are guarded here.
func TestBuild_DirOutputActionCreatesDir(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	requirePython3(t)
	ctx := t.Context()
	dir := tempDir(t)
	setupFiles(t, dir, t.Name(), nil)
	out := filepath.Join(dir, "out/siso/copytree")

	opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
		StateFile:   ".siso_fs_state",
		OutputLocal: func(context.Context, string) bool { return true },
	})
	defer cleanup()
	if _, err := ninjabuild.Run(ctx, graph, opt, []string{"all"}, ninjabuild.RunNinjaOpts{}); err != nil {
		t.Fatalf("ninja err: %v (the action could not create its output directory; siso may have pre-created it)", err)
	}

	if _, err := os.Stat(filepath.Join(out, "foo.txt")); err != nil {
		t.Errorf("copytree/foo.txt missing: %v; the action did not produce the directory as expected", err)
	}
	if _, err := os.Stat(filepath.Join(out, "src")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("copytree/src present (stat err=%v); the output directory was pre-created, nesting the copy under it", err)
	}
}
