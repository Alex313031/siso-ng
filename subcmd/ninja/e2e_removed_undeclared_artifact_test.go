// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/hashfs"
)

func TestBuild_RemovedUndeclaredArtifact(t *testing.T) {
	ctx := t.Context()
	dir := tempDir(t)

	ninja := func(t *testing.T) (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:   ".siso_fs_state",
			OutputLocal: func(context.Context, string) bool { return true },
		})
		defer cleanup()
		return runNinja(ctx, "build.ninja", graph, opt, nil, runNinjaOpts{})
	}
	setupFiles(t, dir, t.Name(), nil)

	t.Logf("-- first build")
	stats, err := ninja(t)
	if err != nil {
		t.Fatalf("ninja err: %v", err)
	}
	if stats.Done != stats.Total || stats.Total != 7 {
		t.Errorf("done=%d total=%d; want done=total=7: %#v", stats.Done, stats.Total, stats)
	}

	for _, fname := range []string{
		"xcuitests_module.xctest/Info.plist",
		"xcuitests_module.xctest/provision",
		"xcuitests_module.xctest/xcuitests_module",
		"module-Runner.app/Plugins/xcuitests_module.xctest/Info.plist",
		"module-Runner.app/Plugins/xcuitests_module.xctest/provision",
		"module-Runner.app/Plugins/xcuitests_module.xctest/xcuitests_module",
	} {
		_, err := os.Stat(filepath.Join(dir, "out/siso", fname))
		if err != nil {
			t.Errorf("Stat(%q)=%v; want nil error", fname, err)
		}
	}

	t.Logf("-- confirm no-op")
	stats, err = ninja(t)
	if err != nil {
		t.Fatalf("ninja err: %v", err)
	}
	if stats.Done != stats.Total || stats.Skipped != stats.Total || stats.Total != 7 {
		t.Errorf("done=%d skipped=%d total=%d; want done=skipped=total=7: %#v", stats.Done, stats.Skipped, stats.Total, stats)
	}

	t.Logf("-- disable --provision")
	buf, err := os.ReadFile(filepath.Join(dir, "out/siso/build.ninja"))
	if err != nil {
		t.Fatal(err)
	}
	buf = bytes.ReplaceAll(buf, []byte(" --provision"), nil)
	err = os.WriteFile(filepath.Join(dir, "out/siso/build.ninja"), buf, 0644)
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("-- second build")

	stats, err = ninja(t)
	if err != nil {
		t.Fatalf("ninja err: %v", err)
	}
	if stats.Done != stats.Total || stats.Total != 7 {
		t.Errorf("done=%d total=%d; want done=total=7: %#v", stats.Done, stats.Total, stats)
	}

	for _, fname := range []string{
		"xcuitests_module.xctest/Info.plist",
		"xcuitests_module.xctest/xcuitests_module",
		"module-Runner.app/Plugins/xcuitests_module.xctest/Info.plist",
		"module-Runner.app/Plugins/xcuitests_module.xctest/xcuitests_module",
	} {
		_, err := os.Stat(filepath.Join(dir, "out/siso", fname))
		if err != nil {
			t.Errorf("Stat(%q)=%v; want nil error", fname, err)
		}
	}

	for _, fname := range []string{
		"xcuitests_module.xctest/provision",
		"module-Runner.app/Plugins/xcuitests_module.xctest/provision",
	} {
		_, err := os.Stat(filepath.Join(dir, "out/siso", fname))
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Stat(%q)=%v; want %v", fname, err, fs.ErrNotExist)
		}
	}

	t.Logf("-- confirm no-op")
	stats, err = ninja(t)
	if err != nil {
		t.Fatalf("ninja err: %v", err)
	}
	if stats.Done != stats.Total || stats.Skipped != stats.Total || stats.Total != 7 {
		t.Errorf("done=%d skipped=%d total=%d; want done=skipped=total=7: %#v", stats.Done, stats.Skipped, stats.Total, stats)
	}
}
