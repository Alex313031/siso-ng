// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package hashfs

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"go.chromium.org/build/hashigo/digest"

	"go.chromium.org/build/siso/blob"
	"go.chromium.org/build/siso/hashfs/osfs"
	"go.chromium.org/build/siso/path"
)

// TestStatMtimeDeferDigestGate: StatMtime skips the digest queue only in
// non-defer mode (where reload repairs missing digests). In -fs_defer_digest
// mode it must queue the digest, or the digestless input lands in
// missing_digests, keeps IsClean false, and disables fast-nop.
func TestStatMtimeDeferDigestGate(t *testing.T) {
	ctx := t.Context()
	check := func(t *testing.T, deferDigest, wantDigest bool) {
		dir := t.TempDir()
		hfs, err := New(ctx, Option{DeferDigest: deferDigest})
		if err != nil {
			t.Fatal(err)
		}
		fname := filepath.Join(dir, "gen.h")
		const body = "header contents"
		if err := os.WriteFile(fname, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
		past := time.Now().Add(-2 * time.Second)
		if err := os.Chtimes(fname, past, past); err != nil {
			t.Fatal(err)
		}
		if _, err := hfs.StatMtime(ctx, dir, "gen.h"); err != nil {
			t.Fatal(err)
		}
		// hashfs keys its directory tree by forward-slash paths (filepath.ToSlash),
		// so the lookup key must match on Windows where filepath.Join yields backslashes.
		e, _, _, ok := hfs.directory.lookup(ctx, path.Path(filepath.ToSlash(fname)))
		if !ok {
			t.Fatal("entry missing after StatMtime")
		}
		want := blob.FromBytes(digest.SHA256, fname, []byte(body)).Digest()
		if !wantDigest {
			// Close drains pending digests in non-defer mode, so a still-zero
			// digest after it proves StatMtime skipped the queue (not "not yet").
			if err := hfs.Close(ctx); err != nil {
				t.Fatal(err)
			}
			if got := e.digest(); !got.IsZero() {
				t.Fatalf("non-defer StatMtime queued a digest %v; want digestless", got)
			}
			return
		}
		deadline := time.Now().Add(2 * time.Second)
		for e.digest() != want {
			if time.Now().After(deadline) {
				t.Fatalf("defer StatMtime left the input digestless; got %v want %v", e.digest(), want)
			}
			runtime.Gosched()
		}
	}
	t.Run("non-defer-skips", func(t *testing.T) { check(t, false, false) })
	t.Run("defer-queues", func(t *testing.T) { check(t, true, true) })
}

// TestReadFileStaleSizeDigest: when an entry's size is stale (the file grew),
// ReadFull fills the stale-size buffer and returns without checking EOF, so the
// inline digest covers only the prefix and suppresses the full-file digest.
// e.d must not be set from a partial read.
func TestReadFileStaleSizeDigest(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	hfs, err := New(ctx, Option{})
	if err != nil {
		t.Fatal(err)
	}
	fname := filepath.Join(dir, "gen.h")
	const prefix = "old small content"
	const grown = prefix + " plus appended bytes the stale entry never saw"
	if err := os.WriteFile(fname, []byte(prefix), 0644); err != nil {
		t.Fatal(err)
	}
	// Avoid init's waitUntilModTime sleeping on a just-written file.
	past := time.Now().Add(-2 * time.Second)
	if err := os.Chtimes(fname, past, past); err != nil {
		t.Fatal(err)
	}
	// A digestless entry capturing the old (small) size.
	e := newLocalEntry()
	e.init(ctx, fname, hfs.executables, hfs.OS)
	if _, err := hfs.directory.store(ctx, path.Path(fname), e); err != nil {
		t.Fatal(err)
	}
	// File grows on disk without a hashfs update.
	if err := os.WriteFile(fname, []byte(grown), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := hfs.ReadFile(ctx, dir, "gen.h"); err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	e2, _, _, ok := hfs.directory.lookup(ctx, path.Path(fname))
	if !ok {
		t.Fatal("entry vanished after ReadFile")
	}
	// ReadFile must never persist the stale-size prefix digest (it would
	// suppress lazyCompute and leave a wrong digest). Valid: zero (deferred)
	// or the full-file digest; the prefix digest is the bug.
	got := e2.digest()
	prefixDigest := blob.FromBytes(digest.SHA256, fname, []byte(prefix)).Digest()
	fullDigest := blob.FromBytes(digest.SHA256, fname, []byte(grown)).Digest()
	if !got.IsZero() && got != fullDigest {
		t.Fatalf("ReadFile stored digest %v; want zero (deferred) or full %v, not the stale prefix %v", got, fullDigest, prefixDigest)
	}
}

func TestDirectoryLookup_Symlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no symlink on windows")
		return
	}
	ctx := t.Context()
	dir := t.TempDir()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}

	setupFile := func(fname string) {
		t.Helper()
		fname = filepath.Join(dir, fname)
		err := os.MkdirAll(filepath.Dir(fname), 0755)
		if err != nil {
			t.Fatal(err)
		}
		err = os.WriteFile(fname, nil, 0644)
		if err != nil {
			t.Fatal(err)
		}
	}
	setupSymlink := func(fname, target string) {
		t.Helper()
		fname = filepath.Join(dir, fname)
		err := os.MkdirAll(filepath.Dir(fname), 0755)
		if err != nil {
			t.Fatal(err)
		}
		err = os.Symlink(target, fname)
		if err != nil {
			t.Fatal(err)
		}
	}

	fileName := "build/mac_files/xcode_binaries/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX.sdk/somefile"
	setupFile(fileName)
	symlinkName := filepath.Join(filepath.Dir(filepath.Dir(fileName)), "MacOSX13.3.sdk")
	setupSymlink(symlinkName, "MacOSX.sdk")

	d := &directory{isRoot: true}
	osfs := osfs.New(ctx, "fs", osfs.Option{})

	fname := filepath.Join(dir, symlinkName)
	_, _, _, ok := d.lookup(ctx, path.New(fname))
	if ok {
		t.Fatalf("d.lookup(ctx, %q): %t; want false", fname, ok)
	}
	e := newLocalEntry()
	e.init(ctx, fname, nil, osfs)
	_, err = d.store(ctx, path.New(fname), e)
	if err != nil {
		t.Fatalf("d.store(ctx, %q) %v; want nil err", fname, err)
	}

	fname = filepath.Join(dir, fileName)
	_, _, _, ok = d.lookup(ctx, path.New(fname))
	if ok {
		t.Fatalf("d.lookup(ctx, %q): %t; want false", fname, ok)
	}
	e = newLocalEntry()
	e.init(ctx, fname, nil, osfs)
	_, err = d.store(ctx, path.New(fname), e)
	if err != nil {
		t.Fatalf("d.store(ctx, %q) %v; want nil err", fname, err)
	}

	t.Log(fname)
	_, _, _, ok = d.lookup(ctx, path.New(fname))
	if !ok {
		t.Fatalf("d.lookup(ctx, %q) %t; want true", fname, ok)
	}
	fname = filepath.Dir(fname)
	t.Log(fname)
	_, _, _, ok = d.lookup(ctx, path.New(fname))
	if !ok {
		t.Fatalf("d.lookup(ctx, %q) %t; want true", fname, ok)
	}

	fname = filepath.Join(dir, symlinkName)
	t.Log(fname)
	_, _, _, ok = d.lookup(ctx, path.New(fname))
	if !ok {
		t.Fatalf("d.lookup(ctx, %q) %t; want true", fname, ok)
	}
	fname = filepath.Join(fname, "somefile")
	t.Log(fname)
	_, _, _, ok = d.lookup(ctx, path.New(fname))
	if !ok {
		t.Fatalf("d.lookup(ctx, %q) %t; want true", fname, ok)
	}
}

