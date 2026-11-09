// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package hashfs

import (
	"fmt"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	pb "go.chromium.org/build/siso/hashfs/proto"
)

func TestHashFS_UpdateBuildLabelMask_LRU_Eviction(t *testing.T) {
	ctx := t.Context()

	// Create a new HashFS instance. We don't need a real working directory for this test.
	opt := Option{
		BuildLabel:    "test-label",
		StateFile:     filepath.Join(t.TempDir(), "test_hashfs.state"),
		CompressLevel: 0,
	}
	hfs, err := New(ctx, opt)
	if err != nil {
		t.Fatalf("Failed to create HashFS: %v", err)
	}
	defer hfs.Close(ctx)

	// Helper to directly peek at internal state for testing
	getBitID := func(label string) (uint32, bool) {
		hfs.ledgerMu.RLock()
		defer hfs.ledgerMu.RUnlock()
		for id, meta := range hfs.buildLabelDictionary {
			if meta.BuildLabel == label {
				return id, true
			}
		}
		return 0, false
	}

	getFileMask := func(fname string) uint64 {
		hfs.ledgerMu.RLock()
		defer hfs.ledgerMu.RUnlock()
		return hfs.fileBuildLabels[fname]
	}

	// 1. Fill the dictionary to its maximum dynamic capacity (63 items)
	for i := 1; i <= 63; i++ {
		labelName := fmt.Sprintf("label-%d", i)
		hfs.UpdateBuildLabelMask(ctx, "", labelName, []string{"shared_file.txt", fmt.Sprintf("file-%d.txt", i)})
		// Add a tiny sleep to ensure LastBuildTimestamp strictly orders the labels for LRU.
		time.Sleep(1 * time.Millisecond)
	}

	// Verify capacity
	hfs.ledgerMu.RLock()
	if len(hfs.buildLabelDictionary) != 63 {
		t.Fatalf("Got %d items in build label dictionary, want 63", len(hfs.buildLabelDictionary))
	}
	hfs.ledgerMu.RUnlock()

	// Verify "label-1" is in the dictionary (it should be the oldest)
	oldestID, ok := getBitID("label-1")
	if !ok {
		t.Fatalf("Expected label-1 to exist before eviction")
	}

	// Check that shared_file.txt has bits from all 63 dynamic labels (bits 0..62)
	mask := getFileMask("shared_file.txt")
	wantMask := (uint64(1) << maxDynamicBuildLabels) - 1
	if mask != wantMask {
		t.Fatalf("Got shared_file.txt mask %x, want %x", mask, wantMask)
	}

	// 2. Add the 64th label, which should trigger eviction of "label-1"
	hfs.UpdateBuildLabelMask(ctx, "", "label-64", []string{"shared_file.txt", "file-64.txt"})

	// Verify "label-1" is evicted
	_, ok = getBitID("label-1")
	if ok {
		t.Errorf("Expected label-1 to be evicted (LRU)")
	}

	// Verify "label-64" got assigned the oldestID
	newID, ok := getBitID("label-64")
	if !ok {
		t.Fatalf("Expected label-64 to exist after eviction")
	}
	if newID != oldestID {
		t.Errorf("Got label-64 ID %d, want oldest ID %d", newID, oldestID)
	}

	// 3. Verify fileLabels bit scrubbing and tombstone tagging
	// "file-1.txt" was exclusively owned by the evicted label-1. It should now be
	// tagged with tombstoneMask (bit 63) and preserved in fileBuildLabels.
	gotFile1Mask := getFileMask("file-1.txt")
	if gotFile1Mask != tombstoneMask {
		t.Errorf("Got file-1.txt mask %x; want tombstoneMask %x", gotFile1Mask, tombstoneMask)
	}

	// "shared_file.txt" had the oldest bit cleared and then re-added by label-64.
	// It should retain all 63 dynamic bits.
	if getFileMask("shared_file.txt") != wantMask {
		t.Errorf("Got shared_file.txt mask %x, want %x", getFileMask("shared_file.txt"), wantMask)
	}

	// "file-2.txt" should still have its original bit.
	id2, ok := getBitID("label-2")
	if !ok {
		t.Fatalf("Expected label-2 to exist in build label dictionary")
	}
	if getFileMask("file-2.txt") != (1 << id2) {
		t.Errorf("Expected file-2.txt to retain its bitmask")
	}
}

