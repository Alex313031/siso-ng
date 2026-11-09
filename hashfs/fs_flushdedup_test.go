// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package hashfs

import (
	"os"
	"path/filepath"
	"testing"

	"go.chromium.org/build/siso/path"
)

// TestExpandFlushDirs_DedupsWithoutDirectory verifies expandFlushDirs dedupes
// its input even when no directory entry is present (a duplicate path otherwise
// makes the Flush loop block on an already-drained e.lready channel). Such a
// duplicate is reachable via cmd.AllOutputs() when the depfile is also a
// declared file output and there are no directory outputs.
func TestExpandFlushDirs_DedupsWithoutDirectory(t *testing.T) {
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

	countOf := func(got []string, want string) int {
		n := 0
		for _, p := range got {
			if p == want {
				n++
			}
		}
		return n
	}

	// Duplicate path with no directory entry: every path routes through the
	// dedup machinery, so the doubled foo.o.d must collapse to one.
	t.Run("DuplicateNoDirectory", func(t *testing.T) {
		for _, name := range []string{"foo.o", "foo.o.d"} {
			if err := os.WriteFile(filepath.Join(root, name), []byte("X"), 0644); err != nil {
				t.Fatal(err)
			}
			if _, err := hfs.Stat(ctx, root, path.Path(name)); err != nil {
				t.Fatal(err)
			}
		}

		got := hfs.expandFlushDirs(ctx, root, []string{"foo.o", "foo.o.d", "foo.o.d"})
		if n := countOf(got, "foo.o.d"); n != 1 {
			t.Errorf("expandFlushDirs emitted %q %d times; want 1 (result=%v); a no-directory input must still be deduped", "foo.o.d", n, got)
		}
	})

	// Sanity: the same duplicate is also deduped when a directory artifact
	// (trailing slash) IS present and expanded to its members.
	t.Run("DuplicateWithDirectory", func(t *testing.T) {
		if err := os.MkdirAll(filepath.Join(root, "d"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "d", "x"), []byte("D"), 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := hfs.ReadDir(ctx, root, "d"); err != nil {
			t.Fatal(err)
		}

		got := hfs.expandFlushDirs(ctx, root, []string{"d/", "foo.o.d", "foo.o.d"})
		if n := countOf(got, "foo.o.d"); n != 1 {
			t.Errorf("expandFlushDirs (directory present) emitted %q %d times; want 1 (result=%v)", "foo.o.d", n, got)
		}
		if countOf(got, "d/x") != 1 {
			t.Errorf("expandFlushDirs did not expand directory artifact %q; result=%v", "d/", got)
		}
	})

	// A directory passed WITHOUT a trailing slash is a plain file output that
	// merely resolves to a directory (e.g. a legacy directory-valued "copy").
	// It must be flushed as-is, not expanded into its members, so its contents
	// are not materialized to disk under build-without-the-bytes.
	t.Run("DirectoryWithoutSlashNotExpanded", func(t *testing.T) {
		if err := os.MkdirAll(filepath.Join(root, "legacy"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "legacy", "child"), []byte("L"), 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := hfs.ReadDir(ctx, root, "legacy"); err != nil {
			t.Fatal(err)
		}

		got := hfs.expandFlushDirs(ctx, root, []string{"legacy"})
		if countOf(got, "legacy/child") != 0 {
			t.Errorf("expandFlushDirs expanded non-slash directory %q into %q; result=%v", "legacy", "legacy/child", got)
		}
		if countOf(got, "legacy") != 1 {
			t.Errorf("expandFlushDirs dropped non-slash directory %q; result=%v", "legacy", got)
		}
	})
}
