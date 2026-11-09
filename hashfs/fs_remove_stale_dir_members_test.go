// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package hashfs_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/path"
	"go.chromium.org/build/siso/reapi/merkletree"
)

// dirOutput is the directory output these tests prune.
const dirOutput = "gen/out"

// recordDirOutput records the directory output and the named members as produced by cmdhash, the
// way a recorded action result does, and writes them to disk. A member with a
// trailing slash is a subdirectory: expandDirOutputs keeps the subdir nodes so
// entriesFromResult records one per subdirectory, and the prune relies on that
// (an unrecorded subdirectory is removed with everything under it).
func recordDirOutput(ctx context.Context, t *testing.T, hfs *hashfs.HashFS, root string, cmdhash []byte, members ...string) {
	t.Helper()
	mkdir := func(name string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := hfs.Mkdir(ctx, root, path.Path(name), cmdhash, nil); err != nil {
			t.Fatalf("Mkdir(%s)=%v; want nil err", name, err)
		}
	}
	mkdir(dirOutput)
	for _, m := range members {
		if name, isDir := strings.CutSuffix(m, "/"); isDir {
			mkdir(name)
			continue
		}
		full := filepath.Join(root, m)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(m), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := hfs.WriteFile(ctx, root, path.Path(m), []byte(m), false, time.Now(), cmdhash, nil); err != nil {
			t.Fatalf("WriteFile(%s)=%v; want nil err", m, err)
		}
	}
}

// writeUnrecorded puts a file on disk that hashfs does not record as a member.
func writeUnrecorded(t *testing.T, root, name string) {
	t.Helper()
	full := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(name), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveStaleDirMembers(t *testing.T) {
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

	cmdhash := []byte("cmdhash")
	recordDirOutput(ctx, t, hfs, dir, cmdhash, "gen/out/keep.txt", "gen/out/sub/", "gen/out/sub/keep2.txt")
	writeUnrecorded(t, dir, "gen/out/stale.txt")
	writeUnrecorded(t, dir, "gen/out/staledir/stale2.txt")

	if err := hfs.RemoveStaleDirMembers(ctx, dir, dirOutput); err != nil {
		t.Fatalf("RemoveStaleDirMembers=%v; want nil err", err)
	}
	for _, name := range []string{"gen/out", "gen/out/keep.txt", "gen/out/sub/keep2.txt"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s: %v; want present (recorded with the directory's cmdhash)", name, err)
		}
	}
	for _, name := range []string{"gen/out/stale.txt", "gen/out/staledir"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s: %v; want %v", name, err, fs.ErrNotExist)
		}
	}
}

// A stale member that a ReadDir walk already picked up into hashfs is still
// pruned: the walk records it without the directory's cmdhash (an unowned
// record), so ownership distinguishes it from a real member. The unowned
// records go too, so the pruned members no longer show up in Stat, ReadDir or
// expanded inputs.
func TestRemoveStaleDirMembers_UnownedRecord(t *testing.T) {
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

	cmdhash := []byte("cmdhash")
	recordDirOutput(ctx, t, hfs, dir, cmdhash, "gen/out/keep.txt")
	writeUnrecorded(t, dir, "gen/out/stale.txt")
	writeUnrecorded(t, dir, "gen/out/staledir/stale2.txt")

	// Let a walk pick up the stale members into hashfs: expanding the
	// directory as an input walks it recursively (and memoizes the
	// expansion), picking up the stale subtree too.
	before, err := hfs.Entries(ctx, dir, []path.Path{dirOutput + "/"})
	if err != nil {
		t.Fatalf("Entries before prune=%v; want nil err", err)
	}
	if got := entryNames(before); !slices.Contains(got, "gen/out/staledir/stale2.txt") {
		t.Fatalf("Entries before prune=%v; want the stale subtree picked up", got)
	}
	for _, name := range []string{"gen/out/stale.txt", "gen/out/staledir", "gen/out/staledir/stale2.txt"} {
		if _, err := hfs.Stat(ctx, dir, path.Path(name)); err != nil {
			t.Fatalf("Stat(%s) after walk=%v; want it picked up", name, err)
		}
	}

	if err := hfs.RemoveStaleDirMembers(ctx, dir, dirOutput); err != nil {
		t.Fatalf("RemoveStaleDirMembers=%v; want nil err", err)
	}
	for _, name := range []string{"gen/out/stale.txt", "gen/out/staledir"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s: %v; want %v; an unowned record must not count as a member", name, err, fs.ErrNotExist)
		}
	}
	if _, err := os.Lstat(filepath.Join(dir, "gen/out/keep.txt")); err != nil {
		t.Errorf("keep.txt: %v; want present", err)
	}

	// The hashfs view must match the disk.
	for _, name := range []string{"gen/out/stale.txt", "gen/out/staledir", "gen/out/staledir/stale2.txt"} {
		if _, err := hfs.Stat(ctx, dir, path.Path(name)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("hashfs Stat(%s)=%v; want %v; the unowned record outlived the prune", name, err, fs.ErrNotExist)
		}
	}
	if _, err := hfs.Stat(ctx, dir, "gen/out/keep.txt"); err != nil {
		t.Errorf("hashfs Stat(keep.txt)=%v; want present", err)
	}
	dents, err := hfs.ReadDir(ctx, dir, dirOutput)
	if err != nil {
		t.Fatalf("ReadDir after prune=%v; want nil err", err)
	}
	var names []string
	for _, de := range dents {
		names = append(names, de.Name())
	}
	if want := []string{"keep.txt"}; !slices.Equal(names, want) {
		t.Errorf("ReadDir after prune=%v; want %v", names, want)
	}
	after, err := hfs.Entries(ctx, dir, []path.Path{dirOutput + "/"})
	if err != nil {
		t.Fatalf("Entries after prune=%v; want nil err", err)
	}
	if got, want := entryNames(after), []string{"gen/out/keep.txt"}; !slices.Equal(got, want) {
		t.Errorf("Entries after prune=%v; want %v; a stale expansion survived the prune", got, want)
	}
}

