// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package hashfs_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/path"
)

func TestResetDirOutput(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	hfs, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatalf("hashfs.New=_, %v; want nil err", err)
	}
	defer hfs.Close(ctx)

	now := time.Now()
	cmdhash := []byte("cmdhash")
	for _, fname := range []string{"gen/out/a", "gen/out/sub/b"} {
		full := filepath.Join(dir, fname)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(fname), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := hfs.WriteFile(ctx, dir, path.Path(fname), []byte(fname), false, now, cmdhash, nil); err != nil {
			t.Fatalf("WriteFile(%s)=%v; want nil err", fname, err)
		}
	}

	if err := hfs.ResetDirOutput(ctx, dir, "gen/out"); err != nil {
		t.Fatalf("ResetDirOutput=%v; want nil err", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "gen/out")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Lstat(gen/out)=_, %v; want %v", err, fs.ErrNotExist)
	}
	if _, err := hfs.Stat(ctx, dir, "gen/out/a"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Stat(gen/out/a)=_, %v; want %v", err, fs.ErrNotExist)
	}
	// The parent must exist so the action can create the dir output itself.
	if fi, err := os.Lstat(filepath.Join(dir, "gen")); err != nil || !fi.IsDir() {
		t.Errorf("Lstat(gen)=%v, %v; want a directory", fi, err)
	}
}

// The reset must not shadow the directory the action then creates in its place.
func TestResetDirOutput_RecreateAfterReset(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	hfs, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatalf("hashfs.New=_, %v; want nil err", err)
	}
	defer hfs.Close(ctx)

	now := time.Now()
	cmdhash := []byte("cmdhash")
	for _, fname := range []string{"gen/out/a", "gen/out/b"} {
		full := filepath.Join(dir, fname)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(fname), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := hfs.WriteFile(ctx, dir, path.Path(fname), []byte(fname), false, now, cmdhash, nil); err != nil {
			t.Fatalf("WriteFile(%s)=%v; want nil err", fname, err)
		}
	}

	if err := hfs.ResetDirOutput(ctx, dir, "gen/out"); err != nil {
		t.Fatalf("ResetDirOutput=%v; want nil err", err)
	}

	// Do not Stat gen/out here: that records the absence itself and masks the
	// record under test.

	// The action creates its own output directory, with fewer members.
	full := filepath.Join(dir, "gen/out/a")
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}

	fi, err := hfs.Stat(ctx, dir, "gen/out")
	if err != nil {
		t.Errorf("Stat(gen/out)=_, %v; want nil err; the reset's record shadowed the new directory", err)
	} else if !fi.IsDir() {
		t.Errorf("Stat(gen/out).IsDir()=false; want a directory")
	}
	if _, err := hfs.Stat(ctx, dir, "gen/out/a"); err != nil {
		t.Errorf("Stat(gen/out/a)=_, %v; want nil err", err)
	}
	if _, err := hfs.Stat(ctx, dir, "gen/out/b"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Stat(gen/out/b)=_, %v; want %v; the stale member came back", err, fs.ErrNotExist)
	}
}

// A partially failed disk wipe must leave hashfs agreeing with disk: children
// RemoveAll deleted are dropped, the root it could not delete stays recorded.
func TestResetDirOutput_DiskWipeFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("cannot force RemoveAll failure via directory permissions on Windows")
	}
	ctx := t.Context()
	dir := t.TempDir()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	hfs, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatalf("hashfs.New=_, %v; want nil err", err)
	}
	defer hfs.Close(ctx)

	now := time.Now()
	cmdhash := []byte("cmdhash")
	members := []string{"gen/out/a", "gen/out/sub/b"}
	for _, fname := range members {
		full := filepath.Join(dir, fname)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(fname), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := hfs.WriteFile(ctx, dir, path.Path(fname), []byte(fname), false, now, cmdhash, nil); err != nil {
			t.Fatalf("WriteFile(%s)=%v; want nil err", fname, err)
		}
	}

	// Read-only sub: RemoveAll deletes gen/out/a but fails on gen/out/sub/b.
	sub := filepath.Join(dir, "gen/out/sub")
	if err := os.Chmod(sub, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(sub, 0o755)
	})

	err = hfs.ResetDirOutput(ctx, dir, "gen/out")
	if err == nil {
		t.Skip("permissions not enforced (e.g. root); cannot exercise the failure path")
	}

	// hashfs must agree with disk for every member.
	for _, fname := range members {
		_, diskErr := os.Lstat(filepath.Join(dir, fname))
		_, hfsErr := hfs.Stat(ctx, dir, path.Path(fname))
		if errors.Is(diskErr, fs.ErrNotExist) != errors.Is(hfsErr, fs.ErrNotExist) {
			t.Errorf("Stat(%s)=_, %v; disk Lstat=_, %v; hashfs must match disk", fname, hfsErr, diskErr)
		}
	}
}
