// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"testing"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/hashfs"
)

func TestBuild_PhonyOutput(t *testing.T) {
	ctx := t.Context()
	dir := tempDir(t)

	ninja := func(t *testing.T) (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile: ".siso_fs_state",
		})
		defer cleanup()
		return runNinja(ctx, "build.ninja", graph, opt, []string{"nothing"}, runNinjaOpts{})
	}

	setupFiles(t, dir, t.Name(), nil)
	t.Logf("-- first build")
	stats, err := ninja(t)
	if err != nil {
		t.Fatalf("ninja %v", err)
	}
	if stats.Done != stats.Total || stats.Total != 1 || stats.Local != 1 {
		t.Errorf("done=%d total=%d local=%d; want done=1 total=1 local=1; %#v", stats.Done, stats.Total, stats.Local, stats)
	}

	t.Logf("-- second build")
	stats, err = ninja(t)
	if err != nil {
		t.Fatalf("ninja %v", err)
	}
	if stats.Done != stats.Total || stats.Total != 1 || stats.Local != 1 {
		t.Errorf("done=%d total=%d local=%d; want done=1 total=1 local=1; %#v", stats.Done, stats.Total, stats.Local, stats)
	}
}

func TestBuild_PhonyOutputDepError(t *testing.T) {
	ctx := t.Context()
	dir := tempDir(t)

	ninja := func(t *testing.T) (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile: ".siso_fs_state",
		})
		defer cleanup()
		return runNinja(ctx, "build.ninja", graph, opt, []string{"bar"}, runNinjaOpts{})
	}

	setupFiles(t, dir, t.Name(), nil)
	_, err := ninja(t)
	if err == nil {
		t.Fatalf("ninja %v; should error", err)
	}
	t.Logf("ninja %v", err)
}

func TestBuild_PhonyOutputOrderOnly(t *testing.T) {
	ctx := t.Context()
	dir := tempDir(t)

	ninja := func(t *testing.T) (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile: ".siso_fs_state",
		})
		defer cleanup()
		return runNinja(ctx, "build.ninja", graph, opt, []string{"bar"}, runNinjaOpts{})
	}

	setupFiles(t, dir, t.Name(), nil)
	stats, err := ninja(t)
	if err != nil {
		t.Fatalf("ninja %v", err)
	}
	if stats.Done != stats.Total || stats.Total != 2 || stats.Local != 2 {
		t.Errorf("done=%d total=%d local=%d; want done=2 total=2 local=2; %#v", stats.Done, stats.Total, stats.Local, stats)
	}
}