// entryNames returns the sorted names of the file entries in ents.
func entryNames(ents []merkletree.Entry) []string {
	var names []string
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		names = append(names, string(e.Name))
	}
	slices.Sort(names)
	return names
}

// A recorded symlink member survives; a stale one is removed as the link, never
// followed to its target.
func TestRemoveStaleDirMembers_Symlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on Windows")
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

	// The target lives outside the directory output: removing the stale link
	// must not touch it.
	target := filepath.Join(dir, "outside.txt")
	if err := os.WriteFile(target, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmdhash := []byte("cmdhash")
	recordDirOutput(ctx, t, hfs, dir, cmdhash, "gen/out/keep.txt")
	if err := os.Symlink("../../outside.txt", filepath.Join(dir, "gen/out/link")); err != nil {
		t.Fatal(err)
	}
	if err := hfs.Symlink(ctx, dir, "../../outside.txt", "gen/out/link", time.Now(), cmdhash, nil); err != nil {
		t.Fatalf("Symlink=%v; want nil err", err)
	}
	if err := os.Symlink("../../outside.txt", filepath.Join(dir, "gen/out/stalelink")); err != nil {
		t.Fatal(err)
	}

	if err := hfs.RemoveStaleDirMembers(ctx, dir, dirOutput); err != nil {
		t.Fatalf("RemoveStaleDirMembers=%v; want nil err", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "gen/out/link")); err != nil {
		t.Errorf("link: %v; want present (recorded with the directory's cmdhash)", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "gen/out/stalelink")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stalelink: %v; want %v", err, fs.ErrNotExist)
	}
	if _, err := os.Lstat(target); err != nil {
		t.Errorf("outside.txt: %v; want present; the stale link was followed to its target", err)
	}
}

// Without a recorded directory output there is nothing authoritative to prune
// against, so the tree is left alone.
func TestRemoveStaleDirMembers_Unrecorded(t *testing.T) {
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

	writeUnrecorded(t, dir, "gen/out/a.txt")
	if err := hfs.RemoveStaleDirMembers(ctx, dir, dirOutput); err != nil {
		t.Fatalf("RemoveStaleDirMembers=%v; want nil err", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "gen/out/a.txt")); err != nil {
		t.Errorf("a.txt: %v; want present (no recorded directory output to prune against)", err)
	}
}

// A member that cannot be removed is left behind (best-effort, like
// cleandead) instead of failing the prune; other stale members still go.
func TestRemoveStaleDirMembers_BestEffort(t *testing.T) {
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

	cmdhash := []byte("cmdhash")
	recordDirOutput(ctx, t, hfs, dir, cmdhash, "gen/out/keep.txt")
	writeUnrecorded(t, dir, "gen/out/stale.txt")
	writeUnrecorded(t, dir, "gen/out/locked/stale2.txt")

	locked := filepath.Join(dir, "gen/out/locked")
	if err := os.Chmod(locked, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(locked, 0o755)
	})

	if err := hfs.RemoveStaleDirMembers(ctx, dir, dirOutput); err != nil {
		t.Fatalf("RemoveStaleDirMembers=%v; want nil err (best-effort)", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "gen/out/locked/stale2.txt")); err != nil {
		t.Skip("permissions not enforced (e.g. root); cannot exercise the failure path")
	}
	if _, err := os.Lstat(filepath.Join(dir, "gen/out/keep.txt")); err != nil {
		t.Errorf("keep.txt: %v; want present", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "gen/out/stale.txt")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stale.txt: %v; want %v", err, fs.ErrNotExist)
	}
}

// Unlike a deletion failure, a traversal failure is returned: a retained
// subdirectory that cannot be read may hide stale members, so the prune cannot
// vouch for the tree. Stale members it could reach are still removed.
func TestRemoveStaleDirMembers_UnreadableRetainedDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("cannot make a directory unreadable via permissions on Windows")
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

	cmdhash := []byte("cmdhash")
	// "a/" sorts before "sub/", so WalkDir reaches the stale member first.
	recordDirOutput(ctx, t, hfs, dir, cmdhash, "gen/out/sub/", "gen/out/sub/keep.txt")
	writeUnrecorded(t, dir, "gen/out/a/stale.txt")
	writeUnrecorded(t, dir, "gen/out/sub/stale2.txt")

	sub := filepath.Join(dir, "gen/out/sub")
	if err := os.Chmod(sub, 0o311); err != nil { // traversable, not listable
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(sub, 0o755)
	})
	if _, err := os.ReadDir(sub); err == nil {
		t.Skip("permissions not enforced (e.g. root); cannot exercise the failure path")
	}

	err = hfs.RemoveStaleDirMembers(ctx, dir, dirOutput)
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("RemoveStaleDirMembers=%v; want %v", err, fs.ErrPermission)
	}
	if _, err := os.Lstat(filepath.Join(dir, "gen/out/a")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("gen/out/a: %v; want %v; a reachable stale member was kept", err, fs.ErrNotExist)
	}
	if _, err := os.Lstat(filepath.Join(dir, "gen/out/sub/keep.txt")); err != nil {
		t.Errorf("sub/keep.txt: %v; want present", err)
	}
}
