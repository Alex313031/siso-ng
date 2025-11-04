// Copyright 2025 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"testing"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/hashfs"
)

func TestBuild_MissingOutput(t *testing.T) {
	ctx := t.Context()
	dir := tempDir(t)

	defer build.SetExperimentForTest("")

	ninja := func(t *testing.T) (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile: ".siso_fs_state",
		})
		defer cleanup()
		return runNinja(ctx, "build.ninja", graph, opt, []string{"b"}, runNinjaOpts{})
	}

	setupFiles(t, dir, t.Name(), nil)
	t.Logf("-- normal build")
	stats, err := ninja(t)
	if err == nil {
		t.Errorf("ninja succeeded. want error due to missing outputs")
	}
	if stats.Fail != 1 {
		t.Errorf("fail=%d; want fail=1 %#v", stats.Fail, stats)
	}

	build.SetExperimentForTest("ignore-missing-outputs")
	t.Logf("-- build with SISO_EXPERIMENTS=ignore-missing-outputs")
	stats, err = ninja(t)
	if err != nil {
		t.Errorf("ninja %v; want nil", err)
	}
	if stats.Done != stats.Total || stats.Total != 2 || stats.Local != 2 || stats.Fail != 0 {
		t.Errorf("done=%d local=%d fail=%d total=%d; want done=2 local=2 fail=0 total=2; %#v", stats.Done, stats.Local, stats.Fail, stats.Total, stats)
	}

	t.Logf("-- build again with SISO_EXPERIMENTS=ignore-missing-outputs. not noop due to missing outputs")
	stats, err = ninja(t)
	if err != nil {
		t.Errorf("ninja %v; want nil", err)
	}
	if stats.Done != stats.Total || stats.Total != 2 || stats.Local != 2 || stats.Fail != 0 {
		t.Errorf("done=%d local=%d fail=%d total=%d; want done=2 local=2 fail=0 total=2; %#v", stats.Done, stats.Local, stats.Fail, stats.Total, stats)
	}
}
