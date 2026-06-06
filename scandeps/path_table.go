// Copyright 2025 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package scandeps

import (
	"fmt"
)

// PathTable manages the mapping between path strings and unique integer indices.
// It is NOT designed to be thread-safe.
type PathTable struct {
	pathToIdx map[string]uint32
	idxToPath []string
}

// NewPathTable creates and returns a new, empty PathTable.
func NewPathTable() *PathTable {
	return &PathTable{
		pathToIdx: make(map[string]uint32),
	}
}

// Index returns the unique integer index for a given path string.
// If the path is new, it assigns a new index and stores the mapping.
func (pt *PathTable) Index(path string) uint32 {
	idx, ok := pt.pathToIdx[path]
	if ok {
		return idx
	}

	idx = uint32(len(pt.idxToPath))
	pt.pathToIdx[path] = idx
	pt.idxToPath = append(pt.idxToPath, path)
	return idx
}

// Path returns the path string for a given integer index.
// It returns an error if the index is invalid.
func (pt *PathTable) Path(idx uint32) (string, error) {
	if idx >= uint32(len(pt.idxToPath)) {
		return "", fmt.Errorf("invalid path index %d", idx)
	}
	return pt.idxToPath[idx], nil
}

// Reset clears the PathTable for reuse.
func (pt *PathTable) Reset() {
	clear(pt.pathToIdx)
	pt.idxToPath = pt.idxToPath[:0]
}
