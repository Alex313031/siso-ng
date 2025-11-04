// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/hashfs"
)

// Test symlink won't modify mtime of symlink's target.
func TestBuild_Symlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink not available on windows")
		return
	}
	ctx := t.Context()
	dir := tempDir(t)

	ninja := func(t *testing.T) (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile: ".siso_fs_state",
		})
		defer cleanup()
		return runNinja(ctx, "build.ninja", graph, opt, nil, runNinjaOpts{})
	}
	setupFiles(t, dir, t.Name(), nil)

	stats, err := ninja(t)
	if err != nil {
		t.Fatalf("ninja %v: want nil err", err)
	}
	if stats.Done != stats.Total {
		t.Errorf("stats.Done=%d Total=%d", stats.Done, stats.Total)
	}

	st, err := hashfs.Load(ctx, hashfs.Option{StateFile: filepath.Join(dir, "out/siso/.siso_fs_state")})
	if err != nil {
		t.Errorf("hashfs.Load=%v; want nil err", err)
	}
	m := hashfs.StateMap(st)
	e1, ok := m[filepath.Join(dir, "out/siso/out1")]
	if !ok {
		t.Errorf("out1 not found: %v", m)
	} else {
		mtime := time.Unix(0, e1.Id.ModTime)
		fi, err := os.Lstat(filepath.Join(dir, "out/siso/out1"))
		if err != nil {
			t.Errorf("out1 not found on disk: %v", err)
		} else if !mtime.Equal(fi.ModTime()) {
			t.Errorf("out1 modtime does not match: state=%s disk=%s", mtime, fi.ModTime())
		}
	}

	t.Logf("-- check confirm no-op")
	stats, err = ninja(t)
	if err != nil {
		t.Fatalf("ninja %v; want nil err", err)
	}
	if stats.Skipped != stats.Total {
		t.Errorf("stats.Skipped=%d Total=%d", stats.Skipped, stats.Total)
	}
	t.Logf("-- recreate symlink")
	target, err := os.Readlink(filepath.Join(dir, "out/siso/out2"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("-- symlink out/siso/out2 -> %s", target)
	err = os.Remove(filepath.Join(dir, "out/siso/out2"))
	if err != nil {
		t.Fatal(err)
	}
	err = os.Symlink(target, filepath.Join(dir, "out/siso/out2"))
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("-- check confirm no-op even if symlink is recreated")
	stats, err = ninja(t)
	if err != nil {
		t.Fatalf("ninja %v; want nil err", err)
	}
	if stats.Skipped != stats.Total {
		t.Errorf("stats.Skipped=%d Total=%d", stats.Skipped, stats.Total)
	}
}

// Test symlink source uses mtime of symlink's target.
func TestBuild_SymlinkSource(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink not available on windows")
		return
	}
	ctx := t.Context()
	dir := tempDir(t)

	ninja := func(t *testing.T) (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile: ".siso_fs_state",
		})
		defer cleanup()
		return runNinja(ctx, "build.ninja", graph, opt, nil, runNinjaOpts{})
	}
	setupFiles(t, dir, t.Name(), nil)

	err := os.Symlink("input_file1", filepath.Join(dir, "input_symlink"))
	if err != nil {
		t.Fatal(err)
	}

	stats, err := ninja(t)
	if err != nil {
		t.Fatalf("ninja %v: want nil err", err)
	}
	if stats.Done != stats.Total {
		t.Errorf("stats done=%d total=%d", stats.Done, stats.Total)
	}

	input1, err := os.ReadFile(filepath.Join(dir, "input_file1"))
	if err != nil {
		t.Fatal(err)
	}
	input2, err := os.ReadFile(filepath.Join(dir, "input_file2"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(filepath.Join(dir, "out/siso/out"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(input1, out) {
		t.Errorf("unexpected out: got:\n%s\nwant:\n%s", out, input1)
	}

	t.Logf("-- check confirm no-op")
	stats, err = ninja(t)
	if err != nil {
		t.Fatalf("ninja %v; want nil err", err)
	}
	if stats.Skipped != stats.Total {
		t.Errorf("stats.Skipped=%d Total=%d", stats.Skipped, stats.Total)
	}

	touchFile(t, dir, "input_file1")
	t.Logf("-- check action triggered if input_symlink target is updated")
	stats, err = ninja(t)
	if err != nil {
		t.Fatalf("ninja %v; want nil err", err)
	}
	if stats.Skipped != 0 {
		t.Errorf("not triggered? stats=%#v", stats)
	}
	out, err = os.ReadFile(filepath.Join(dir, "out/siso/out"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(input1, out) {
		t.Errorf("unexpected out: got:\n%s\nwant:\n%s", out, input1)
	}

	modifyFile(t, dir, "input_file1", func(data []byte) []byte {
		return append(data, []byte("modified\n")...)
	})
	input1Modified, err := os.ReadFile(filepath.Join(dir, "input_file1"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("-- check action triggered if input_symlink target file is modified")
	stats, err = ninja(t)
	if err != nil {
		t.Fatalf("ninja %v; want nil err", err)
	}
	if stats.Skipped != 0 {
		t.Errorf("not triggered? stats=%#v", stats)
	}
	out, err = os.ReadFile(filepath.Join(dir, "out/siso/out"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(input1Modified, out) {
		t.Errorf("unexpected out: got:\n%s\nwant:\n%s", out, input1Modified)
	}

	t.Logf("-- check action triggered if symlink is updated")
	lastTimestamp := time.Now()
	for {
		err = os.Remove(filepath.Join(dir, "input_symlink"))
		if err != nil {
			t.Fatal(err)
		}
		err = os.Symlink("input_file2", filepath.Join(dir, "input_symlink"))
		if err != nil {
			t.Fatal(err)
		}
		fi, err := os.Lstat(filepath.Join(dir, "input_symlink"))
		if err != nil {
			t.Fatal(err)
		}
		if fi.ModTime().After(lastTimestamp) {
			break
		}
	}
	stats, err = ninja(t)
	if err != nil {
		t.Fatalf("ninja %v; want nil err", err)
	}
	if stats.Skipped != 0 {
		t.Errorf("not triggered? stats=%#v", stats)
	}
	out, err = os.ReadFile(filepath.Join(dir, "out/siso/out"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(input2, out) {
		t.Errorf("unexpected out: got:\n%s\nwant:\n%s", out, input2)
	}

}

// Test symlink source uses mtime of symlink's target.
func TestBuild_SymlinkSourceSymlinkDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink not available on windows")
		return
	}
	ctx := t.Context()
	dir := tempDir(t)

	ninja := func(t *testing.T) (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile: ".siso_fs_state",
		})
		defer cleanup()
		return runNinja(ctx, "build.ninja", graph, opt, nil, runNinjaOpts{})
	}
	setupFiles(t, dir, t.Name(), nil)

	err := os.Symlink("../libutils/include/utils/", filepath.Join(dir, "system/core/include/utils"))
	if err != nil {
		t.Fatal(err)
	}
	err = os.Symlink("../../binder/include/utils/Errors.h", filepath.Join(dir, "system/core/libutils/include/utils/Errors.h"))
	if err != nil {
		t.Fatal(err)
	}

	// system/core/include/utils/Errors.h should be resolved as
	// system/core/libutils/binder/include/utils/Errors.h

	stats, err := ninja(t)
	if err != nil {
		t.Fatalf("ninja %v: want nil err", err)
	}
	if stats.Done != stats.Total {
		t.Errorf("stats done=%d total=%d", stats.Done, stats.Total)
	}
	t.Logf("-- check confirm no-op")
	stats, err = ninja(t)
	if err != nil {
		t.Fatalf("ninja %v; want nil err", err)
	}
	if stats.Skipped != stats.Total {
		t.Errorf("stats.Skipped=%d Total=%d", stats.Skipped, stats.Total)
	}
}

func TestBuild_SymlinkGeneratedDangling(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink not available on windows")
		return
	}
	ctx := t.Context()
	dir := tempDir(t)

	ninja := func(t *testing.T) (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile: ".siso_fs_state",
		})
		defer cleanup()
		return runNinja(ctx, "build.ninja", graph, opt, nil, runNinjaOpts{})
	}
	setupFiles(t, dir, t.Name(), nil)

	stats, err := ninja(t)
	if err != nil {
		t.Fatalf("ninja %v: want nil err", err)
	}
	if stats.Done != stats.Total {
		t.Errorf("stats done=%d total=%d %#v", stats.Done, stats.Total, stats)
	}
	buf, err := os.ReadFile(filepath.Join(dir, "out/siso/out"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf, []byte("/module")) {
		t.Errorf("out/siso/out=%q; want=%q", buf, "/module")
	}

	t.Logf("-- check confirm no-op")
	stats, err = ninja(t)
	if err != nil {
		t.Fatalf("ninja %v; want nil err", err)
	}
	if stats.Skipped != stats.Total {
		t.Errorf("stats.Skipped=%d Total=%d %#v", stats.Skipped, stats.Total, stats)
	}
}
