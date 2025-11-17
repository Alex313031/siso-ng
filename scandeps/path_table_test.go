// Copyright 2025 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package scandeps

import (
	"testing"
)

func TestNewPathTable(t *testing.T) {
	pt := NewPathTable()
	_, err := pt.GetPath(0)
	if err == nil {
		t.Fatalf("Expected error for index 0 on a new table, but got none")
	}
	if len(pt.pathToIdx) != 0 {
		t.Errorf("Expected pathToIdx to be empty, got %d entries", len(pt.pathToIdx))
	}
	if len(pt.idxToPath) != 0 {
		t.Errorf("Expected idxToPath to be empty, got %d", len(pt.idxToPath))
	}
}

func TestGetIndexAndGetPath(t *testing.T) {
	pt := NewPathTable()

	// 1. Add a new path and verify its index and retrieval.
	path1 := "path/to/file1.h"
	wantIdx1 := 0
	gotIdx1 := pt.GetIndex(path1)
	if gotIdx1 != wantIdx1 {
		t.Errorf("GetIndex(%q) = %d; want %d", path1, gotIdx1, wantIdx1)
	}
	gotPath1, err := pt.GetPath(gotIdx1)
	if err != nil {
		t.Errorf("GetPath(%d) returned error: %v; want nil", gotIdx1, err)
	}
	if gotPath1 != path1 {
		t.Errorf("GetPath(%d) = %q; want %q", gotIdx1, gotPath1, path1)
	}

	// 2. Add a second path.
	path2 := "path/to/file2.cc"
	wantIdx2 := 1
	gotIdx2 := pt.GetIndex(path2)
	if gotIdx2 != wantIdx2 {
		t.Errorf("GetIndex(%q) = %d; want %d", path2, gotIdx2, wantIdx2)
	}

	// 3. Get the first path again to ensure it returns the same index.
	gotIdx1Again := pt.GetIndex(path1)
	if gotIdx1Again != wantIdx1 {
		t.Errorf("GetIndex(%q) again = %d; want %d", path1, gotIdx1Again, wantIdx1)
	}

	// 4. Add an empty string path.
	path3 := ""
	wantIdx3 := 2
	gotIdx3 := pt.GetIndex(path3)
	if gotIdx3 != wantIdx3 {
		t.Errorf("GetIndex(%q) = %d; want %d", path3, gotIdx3, wantIdx3)
	}

	// 5. Test invalid indices.
	_, err = pt.GetPath(999)
	if err == nil {
		t.Errorf("GetPath(999) expected error, got nil")
	}
	_, err = pt.GetPath(-1)
	if err == nil {
		t.Errorf("GetPath(-1) expected error, got nil")
	}
}
