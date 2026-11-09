// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package hashfs

import (
	"context"
	"math"
	"path/filepath"
	"sort"
	"time"

	pb "go.chromium.org/build/siso/hashfs/proto"
)

const (
	maxDynamicBuildLabels        = 63
	tombstoneBitID        uint32 = maxDynamicBuildLabels
	tombstoneMask         uint64 = uint64(1) << tombstoneBitID
)

// UpdateBuildLabelMask applies the bitmask for the given build label to the provided files.
// It manages dynamic build label bit allocations up to 63 labels (0..62), utilizing an LRU
// eviction strategy if the mask space overflows and reserving bit 63 as a tombstone sentinel.
func (hfs *HashFS) UpdateBuildLabelMask(ctx context.Context, execRoot, buildLabel string, files []string) {
	if buildLabel == "" || len(files) == 0 {
		return
	}

	hfs.ledgerMu.Lock()
	defer hfs.ledgerMu.Unlock()

	bitID := hfs.allocateBitIDLocked(buildLabel)
	mask := uint64(1) << bitID

	for _, f := range files {
		if !filepath.IsAbs(f) {
			f = filepath.Join(execRoot, f)
		}
		f = filepath.ToSlash(f)
		// Siso paths in .siso_fs_state are stored natively matching e.Name.
		// Strip tombstoneMask if the file was previously tombstoned and is now rebuilt.
		hfs.fileBuildLabels[f] = (hfs.fileBuildLabels[f] &^ tombstoneMask) | mask
	}
	hfs.clean.Store(false)
}

// allocateBitIDLocked assigns a unique 0..62 bit ID for the buildLabel.
// When dynamic capacity (63 items) is reached, it evicts the least-recently used label.
// It must be called with ledgerMu held.
func (hfs *HashFS) allocateBitIDLocked(buildLabel string) uint32 {
	var oldestID uint32
	var oldestTime int64 = math.MaxInt64
	var dynamicCount int

	// Check if build label already exists and find the oldest label for potential eviction.
	for id, metadata := range hfs.buildLabelDictionary {
		if id >= maxDynamicBuildLabels || metadata == nil {
			continue
		}
		dynamicCount++
		if metadata.BuildLabel == buildLabel {
			metadata.LastBuildTimestamp = time.Now().UnixNano()
			return id
		}
		if metadata.LastBuildTimestamp < oldestTime || (metadata.LastBuildTimestamp == oldestTime && id > oldestID) {
			oldestTime = metadata.LastBuildTimestamp
			oldestID = id
		}
	}

	// Try to allocate a new bit ID if there is room within maxDynamicBuildLabels (0..62).
	if dynamicCount < maxDynamicBuildLabels {
		for newID := range uint32(maxDynamicBuildLabels) {
			if _, ok := hfs.buildLabelDictionary[newID]; !ok {
				hfs.buildLabelDictionary[newID] = &pb.BuildLabelMetadata{
					BuildLabel:         buildLabel,
					LastBuildTimestamp: time.Now().UnixNano(),
				}
				return newID
			}
		}
	}

	// We are at dynamic capacity (63 items). Evict the oldest.
	evictMask := uint64(1) << oldestID

	// Scrub the evicted bit from all files.
	// If a file was exclusively owned by oldestID (newMask == 0), tag it with tombstoneMask
	// so that it is preserved in the ledger as an eviction candidate for out-of-band GC.
	// If it was shared with other active labels, strip oldestID.
	for f, m := range hfs.fileBuildLabels {
		if m&evictMask == 0 {
			continue
		}
		newMask := m &^ evictMask
		if newMask == 0 {
			newMask = tombstoneMask
		}
		hfs.fileBuildLabels[f] = newMask
	}

	// Re-purpose the oldest ID for the new buildLabel.
	hfs.buildLabelDictionary[oldestID] = &pb.BuildLabelMetadata{
		BuildLabel:         buildLabel,
		LastBuildTimestamp: time.Now().UnixNano(),
	}

	return oldestID
}

// ActiveBuildLabels returns a sorted list of all active build labels currently tracked in the state ledger.
// It ignores the tombstone sentinel bit.
func (hfs *HashFS) ActiveBuildLabels() []string {
	hfs.ledgerMu.RLock()
	defer hfs.ledgerMu.RUnlock()

	labels := make([]string, 0, len(hfs.buildLabelDictionary))
	for id, meta := range hfs.buildLabelDictionary {
		if id < maxDynamicBuildLabels && meta != nil && meta.BuildLabel != "" {
			labels = append(labels, meta.BuildLabel)
		}
	}

	sort.Strings(labels)
	return labels
}