// TestExpandFlushDirs_DedupesNestedDirs verifies expandFlushDirs does not emit a
// path twice for overlapping directory artifacts (a directory and a nested one
// inside it); a duplicate makes the Flush loop block on a drained e.lready channel.
func TestExpandFlushDirs_DedupesNestedDirs(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	hfs, err := New(ctx, Option{})
	if err != nil {
		t.Fatal(err)
	}
	defer hfs.Close(ctx)
	if err := hfs.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(root, "d", "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "d", "top.txt"), []byte("T"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "d", "sub", "a.txt"), []byte("A"), 0644); err != nil {
		t.Fatal(err)
	}
	// Populate the in-memory hashfs tree so d and d/sub are directories.
	if _, err := hfs.ReadDir(ctx, root, "d"); err != nil {
		t.Fatal(err)
	}
	if _, err := hfs.ReadDir(ctx, root, "d/sub"); err != nil {
		t.Fatal(err)
	}

	got := hfs.expandFlushDirs(ctx, root, []string{"d/", "d/sub/"})
	counts := make(map[string]int)
	for _, p := range got {
		counts[p]++
	}
	for p, n := range counts {
		if n > 1 {
			t.Errorf("expandFlushDirs emitted %q %d times; want 1 (result=%v)", p, n, got)
		}
	}
	if counts["d/sub/a.txt"] == 0 {
		t.Errorf("expandFlushDirs dropped d/sub/a.txt: %v", got)
	}
}

