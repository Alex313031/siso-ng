// Copyright 2025 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package scandeps

import (
	"testing"
)

func TestNewPathTable(t *testing.T) {
	pt := NewPathTable()
	_, err := pt.Path(0)
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
	var wantIdx1 uint32 = 0
	gotIdx1 := pt.Index(path1)
	if gotIdx1 != wantIdx1 {
		t.Errorf("Index(%q) = %d; want %d", path1, gotIdx1, wantIdx1)
	}
	gotPath1, err := pt.Path(gotIdx1)
	if err != nil {
		t.Errorf("Path(%d) returned error: %v; want nil", gotIdx1, err)
	}
	if gotPath1 != path1 {
		t.Errorf("Path(%d) = %q; want %q", gotIdx1, gotPath1, path1)
	}

	// 2. Add a second path.
	path2 := "path/to/file2.cc"
	var wantIdx2 uint32 = 1
	gotIdx2 := pt.Index(path2)
	if gotIdx2 != wantIdx2 {
		t.Errorf("Index(%q) = %d; want %d", path2, gotIdx2, wantIdx2)
	}

	// 3. Get the first path again to ensure it returns the same index.
	gotIdx1Again := pt.Index(path1)
	if gotIdx1Again != wantIdx1 {
		t.Errorf("Index(%q) again = %d; want %d", path1, gotIdx1Again, wantIdx1)
	}

	// 4. Add an empty string path.
	path3 := ""
	var wantIdx3 uint32 = 2
	gotIdx3 := pt.Index(path3)
	if gotIdx3 != wantIdx3 {
		t.Errorf("Index(%q) = %d; want %d", path3, gotIdx3, wantIdx3)
	}

	// 5. Test invalid indices.
	_, err = pt.Path(999)
	if err == nil {
		t.Errorf("Path(999) expected error, got nil")
	}
}
