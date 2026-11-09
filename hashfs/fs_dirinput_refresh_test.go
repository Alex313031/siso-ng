// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package hashfs

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestExpandDirInputs_InvalidatedByRefresh verifies that Refresh drops the
// memoized directory-input expansion, since Refresh re-syncs hashfs with on-disk
// truth (resetting hfs.directory and reloading state). A new file appearing on
// disk before the Refresh must show up in a fresh expansion afterwards, not be
// masked by a stale dirInputCache entry that survived the Refresh.
func TestExpandDirInputs_InvalidatedByRefresh(t *testing.T) {
	ctx := t.Context()
	root, err := filepath.EvalSymlinks(t.TempDir())
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

	if err := os.MkdirAll(filepath.Join(root, "d"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "d", "a"), []byte("A"), 0644); err != nil {
		t.Fatal(err)
	}

	// First expansion walks the tree and memoizes [d/a] in dirInputCache.
	first := hfs.expandDirInputs(ctx, root, []string{"d/"})
	if !slices.Contains(first, "d/a") || len(first) != 1 {
		t.Fatalf("first expansion = %v; want exactly [d/a]", first)
	}

	// A new file appears under d/ on disk, then Refresh re-syncs the tree.
	if err := os.WriteFile(filepath.Join(root, "d", "b"), []byte("B"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := hfs.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	second := hfs.expandDirInputs(ctx, root, []string{"d/"})
	if !slices.Contains(second, "d/b") {
		t.Errorf("second expansion after Refresh = %v; want it to contain d/b "+
			"(stale dirInputCache survived Refresh)", second)
	}
}