func TestClearStaleFileForDirOutput(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	hfs, err := New(ctx, Option{})
	if err != nil {
		t.Fatal(err)
	}
	defer hfs.Close(ctx)
	if err := hfs.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}

	// "d" is a directory, "f" a regular file; stat them to populate hashfs.
	if err := os.MkdirAll(filepath.Join(root, "d", "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "d", "x"), []byte("X"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "f"), []byte("F"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := hfs.Stat(ctx, root, "d"); err != nil {
		t.Fatal(err)
	}
	if _, err := hfs.Stat(ctx, root, "f"); err != nil {
		t.Fatal(err)
	}

	exists := func(name string) bool {
		_, err := os.Stat(filepath.Join(root, name))
		return err == nil
	}

	// A directory recorded under a directory-output target is left alone: it
	// may be a legitimate directory output, not a kind flip.
	if err := hfs.ClearStaleFileForDirOutput(ctx, root, "d"); err != nil {
		t.Fatalf("ClearStaleFileForDirOutput(d): %v", err)
	}
	if !exists("d") {
		t.Errorf("directory wrongly removed for a directory-output target")
	}

	// Missing entry: no-op, no error.
	if err := hfs.ClearStaleFileForDirOutput(ctx, root, "nonexistent"); err != nil {
		t.Errorf("ClearStaleFileForDirOutput(nonexistent): %v; want nil", err)
	}

	// A file recorded under a directory-output target is a genuine
	// file->directory flip: the stale file must be removed.
	if err := hfs.ClearStaleFileForDirOutput(ctx, root, "f"); err != nil {
		t.Fatalf("ClearStaleFileForDirOutput(f): %v", err)
	}
	if exists("f") {
		t.Errorf("stale file not removed when target flipped to a directory output")
	}

	// A stale file present on disk but with no in-memory hashfs entry
	// (state reset/version bump, or a file produced outside siso) must
	// still be cleared by consulting disk; otherwise ensureActionOutputDirs'
	// MkdirAll fails ENOTDIR.
	if err := os.WriteFile(filepath.Join(root, "g"), []byte("G"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := hfs.ClearStaleFileForDirOutput(ctx, root, "g"); err != nil {
		t.Fatalf("ClearStaleFileForDirOutput(g, foreign on-disk file): %v", err)
	}
	if exists("g") {
		t.Errorf("stale on-disk file with no hashfs entry not removed; MkdirAll would fail ENOTDIR")
	}
}

// TestDirectoryLookupSymlink_DrivePath pins symlink resolution through a
// drive-absolute path: "C:" must stay the first path element (no "/"
// sentinel), or the Windows resolve-path join is malformed.
func TestDirectoryLookupSymlink_DrivePath(t *testing.T) {
	ctx := t.Context()
	d := &directory{isRoot: true}

	file := newLocalEntry()
	file.mode = 0644
	if _, err := d.store(ctx, path.Path("C:/a/b/f"), file); err != nil {
		t.Fatalf("store file: %v", err)
	}
	link := newLocalEntry()
	link.mode = 0644 | fs.ModeSymlink
	link.target = "b"
	if _, err := d.store(ctx, path.Path("C:/a/link"), link); err != nil {
		t.Fatalf("store link: %v", err)
	}
	got, _, _, ok := d.lookup(ctx, path.Path("C:/a/link/f"))
	if !ok {
		t.Fatalf("lookup(C:/a/link/f) failed; want resolution through the symlink")
	}
	if got != file {
		t.Fatalf("lookup(C:/a/link/f) = %v, want the stored file entry", got)
	}
}

func TestExpandDirInputs_EmptyDir(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	hfs, err := New(ctx, Option{})
	if err != nil {
		t.Fatal(err)
	}
	defer hfs.Close(ctx)
	if err := hfs.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(root, "empty"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "full"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "full", "a.txt"), []byte("A"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := hfs.ReadDir(ctx, root, "empty"); err != nil {
		t.Fatal(err)
	}
	if _, err := hfs.ReadDir(ctx, root, "full"); err != nil {
		t.Fatal(err)
	}

	got := hfs.expandDirInputs(ctx, root, []string{"empty/", "full/"})
	has := func(p string) bool {
		return slices.Contains(got, p)
	}
	// An empty directory input must not vanish: keep the bare dir so the
	// (empty) directory is still represented in the input tree.
	if !has("empty") {
		t.Errorf("expandDirInputs dropped empty directory input: got %v; want it to contain %q", got, "empty")
	}
	// A non-empty directory input still expands to its files.
	if !has("full/a.txt") {
		t.Errorf("expandDirInputs did not expand non-empty dir: got %v", got)
	}
}

// TestEscapingSymlinkNameKeyForm locks the key form for the names computed
// in buildMerkletreeEntries and resolveEscapingSymlink. Both sites join the
// OS-native workspace root with a workspace-relative Path and use the result
// as an hfs.directory key, which is always forward-slash: the join must
// normalize (path.JoinRoot), not concatenate with the OS separator, or on
// Windows the key keeps backslashes and silently stops matching the
// directory. The escape sites must also produce the same key form as every
// other makeFullpath call site so stores and lookups agree.
func TestEscapingSymlinkNameKeyForm(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ws") // OS-native: backslashes on Windows.
	for _, fname := range []path.Path{
		"out/gen/foo.h",
		"a/b/c",
		"./out/foo", // leading dot segment
		"out//dup",  // duplicate slash, as untrusted include directives produce
	} {
		name := path.JoinRoot(root, fname) // the form the escape sites use.
		if strings.ContainsRune(string(name), '\\') {
			t.Errorf("JoinRoot(%q, %q) = %q contains a backslash; violates the Path forward-slash invariant", root, fname, name)
		}
		if got := makeFullpath(root, fname); got != name {
			t.Errorf("makeFullpath(%q, %q) = %q; JoinRoot = %q; want equal", root, fname, got, name)
		}
	}
}
