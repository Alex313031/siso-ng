// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package hashfs_test

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"go.chromium.org/build/hashigo/digest"

	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/path"
)

// TestState_RemoveAllFailureKeepsEntry verifies a failed disk removal keeps
// the generated file's hashfs entry. The file then stays in saved state and
// in PreviouslyGeneratedFiles, so the next build's clean-dead retries the
// removal instead of leaving the dead output behind forever.
func TestState_RemoveAllFailureKeepsEntry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permission bits do not block removal on windows")
	}
	if os.Getuid() == 0 {
		t.Skip("running as root; permission bits are not enforced")
	}
	ctx := t.Context()
	dir := t.TempDir()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	hfs, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}
	defer hfs.Close(ctx)
	if err := hfs.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}

	cmdhash := []byte("dummy-cmdhash")
	if err := hfs.WriteFile(ctx, dir, "gen/dead", []byte("dead"), false, time.Now(), cmdhash, nil); err != nil {
		t.Fatalf("WriteFile=%v", err)
	}
	if err := hfs.Flush(ctx, dir, []path.Path{"gen/dead"}); err != nil {
		t.Fatalf("Flush=%v", err)
	}

	// Strip write permission from the parent so unlinking gen/dead fails,
	// like a sharing violation or permission error during clean-dead.
	genDir := filepath.Join(dir, "gen")
	if err := os.Chmod(genDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(genDir, 0o755); err != nil {
			t.Error(err)
		}
	})

	if err := hfs.RemoveAll(ctx, dir, "gen/dead"); err == nil {
		t.Fatal("RemoveAll=nil; want error (test setup failed to block the removal)")
	}
	if _, err := os.Lstat(filepath.Join(dir, "gen/dead")); err != nil {
		t.Fatalf("Lstat(gen/dead)=%v; want file left on disk", err)
	}

	m := hashfs.StateMap(digest.SHA256, hfs.State(ctx))
	ent, ok := m[filepath.ToSlash(filepath.Join(dir, "gen/dead"))]
	if !ok {
		t.Fatal("gen/dead missing from state after failed RemoveAll; clean-dead will never retry removing it")
	}
	if !bytes.Equal(ent.CmdHash, cmdhash) {
		t.Errorf("gen/dead cmdhash = %q; want %q so it stays in PreviouslyGeneratedFiles", ent.CmdHash, cmdhash)
	}
}

// TestState_RemoveAllPartialFailureDropsRemovedChildren covers a RemoveAll
// that deletes some members of a directory before failing on another (e.g. a
// sibling locked by a scanner on Windows). The root entry must stay recorded
// so clean-dead retries the removal; entries for members proven gone from
// disk must be dropped, or a stale entry satisfies Stat for a missing path.
func TestState_RemoveAllPartialFailureDropsRemovedChildren(t *testing.T) {
	if runtime.GOOS != "windows" && os.Getuid() == 0 {
		t.Skip("running as root; permission bits are not enforced")
	}
	ctx := t.Context()
	dir := t.TempDir()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	hfs, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}
	defer hfs.Close(ctx)
	if err := hfs.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}

	cmdhash := []byte("dummy-cmdhash")
	if err := hfs.Mkdir(ctx, dir, "gen/dead", cmdhash, nil); err != nil {
		t.Fatalf("Mkdir=%v", err)
	}
	for _, f := range []path.Path{"gen/dead/a", "gen/dead/sub/f"} {
		if err := hfs.WriteFile(ctx, dir, f, []byte("dead"), false, time.Now(), cmdhash, nil); err != nil {
			t.Fatalf("WriteFile(%q)=%v", f, err)
		}
	}
	if err := hfs.Flush(ctx, dir, []path.Path{"gen/dead/a", "gen/dead/sub/f"}); err != nil {
		t.Fatalf("Flush=%v", err)
	}

	// Block removal of gen/dead/sub/f while gen/dead/a stays deletable.
	if runtime.GOOS == "windows" {
		// An open handle without FILE_SHARE_DELETE makes unlinking fail with a
		// sharing violation, the common cause in the wild.
		fh, err := os.Open(filepath.Join(dir, "gen/dead/sub/f"))
		if err != nil {
			t.Fatal(err)
		}
		defer fh.Close()
	} else {
		subDir := filepath.Join(dir, "gen/dead/sub")
		if err := os.Chmod(subDir, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := os.Chmod(subDir, 0o755); err != nil {
				t.Error(err)
			}
		})
	}

	if err := hfs.RemoveAll(ctx, dir, "gen/dead"); err == nil {
		t.Fatal("RemoveAll=nil; want error (test setup failed to block the removal)")
	}
	// os.RemoveAll continues past the member it cannot delete, so gen/dead/a
	// must be gone from disk while gen/dead/sub/f survived.
	if _, err := os.Lstat(filepath.Join(dir, "gen/dead/a")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Lstat(gen/dead/a)=%v; want ErrNotExist (test setup: RemoveAll did not delete it)", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "gen/dead/sub/f")); err != nil {
		t.Fatalf("Lstat(gen/dead/sub/f)=%v; want blocked member left on disk", err)
	}

	// The deleted member must not be statable from a stale entry.
	if _, err := hfs.Stat(ctx, dir, "gen/dead/a"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Stat(gen/dead/a)=%v; want ErrNotExist (deleted from disk by RemoveAll)", err)
	}

	// The root and the surviving member stay recorded so the next build's
	// clean-dead retries the removal.
	m := hashfs.StateMap(digest.SHA256, hfs.State(ctx))
	if _, ok := m[filepath.ToSlash(filepath.Join(dir, "gen/dead"))]; !ok {
		t.Error("gen/dead missing from state after failed RemoveAll; clean-dead will never retry removing it")
	}
	if _, ok := m[filepath.ToSlash(filepath.Join(dir, "gen/dead/sub/f"))]; !ok {
		t.Error("gen/dead/sub/f missing from state; it is still on disk and must stay recorded")
	}
	if _, ok := m[filepath.ToSlash(filepath.Join(dir, "gen/dead/a"))]; ok {
		t.Error("gen/dead/a still in state; its stale entry must be dropped after RemoveAll deleted it from disk")
	}
}