func TestHashFS_UpdateBuildLabelMask_Basic(t *testing.T) {
	ctx := t.Context()
	hfs, err := New(ctx, Option{})
	if err != nil {
		t.Fatalf("Failed to create HashFS: %v", err)
	}
	defer hfs.Close(ctx)

	execRoot := "/root"
	if runtime.GOOS == "windows" {
		execRoot = "C:/root"
	}

	// 1. Try empty target and empty files
	hfs.UpdateBuildLabelMask(ctx, execRoot, "", []string{"file.txt"}) // Empty target
	hfs.UpdateBuildLabelMask(ctx, execRoot, "label-x", []string{})    // Empty files
	hfs.ledgerMu.RLock()
	if len(hfs.buildLabelDictionary) != 0 || len(hfs.fileBuildLabels) != 0 {
		t.Errorf("Got labels: %v, files: %v for empty inputs, want empty maps", hfs.buildLabelDictionary, hfs.fileBuildLabels)
	}
	hfs.ledgerMu.RUnlock()

	// 2. Test basic insertion and path normalization
	hfs.UpdateBuildLabelMask(ctx, execRoot, "label-a", []string{"file1.txt"})

	var idA uint32
	hfs.ledgerMu.RLock()
	for id, meta := range hfs.buildLabelDictionary {
		if meta.BuildLabel == "label-a" {
			idA = id
		}
	}
	hfs.ledgerMu.RUnlock()
	if idA == 0 && len(hfs.buildLabelDictionary) == 0 {
		t.Fatalf("label-a was not registered in dictionary")
	}

	// Check path key normalization
	expectedKey := "/root/file1.txt"
	if runtime.GOOS == "windows" {
		expectedKey = "C:/root/file1.txt"
	}

	hfs.ledgerMu.RLock()
	mask, exists := hfs.fileBuildLabels[expectedKey]
	hfs.ledgerMu.RUnlock()
	if !exists {
		t.Errorf("Expected key %q to exist in fileLabels", expectedKey)
	}
	if mask != (1 << idA) {
		t.Errorf("Got mask %b, want %b", mask, 1<<idA)
	}

	// 3. Test label reuse (updates timestamp, reuses same ID)
	hfs.UpdateBuildLabelMask(ctx, execRoot, "label-a", []string{"file2.txt"})
	hfs.ledgerMu.RLock()
	dictionaryLen := len(hfs.buildLabelDictionary)
	hfs.ledgerMu.RUnlock()
	if dictionaryLen != 1 {
		t.Errorf("Got %d labels in dictionary, want 1", dictionaryLen)
	}

	// 4. Test bitwise OR overlap on shared files
	hfs.UpdateBuildLabelMask(ctx, execRoot, "label-b", []string{"file2.txt"})

	var idB uint32
	hfs.ledgerMu.RLock()
	for id, meta := range hfs.buildLabelDictionary {
		if meta.BuildLabel == "label-b" {
			idB = id
		}
	}
	hfs.ledgerMu.RUnlock()

	expectedOverlapKey := "/root/file2.txt"
	if runtime.GOOS == "windows" {
		expectedOverlapKey = "C:/root/file2.txt"
	}

	hfs.ledgerMu.RLock()
	overlapMask := hfs.fileBuildLabels[expectedOverlapKey]
	hfs.ledgerMu.RUnlock()

	expectedMask := (uint64(1) << idA) | (uint64(1) << idB)
	if overlapMask != expectedMask {
		t.Errorf("Got overlap mask %b, want %b", overlapMask, expectedMask)
	}
}

func TestHashFS_UpdateBuildLabelMask_LRU_Ordering(t *testing.T) {
	ctx := t.Context()
	hfs, err := New(ctx, Option{})
	if err != nil {
		t.Fatalf("Failed to create HashFS: %v", err)
	}
	defer hfs.Close(ctx)

	checkLabelExists := func(label string) bool {
		hfs.ledgerMu.RLock()
		defer hfs.ledgerMu.RUnlock()
		for _, meta := range hfs.buildLabelDictionary {
			if meta.BuildLabel == label {
				return true
			}
		}
		return false
	}

	// 1. Fill the dictionary to capacity (63 labels)
	for i := 1; i <= 63; i++ {
		labelName := fmt.Sprintf("label-%d", i)
		hfs.UpdateBuildLabelMask(ctx, "", labelName, []string{"shared.txt"})
		time.Sleep(1 * time.Millisecond) // strictly order timestamps
	}

	// label-1 is now the oldest.
	// 2. Re-access label-1 to update its timestamp, making it the newest
	hfs.UpdateBuildLabelMask(ctx, "", "label-1", []string{"shared.txt"})
	time.Sleep(1 * time.Millisecond)

	// label-2 should now be the oldest
	// 3. Add label-64, which should evict label-2 (since label-1 was touched)
	hfs.UpdateBuildLabelMask(ctx, "", "label-64", []string{"shared.txt"})

	// label-1 should still exist!
	if !checkLabelExists("label-1") {
		t.Errorf("Expected label-1 to NOT be evicted because its timestamp was updated")
	}

	// label-2 should be evicted!
	if checkLabelExists("label-2") {
		t.Errorf("Expected label-2 to be evicted (LRU)")
	}
}

