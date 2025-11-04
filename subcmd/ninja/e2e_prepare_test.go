// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/hashfs"
)

func TestBuild_PrepareHeaderOnly(t *testing.T) {
	ctx := t.Context()
	dir := tempDir(t)

	ninja := func(t *testing.T, targets []string) (build.Stats, error) {
		t.Helper()
		build.SetExperimentForTest("prepare-header-only")
		defer func() {
			build.SetExperimentForTest("")
		}()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile: ".siso_fs_state",
		})
		defer cleanup()
		opt.Prepare = true
		return runNinja(ctx, "build.ninja", graph, opt, targets, runNinjaOpts{})
	}

	setupFiles(t, dir, t.Name(), nil)

	_, err := ninja(t, []string{"../../foo/foo.cc^"})
	if err != nil {
		t.Fatalf("ninja %v; want nil error", err)
	}

	t.Logf("-- check if the header and source files are generated")
	for _, fname := range []string{
		"out/siso/gen/bar.pb.h",
		"out/siso/gen/bar.pb.cc",
	} {
		_, err = os.Stat(filepath.Join(dir, fname))
		if err != nil {
			t.Errorf("Stat(%q)=%v; want nil err", fname, err)
		}
	}

	t.Logf("-- check if other files like Python file and object file are not generated")
	for _, fname := range []string{
		"out/siso/gen/foo_pb.py",
		"out/siso/foo.o",
	} {
		_, err = os.Stat(filepath.Join(dir, fname))
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Stat(%q)=%v; want %v", fname, err, fs.ErrNotExist)
		}
	}
}
