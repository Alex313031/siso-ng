// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build windows

package osfs

import (
	"os"
	"path/filepath"
	"testing"
)

// These tests lock in the delete semantics siso relies on when replacing a
// stale output while an async digest read still holds it open (openRead uses
// FILE_SHARE_DELETE): on modern Windows, DeleteFile applies POSIX delete by
// default on NTFS and ReFS, so the name leaves the namespace immediately and
// can be recreated. A failure means names linger delete-pending behind open
// handles (older Windows, or a filesystem like exFAT that cannot free them).

// TestRemoveAll_FreesNameWithOpenReader: a stale file removed while a digest
// read holds it open must be recreatable immediately, as a directory-output
// action does right after ClearStaleFileForDirOutput.
func TestRemoveAll_FreesNameWithOpenReader(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "stale")
	if err := os.WriteFile(f, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	reader, err := openRead(f) // as the async digester holds the file open
	if err != nil {
		t.Fatalf("openRead: %v", err)
	}
	defer reader.Close()

	ctx := t.Context()
	ofs := New(ctx, "test", Option{})
	if err := ofs.RemoveAll(ctx, f); err != nil {
		t.Fatalf("RemoveAll with open reader: %v", err)
	}
	if err := os.Mkdir(f, 0o755); err != nil {
		t.Errorf("recreate name after RemoveAll: %v; want the name freed immediately", err)
	}
}

// TestRemoveAll_FreesDirTreeWithOpenChild: removing a stale directory tree
// whose member is held open by a digest read must free the name immediately,
// as the dir->file/dir->symlink flush transitions require.
func TestRemoveAll_FreesDirTreeWithOpenChild(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	tree := filepath.Join(dir, "stale-tree")
	if err := os.MkdirAll(tree, 0o755); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(tree, "child")
	if err := os.WriteFile(child, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	reader, err := openRead(child) // as the async digester holds a member open
	if err != nil {
		t.Fatalf("openRead: %v", err)
	}
	defer reader.Close()

	ofs := New(ctx, "test", Option{})
	if err := ofs.RemoveAll(ctx, tree); err != nil {
		t.Fatalf("RemoveAll(dir tree with open child): %v", err)
	}
	if _, err := os.Lstat(tree); !os.IsNotExist(err) {
		t.Errorf("after RemoveAll, Lstat(tree) err=%v; want not-exist", err)
	}
	if err := os.WriteFile(tree, []byte("now-a-file"), 0o644); err != nil {
		t.Errorf("recreate name after RemoveAll: %v; want the tree freed immediately", err)
	}
}