func TestHashFS_ActiveBuildLabels(t *testing.T) {
	ctx := t.Context()
	hfs, err := New(ctx, Option{})
	if err != nil {
		t.Fatalf("Failed to create HashFS: %v", err)
	}
	defer hfs.Close(ctx)

	// Add 3 labels
	hfs.UpdateBuildLabelMask(ctx, "", "label-c", []string{"c.txt"})
	hfs.UpdateBuildLabelMask(ctx, "", "label-a", []string{"a.txt"})
	hfs.UpdateBuildLabelMask(ctx, "", "label-b", []string{"b.txt"})

	labels := hfs.ActiveBuildLabels()
	want := []string{"label-a", "label-b", "label-c"}
	if diff := cmp.Diff(want, labels); diff != "" {
		t.Errorf("ActiveBuildLabels diff (-want +got):\n%s", diff)
	}

	// Add an entry with tombstoneBitID (63) directly to buildLabelDictionary,
	// verifying ActiveBuildLabels() ignores dictionary entries >= maxDynamicBuildLabels.
	hfs.ledgerMu.Lock()
	hfs.buildLabelDictionary[tombstoneBitID] = &pb.BuildLabelMetadata{
		BuildLabel:         "tombstone-sentinel",
		LastBuildTimestamp: time.Now().UnixNano(),
	}
	hfs.ledgerMu.Unlock()

	labels = hfs.ActiveBuildLabels()
	if diff := cmp.Diff(want, labels); diff != "" {
		t.Errorf("ActiveBuildLabels with tombstone diff (-want +got):\n%s", diff)
	}
}

func TestHashFS_UpdateBuildLabelMask_RebuildTombstonedFile(t *testing.T) {
	ctx := t.Context()
	hfs, err := New(ctx, Option{})
	if err != nil {
		t.Fatalf("Failed to create HashFS: %v", err)
	}
	defer hfs.Close(ctx)

	dir := t.TempDir()
	filePath := filepath.Join(dir, "rebuilt_file.txt")
	slashPath := filepath.ToSlash(filePath)

	// 1. Mark a file as tombstoned
	hfs.ledgerMu.Lock()
	hfs.fileBuildLabels[slashPath] = tombstoneMask
	hfs.ledgerMu.Unlock()

	// 2. Rebuild the file with an active build label
	hfs.UpdateBuildLabelMask(ctx, dir, "active-label", []string{"rebuilt_file.txt"})

	// 3. Verify tombstoneMask has been stripped and only the active label bit is present
	var activeID uint32
	var found bool
	hfs.ledgerMu.RLock()
	for id, meta := range hfs.buildLabelDictionary {
		if meta != nil && meta.BuildLabel == "active-label" {
			activeID = id
			found = true
			break
		}
	}
	gotMask := hfs.fileBuildLabels[slashPath]
	hfs.ledgerMu.RUnlock()

	if !found {
		t.Fatalf("Expected active-label to exist in build label dictionary")
	}

	wantMask := uint64(1) << activeID
	if gotMask != wantMask {
		t.Errorf("Got mask %x, want %x (tombstone bit should be stripped)", gotMask, wantMask)
	}
}

func TestHashFS_UpdateBuildLabelMask_DynamicCountWithNilMetadata(t *testing.T) {
	ctx := t.Context()
	hfs, err := New(ctx, Option{})
	if err != nil {
		t.Fatalf("Failed to create HashFS: %v", err)
	}
	defer hfs.Close(ctx)

	// Populate dynamic slots with one nil metadata entry to ensure
	// allocateBitIDLocked handles nil metadata gracefully without panic or count skew.
	hfs.ledgerMu.Lock()
	for i := range uint32(10) {
		hfs.buildLabelDictionary[i] = &pb.BuildLabelMetadata{
			BuildLabel:         fmt.Sprintf("label-%d", i),
			LastBuildTimestamp: time.Now().UnixNano(),
		}
	}
	hfs.buildLabelDictionary[10] = nil
	hfs.ledgerMu.Unlock()

	// Allocating a new label should find an empty slot (e.g. 11) without error.
	hfs.UpdateBuildLabelMask(ctx, "", "label-new", []string{"new_file.txt"})

	hfs.ledgerMu.RLock()
	if hfs.buildLabelDictionary[10] != nil {
		t.Errorf("hfs.buildLabelDictionary[10] = %v, want nil", hfs.buildLabelDictionary[10])
	}
	newMeta := hfs.buildLabelDictionary[11]
	hfs.ledgerMu.RUnlock()

	if newMeta == nil {
		t.Fatalf("hfs.buildLabelDictionary[11] = nil, want label-new")
	}
	if got, want := newMeta.BuildLabel, "label-new"; got != want {
		t.Fatalf("hfs.buildLabelDictionary[11].BuildLabel = %q, want %q", got, want)
	}
}
