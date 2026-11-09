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

// TestRename_ReplacesFileWithOpenReader: Rename over a destination held open by
// a share-delete reader must succeed (the TestWriteDataFlush flush race).
func TestRename_ReplacesFileWithOpenReader(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()

	dst := filepath.Join(dir, "out")
	if err := os.WriteFile(dst, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(dir, ".out.siso_tmp")
	if err := os.WriteFile(tmp, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}

	reader, err := openRead(dst) // as the async digester holds the old output open
	if err != nil {
		t.Fatalf("openRead: %v", err)
	}
	defer reader.Close()

	ofs := New(ctx, "test", Option{})
	if err := ofs.Rename(ctx, tmp, dst); err != nil {
		t.Fatalf("Rename over open destination: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("ReadFile(dst): %v", err)
	}
	if string(got) != "new" {
		t.Errorf("dst content = %q; want %q", got, "new")
	}
	if _, err := os.Lstat(tmp); !os.IsNotExist(err) {
		t.Errorf("after Rename, Lstat(tmp) err=%v; want not-exist", err)
	}
}
