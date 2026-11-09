// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package hashfs

import (
	"context"
	"io/fs"
	"testing"

	"go.chromium.org/build/hashigo/digest"

	"go.chromium.org/build/siso/blob"
	"go.chromium.org/build/siso/path"
	"go.chromium.org/build/siso/reapi/merkletree"
)

// TestShouldKeep_DiskMissDoesNotEvictBuildWithoutBytes checks that a
// local-disk miss does not evict a recorded not-local output (directory and
// its members, plain file, symlink), while an explicit removal still does.
func TestShouldKeep_DiskMissDoesNotEvictBuildWithoutBytes(t *testing.T) {
	const dir = "out/siso/gen"
	const member = "out/siso/gen/inner.txt"
	const file = "out/siso/gen/out.o"
	const link = "out/siso/gen/out.link"
	cmdhash := []byte("dircmdhash")
	action := digest.Digest{Hash: "actionhash", SizeBytes: 10}

	// record stores a not-local dir output plus one member, a plain file and a
	// symlink, like RecordOutputs of remote outputs with output_local=false.
	record := func(ctx context.Context, t *testing.T) (*HashFS, string) {
		t.Helper()
		root := t.TempDir()
		hfs, err := New(ctx, Option{})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		t.Cleanup(func() {
			if err := hfs.Close(ctx); err != nil {
				t.Errorf("hfs.Close=%v; want nil", err)
			}
		})
		if err := hfs.Update(ctx, root, []UpdateEntry{
			{Name: dir, Entry: &merkletree.Entry{Name: dir}, Mode: 0o755 | fs.ModeDir, CmdHash: cmdhash, Action: action},
			{Name: member, Entry: &merkletree.Entry{Name: member, Data: blob.NewData(nil, digest.Digest{Hash: "innerhash", SizeBytes: 3})}, Mode: 0o644, CmdHash: cmdhash, Action: action},
			{Name: file, Entry: &merkletree.Entry{Name: file, Data: blob.NewData(nil, digest.Digest{Hash: "filehash", SizeBytes: 4})}, Mode: 0o644, CmdHash: cmdhash, Action: action},
			{Name: link, Entry: &merkletree.Entry{Name: link, Target: "out.o"}, Mode: 0o644 | fs.ModeSymlink, CmdHash: cmdhash, Action: action},
		}); err != nil {
			t.Fatalf("Update: %v", err)
		}
		return hfs, root
	}

	// negEntry builds a negative (ErrNotExist) entry. diskMiss marks a
	// local-disk probe; an explicit Remove/RemoveAll leaves it false.
	negEntry := func(diskMiss bool) *entry {
		e := newLocalEntry()
		e.err = fs.ErrNotExist
		e.diskMiss = diskMiss
		return e
	}

	present := func(ctx context.Context, t *testing.T, hfs *HashFS, root string, name path.Path) bool {
		t.Helper()
		_, err := hfs.Stat(ctx, root, name)
		return err == nil
	}

	presentDir := func(ctx context.Context, t *testing.T, hfs *HashFS, root string, name path.Path) bool {
		t.Helper()
		fi, err := hfs.Stat(ctx, root, name)
		return err == nil && fi.IsDir()
	}

	t.Run("disk_miss_keeps_dir_and_member", func(t *testing.T) {
		ctx := t.Context()
		hfs, root := record(ctx, t)
		if !presentDir(ctx, t, hfs, root, dir) {
			t.Fatalf("dir %q not recorded; want present before the disk probe", dir)
		}
		if !present(ctx, t, hfs, root, member) {
			t.Fatalf("member %q not recorded; want present before the disk probe", member)
		}
		if _, err := hfs.directory.store(ctx, makeFullpath(root, dir), negEntry(true)); err != nil {
			t.Fatalf("store negative: %v", err)
		}
		if !presentDir(ctx, t, hfs, root, dir) {
			t.Errorf("Stat(%q) missing or not a dir; want present dir (disk miss must not evict a build-without-bytes dir)", dir)
		}
		if !present(ctx, t, hfs, root, member) {
			t.Errorf("member %q evicted with the dir; want preserved", member)
		}
	})

	t.Run("disk_miss_keeps_file", func(t *testing.T) {
		ctx := t.Context()
		hfs, root := record(ctx, t)
		if !present(ctx, t, hfs, root, file) {
			t.Fatalf("file %q not recorded; want present before the disk probe", file)
		}
		if _, err := hfs.directory.store(ctx, makeFullpath(root, file), negEntry(true)); err != nil {
			t.Fatalf("store negative: %v", err)
		}
		if !present(ctx, t, hfs, root, file) {
			t.Errorf("Stat(%q) missing; want present (disk miss must not evict a build-without-bytes file)", file)
		}
	})

	t.Run("disk_miss_keeps_symlink", func(t *testing.T) {
		ctx := t.Context()
		hfs, root := record(ctx, t)
		if !present(ctx, t, hfs, root, link) {
			t.Fatalf("symlink %q not recorded; want present before the disk probe", link)
		}
		if _, err := hfs.directory.store(ctx, makeFullpath(root, link), negEntry(true)); err != nil {
			t.Fatalf("store negative: %v", err)
		}
		if !present(ctx, t, hfs, root, link) {
			t.Errorf("Stat(%q) missing; want present (disk miss must not evict a build-without-bytes symlink)", link)
		}
	})

	t.Run("explicit_removal_evicts", func(t *testing.T) {
		ctx := t.Context()
		hfs, root := record(ctx, t)
		if !presentDir(ctx, t, hfs, root, dir) {
			t.Fatalf("dir %q not recorded; want present before the removal", dir)
		}
		if _, err := hfs.directory.store(ctx, makeFullpath(root, dir), negEntry(false)); err != nil {
			t.Fatalf("store negative: %v", err)
		}
		if _, err := hfs.Stat(ctx, root, dir); err == nil {
			t.Errorf("Stat(%q)=nil err; want eviction for an explicit (non-diskMiss) removal", dir)
		}
	})
}
