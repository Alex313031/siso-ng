// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package hashfs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	pb "go.chromium.org/build/siso/hashfs/proto"
)

func setupGCFileSystem(t *testing.T) (*HashFS, string, string, string, string) {
	t.Helper()
	dir := t.TempDir()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}

	hfs := &HashFS{
		buildLabelDictionary: make(map[uint32]*pb.BuildLabelMetadata),
		fileBuildLabels:      make(map[string]uint64),
	}

	now := time.Now().UnixNano()

	hfs.buildLabelDictionary[0] = &pb.BuildLabelMetadata{BuildLabel: "chromeos", LastBuildTimestamp: now - time.Hour.Nanoseconds()}
	hfs.buildLabelDictionary[1] = &pb.BuildLabelMetadata{BuildLabel: "android", LastBuildTimestamp: now - time.Second.Nanoseconds()}
	hfs.buildLabelDictionary[2] = &pb.BuildLabelMetadata{BuildLabel: "pixel", LastBuildTimestamp: now - (24 * time.Hour).Nanoseconds()}

	fileA := filepath.Join(dir, "chromeos_exclusive")
	fileB := filepath.Join(dir, "android_exclusive")
	fileC := filepath.Join(dir, "pixel_exclusive")
	fileShared := filepath.Join(dir, "shared_bin")

	os.WriteFile(fileA, []byte("111"), 0644)
	os.WriteFile(fileB, []byte("2222"), 0644)
	os.WriteFile(fileC, []byte("33333"), 0644)
	os.WriteFile(fileShared, []byte("999999"), 0644)

	hfs.fileBuildLabels[fileA] = 1 << 0
	hfs.fileBuildLabels[fileB] = 1 << 1
	hfs.fileBuildLabels[fileC] = 1 << 2
	hfs.fileBuildLabels[fileShared] = (1 << 0) | (1 << 1)

	return hfs, fileA, fileB, fileC, fileShared
}

func checkFile(t *testing.T, path, name string, wantDeleted bool) {
	t.Helper()
	_, gotErr := os.Stat(path)
	if wantDeleted {
		if !os.IsNotExist(gotErr) {
			t.Errorf("os.Stat(%q)=_, %v; want os.ErrNotExist", name, gotErr)
		}
	} else {
		if gotErr != nil {
			t.Errorf("os.Stat(%q)=_, %v; want nil err", name, gotErr)
		}
	}
}

func checkLedger(t *testing.T, hfs *HashFS, path, name string, wantDeleted bool) {
	t.Helper()
	_, gotExists := hfs.fileBuildLabels[path]
	if wantDeleted {
		if gotExists {
			t.Errorf("hfs.fileBuildLabels[%q]=true; want false", name)
		}
	} else {
		if !gotExists {
			t.Errorf("hfs.fileBuildLabels[%q]=false; want true", name)
		}
	}
}

func TestGarbageCollectBuildLabels(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	tests := []struct {
		name                   string
		opts                   BuildLabelGCOptions
		wantErr                bool
		wantFilesDeleted       int
		wantBytesReclaimed     int64
		wantEvictedBuildLabels []string
		wantFileADeleted       bool
		wantFileBDeleted       bool
		wantFileCDeleted       bool
		wantFileSharedDeltd    bool
	}{
		{
			name: "Strategy_EvictLabels",
			opts: BuildLabelGCOptions{
				Strategy: BuildLabelGCSweepStrategy{EvictLabels: []string{"chromeos"}},
				DryRun:   false,
			},
			wantFilesDeleted:       1,
			wantBytesReclaimed:     3,
			wantEvictedBuildLabels: []string{"chromeos"},
			wantFileADeleted:       true,
			wantFileBDeleted:       false,
			wantFileCDeleted:       false,
			wantFileSharedDeltd:    false,
		},
		{
			name: "Strategy_KeepLabels_Regex",
			opts: BuildLabelGCOptions{
				Strategy: BuildLabelGCSweepStrategy{KeepLabels: []string{"and.*", "pix.*"}},
				DryRun:   false,
			},
			wantFilesDeleted:       1,
			wantBytesReclaimed:     3,
			wantEvictedBuildLabels: []string{"chromeos"},
			wantFileADeleted:       true,
			wantFileBDeleted:       false,
			wantFileCDeleted:       false,
			wantFileSharedDeltd:    false,
		},
		{
			name: "Strategy_RetainLastX",
			opts: BuildLabelGCOptions{
				Strategy: BuildLabelGCSweepStrategy{RetainLastX: 1},
				DryRun:   false,
			},
			wantFilesDeleted:       2,
			wantBytesReclaimed:     8,
			wantEvictedBuildLabels: []string{"chromeos", "pixel"},
			wantFileADeleted:       true,
			wantFileBDeleted:       false,
			wantFileCDeleted:       true,
			wantFileSharedDeltd:    false,
		},
		{
			name: "Strategy_OlderThan",
			opts: BuildLabelGCOptions{
				Strategy: BuildLabelGCSweepStrategy{OlderThan: 2 * time.Hour},
				DryRun:   false,
			},
			wantFilesDeleted:       1,
			wantBytesReclaimed:     5,
			wantEvictedBuildLabels: []string{"pixel"},
			wantFileADeleted:       false,
			wantFileBDeleted:       false,
			wantFileCDeleted:       true,
			wantFileSharedDeltd:    false,
		},
		{
			name: "DryRun",
			opts: BuildLabelGCOptions{
				Strategy: BuildLabelGCSweepStrategy{EvictLabels: []string{"pixel"}},
				DryRun:   true,
			},
			wantErr:                false,
			wantFilesDeleted:       1,
			wantBytesReclaimed:     5,
			wantEvictedBuildLabels: []string{"pixel"},
			wantFileADeleted:       false,
			wantFileBDeleted:       false,
			wantFileCDeleted:       false,
			wantFileSharedDeltd:    false,
		},
		{
			name: "NoStrategy_Error",
			opts: BuildLabelGCOptions{
				Strategy: BuildLabelGCSweepStrategy{},
				DryRun:   true,
			},
			wantErr: true,
		},
		{
			name: "MultipleStrategies_Error",
			opts: BuildLabelGCOptions{
				Strategy: BuildLabelGCSweepStrategy{
					RetainLastX: 1,
					OlderThan:   time.Hour,
				},
				DryRun: true,
			},
			wantErr: true,
		},
		{
			name: "InvalidRegex_Error",
			opts: BuildLabelGCOptions{
				Strategy: BuildLabelGCSweepStrategy{EvictLabels: []string{"["}},
				DryRun:   true,
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			hfs, fileA, fileB, fileC, fileShared := setupGCFileSystem(t)

			gotResult, gotErr := hfs.GarbageCollectBuildLabels(ctx, tt.opts)
			if tt.wantErr {
				if gotErr == nil {
					t.Fatalf("GarbageCollectBuildLabels(...)=_, nil; want error")
				}
				return
			}
			if gotErr != nil {
				t.Fatalf("GarbageCollectBuildLabels(...)=_, %v; want nil err", gotErr)
			}

			if gotResult.TotalFilesDeleted != tt.wantFilesDeleted {
				t.Errorf("TotalFilesDeleted=%d; want=%d", gotResult.TotalFilesDeleted, tt.wantFilesDeleted)
			}
			if gotResult.TotalBytesReclaimed != tt.wantBytesReclaimed {
				t.Errorf("TotalBytesReclaimed=%d; want=%d", gotResult.TotalBytesReclaimed, tt.wantBytesReclaimed)
			}

			sort.Strings(gotResult.EvictedBuildLabels)
			sort.Strings(tt.wantEvictedBuildLabels)
			if diff := cmp.Diff(tt.wantEvictedBuildLabels, gotResult.EvictedBuildLabels); diff != "" {
				t.Errorf("EvictedBuildLabels diff -want +got:\n%s", diff)
			}

			checkFile(t, fileA, "fileA", tt.wantFileADeleted)
			checkFile(t, fileB, "fileB", tt.wantFileBDeleted)
			checkFile(t, fileC, "fileC", tt.wantFileCDeleted)
			checkFile(t, fileShared, "fileShared", tt.wantFileSharedDeltd)

			checkLedger(t, hfs, fileA, "fileA", tt.wantFileADeleted && !tt.opts.DryRun)
			checkLedger(t, hfs, fileB, "fileB", tt.wantFileBDeleted && !tt.opts.DryRun)
			checkLedger(t, hfs, fileC, "fileC", tt.wantFileCDeleted && !tt.opts.DryRun)
		})
	}
}

func TestGarbageCollectBuildLabels_TombstoneBit(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	dir := t.TempDir()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}

	hfs := &HashFS{
		buildLabelDictionary: make(map[uint32]*pb.BuildLabelMetadata),
		fileBuildLabels:      make(map[string]uint64),
	}

	now := time.Now().UnixNano()
	hfs.buildLabelDictionary[0] = &pb.BuildLabelMetadata{BuildLabel: "chromeos", LastBuildTimestamp: now - time.Hour.Nanoseconds()}
	hfs.buildLabelDictionary[1] = &pb.BuildLabelMetadata{BuildLabel: "android", LastBuildTimestamp: now - time.Second.Nanoseconds()}

	tombstoneExcl := filepath.Join(dir, "tombstone_exclusive.txt")
	tombstoneShared := filepath.Join(dir, "tombstone_shared.txt")
	androidExcl := filepath.Join(dir, "android_exclusive.txt")

	os.WriteFile(tombstoneExcl, []byte("tombstone_data"), 0644)
	os.WriteFile(tombstoneShared, []byte("shared_data"), 0644)
	os.WriteFile(androidExcl, []byte("android_data"), 0644)

	// File exclusively tombstoned (bit 63)
	hfs.fileBuildLabels[filepath.ToSlash(tombstoneExcl)] = tombstoneMask
	// File shared between tombstone and active label 1 (android)
	hfs.fileBuildLabels[filepath.ToSlash(tombstoneShared)] = tombstoneMask | (1 << 1)
	// File exclusively owned by active label 1
	hfs.fileBuildLabels[filepath.ToSlash(androidExcl)] = 1 << 1

	// Strategy: Keep all active labels (android, chromeos)
	opts := BuildLabelGCOptions{
		Strategy: BuildLabelGCSweepStrategy{KeepLabels: []string{"android", "chromeos"}},
		DryRun:   false,
	}

	gotResult, gotErr := hfs.GarbageCollectBuildLabels(ctx, opts)
	if gotErr != nil {
		t.Fatalf("GarbageCollectBuildLabels failed: %v", gotErr)
	}

	// Only tombstoneExcl should be deleted
	if gotResult.TotalFilesDeleted != 1 {
		t.Errorf("TotalFilesDeleted=%d; want 1", gotResult.TotalFilesDeleted)
	}
	if gotResult.TotalBytesReclaimed != int64(len("tombstone_data")) {
		t.Errorf("TotalBytesReclaimed=%d; want %d", gotResult.TotalBytesReclaimed, len("tombstone_data"))
	}
	// EvictedBuildLabels should be empty since no dynamic labels were evicted
	if len(gotResult.EvictedBuildLabels) != 0 {
		t.Errorf("EvictedBuildLabels=%v; want empty", gotResult.EvictedBuildLabels)
	}

	// Check physical file status
	checkFile(t, tombstoneExcl, "tombstoneExcl", true)
	checkFile(t, tombstoneShared, "tombstoneShared", false)
	checkFile(t, androidExcl, "androidExcl", false)

	// Check ledger status
	checkLedger(t, hfs, filepath.ToSlash(tombstoneExcl), "tombstoneExcl", true)
	checkLedger(t, hfs, filepath.ToSlash(tombstoneShared), "tombstoneShared", false)
	checkLedger(t, hfs, filepath.ToSlash(androidExcl), "androidExcl", false)

	// In the ledger, tombstoneShared should have had tombstoneMask stripped, retaining bit 1
	if mask := hfs.fileBuildLabels[filepath.ToSlash(tombstoneShared)]; mask != (1 << 1) {
		t.Errorf("tombstoneShared mask=%b; want %b", mask, uint64(1<<1))
	}

	// Active dictionary entries must remain intact
	if _, ok := hfs.buildLabelDictionary[0]; !ok {
		t.Errorf("label 0 (chromeos) was unexpectedly deleted from dictionary")
	}
	if _, ok := hfs.buildLabelDictionary[1]; !ok {
		t.Errorf("label 1 (android) was unexpectedly deleted from dictionary")
	}
}

func TestGarbageCollectBuildLabels_OverflowLRU(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}

	opt := Option{
		BuildLabel:    "test-label",
		StateFile:     filepath.Join(dir, "test_hashfs.state"),
		CompressLevel: 0,
	}
	hfs, err := New(ctx, opt)
	if err != nil {
		t.Fatalf("Failed to create HashFS: %v", err)
	}
	defer hfs.Close(ctx)

	// 1. Create physical files and register 63 dynamic labels
	file1 := filepath.Join(dir, "file-1.txt")
	file2 := filepath.Join(dir, "file-2.txt")
	sharedFile := filepath.Join(dir, "shared.txt")

	os.WriteFile(file1, []byte("data1"), 0644)
	os.WriteFile(file2, []byte("data2"), 0644)
	os.WriteFile(sharedFile, []byte("datashared"), 0644)

	for i := 1; i <= 63; i++ {
		labelName := fmt.Sprintf("label-%d", i)
		files := []string{sharedFile}
		if i == 1 {
			files = append(files, file1)
		}
		if i == 2 {
			files = append(files, file2)
		}
		hfs.UpdateBuildLabelMask(ctx, dir, labelName, files)
		time.Sleep(1 * time.Millisecond) // strictly order timestamps for LRU
	}

	// 2. Add the 64th label to trigger LRU eviction of label-1
	file64 := filepath.Join(dir, "file-64.txt")
	os.WriteFile(file64, []byte("data64"), 0644)
	hfs.UpdateBuildLabelMask(ctx, dir, "label-64", []string{sharedFile, file64})

	// Verify file-1.txt has been tagged with tombstoneMask
	hfs.ledgerMu.RLock()
	mask1 := hfs.fileBuildLabels[filepath.ToSlash(file1)]
	hfs.ledgerMu.RUnlock()
	if mask1 != tombstoneMask {
		t.Fatalf("file-1.txt mask = %x; want tombstoneMask %x", mask1, tombstoneMask)
	}

	// 3. Run GarbageCollectBuildLabels retaining all 63 active labels
	opts := BuildLabelGCOptions{
		Strategy: BuildLabelGCSweepStrategy{RetainLastX: 63},
		DryRun:   false,
	}
	result, err := hfs.GarbageCollectBuildLabels(ctx, opts)
	if err != nil {
		t.Fatalf("GarbageCollectBuildLabels failed: %v", err)
	}

	// file-1.txt should have been swept and deleted
	if result.TotalFilesDeleted != 1 {
		t.Errorf("TotalFilesDeleted=%d; want 1", result.TotalFilesDeleted)
	}
	checkFile(t, file1, "file1", true)
	checkFile(t, file2, "file2", false)
	checkFile(t, sharedFile, "sharedFile", false)
	checkFile(t, file64, "file64", false)

	checkLedger(t, hfs, filepath.ToSlash(file1), "file1", true)
	checkLedger(t, hfs, filepath.ToSlash(file2), "file2", false)
	checkLedger(t, hfs, filepath.ToSlash(sharedFile), "sharedFile", false)
	checkLedger(t, hfs, filepath.ToSlash(file64), "file64", false)
}

func TestGarbageCollectBuildLabels_ContextCancel(t *testing.T) {
	t.Parallel()
	hfs, fileA, _, _, _ := setupGCFileSystem(t)

	// Pre-cancel the context to simulate an instant Ctrl-C SIGINT abort
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	opts := BuildLabelGCOptions{
		Strategy: BuildLabelGCSweepStrategy{EvictLabels: []string{"chromeos"}},
		DryRun:   false,
	}

	gotResult, gotErr := hfs.GarbageCollectBuildLabels(ctx, opts)
	if gotErr == nil {
		t.Fatalf("GarbageCollectBuildLabels with canceled context should return err")
	}

	if gotResult.TotalFilesDeleted != 0 {
		t.Errorf("TotalFilesDeleted=%d; want=0", gotResult.TotalFilesDeleted)
	}

	// Verify fileA was not physically removed because context was canceled before deletion
	checkFile(t, fileA, "fileA", false)
	checkLedger(t, hfs, fileA, "fileA", false)
	if _, ok := hfs.buildLabelDictionary[0]; !ok {
		t.Errorf("Target ID 0 (chromeos) was erroneously stripped from dictionary during abort")
	}
}

func TestGarbageCollectBuildLabels_FailedDeletions(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("Windows does not securely process directory permission mocking for DeleteFile structurally.")
	}

	hfs, fileA, _, _, _ := setupGCFileSystem(t)

	dir := filepath.Dir(fileA)
	if err := os.Chmod(dir, 0555); err != nil {
		t.Skipf("Cannot mock OS-level directory lock dynamically on this architecture: %v", err)
	}
	defer os.Chmod(dir, 0755)

	opts := BuildLabelGCOptions{
		Strategy: BuildLabelGCSweepStrategy{EvictLabels: []string{"chromeos"}},
		DryRun:   false,
	}

	gotResult, gotErr := hfs.GarbageCollectBuildLabels(t.Context(), opts)
	if gotErr != nil {
		t.Fatalf("GarbageCollectBuildLabels should cleanly ignore physical IO limits: _, %v; want nil err", gotErr)
	}

	if gotResult.TotalFilesDeleted != 0 {
		t.Errorf("TotalFilesDeleted=%d; want 0", gotResult.TotalFilesDeleted)
	}
	if len(gotResult.FailedDeletions) != 1 {
		t.Errorf("len(FailedDeletions)=%d; want 1", len(gotResult.FailedDeletions))
	} else if filepath.Base(gotResult.FailedDeletions[0]) != "chromeos_exclusive" {
		t.Errorf("FailedDeletions[0]=%q; want target matching chromeos_exclusive", gotResult.FailedDeletions[0])
	}

	checkFile(t, fileA, "fileA", false)
	checkLedger(t, hfs, fileA, "fileA", false)
	if _, ok := hfs.buildLabelDictionary[0]; !ok {
		t.Errorf("Target ID 0 (chromeos) was erroneously stripped from dictionary after failed deletion")
	}
}

func TestUpdateLedger(t *testing.T) {
	t.Parallel()

	hfs := &HashFS{
		buildLabelDictionary: map[uint32]*pb.BuildLabelMetadata{
			0: {BuildLabel: "label-0"},
			1: {BuildLabel: "label-1"},
			2: {BuildLabel: "label-2"},
		},
		fileBuildLabels: map[string]uint64{
			"/path/to/deleted_file": 1 << 0,
			"/path/to/shared_file":  (1 << 0) | (1 << 1),
			"/path/to/other_file":   1 << 2,
		},
	}
	hfs.clean.Store(true)

	opts := BuildLabelGCOptions{DryRun: false}
	evictMask := uint64(1 << 0)
	successfullyDeleted := []string{"/path/to/deleted_file"}

	hfs.updateLedger(opts, evictMask, successfullyDeleted)

	// Successfully deleted file should be removed from the ledger.
	if _, ok := hfs.fileBuildLabels["/path/to/deleted_file"]; ok {
		t.Errorf("deleted_file remained in fileBuildLabels")
	}

	// Surviving shared file should have the evicted bit cleared.
	if gotMask, ok := hfs.fileBuildLabels["/path/to/shared_file"]; !ok {
		t.Errorf("shared_file was unexpectedly deleted from fileBuildLabels")
	} else if gotMask != (1 << 1) {
		t.Errorf("shared_file mask = %b; want %b", gotMask, uint64(1<<1))
	}

	// Unrelated file should remain unchanged.
	if gotMask, ok := hfs.fileBuildLabels["/path/to/other_file"]; !ok {
		t.Errorf("other_file was unexpectedly deleted from fileBuildLabels")
	} else if gotMask != (1 << 2) {
		t.Errorf("other_file mask = %b; want %b", gotMask, uint64(1<<2))
	}

	// Evicted label should be removed from the dictionary because all exclusive files were deleted.
	if _, ok := hfs.buildLabelDictionary[0]; ok {
		t.Errorf("evicted label 0 remained in buildLabelDictionary")
	}

	// Non-evicted labels should remain in the dictionary.
	if _, ok := hfs.buildLabelDictionary[1]; !ok {
		t.Errorf("label 1 was unexpectedly removed from buildLabelDictionary")
	}
	if _, ok := hfs.buildLabelDictionary[2]; !ok {
		t.Errorf("label 2 was unexpectedly removed from buildLabelDictionary")
	}

	// Clean state should be marked false to ensure persistence on Close.
	if hfs.clean.Load() {
		t.Errorf("hfs.clean is true; want false")
	}
}

func TestUpdateLedger_TombstoneHandling(t *testing.T) {
	t.Parallel()

	hfs := &HashFS{
		buildLabelDictionary: map[uint32]*pb.BuildLabelMetadata{
			0: {BuildLabel: "label-0"},
			1: {BuildLabel: "label-1"},
		},
		fileBuildLabels: map[string]uint64{
			"/path/to/deleted_tombstone": tombstoneMask,
			"/path/to/aborted_tombstone": tombstoneMask,
			"/path/to/shared_tombstone":  tombstoneMask | (1 << 1),
			"/path/to/active_file":       1 << 0,
		},
	}
	hfs.clean.Store(true)

	opts := BuildLabelGCOptions{DryRun: false}
	evictMask := tombstoneMask
	successfullyDeleted := []string{"/path/to/deleted_tombstone"}

	hfs.updateLedger(opts, evictMask, successfullyDeleted)

	// Successfully deleted tombstoned file is removed.
	if _, ok := hfs.fileBuildLabels["/path/to/deleted_tombstone"]; ok {
		t.Errorf("deleted_tombstone remained in fileBuildLabels")
	}

	// Aborted tombstoned file retains tombstoneMask.
	if gotMask, ok := hfs.fileBuildLabels["/path/to/aborted_tombstone"]; !ok {
		t.Errorf("aborted_tombstone was unexpectedly purged from fileBuildLabels")
	} else if gotMask != tombstoneMask {
		t.Errorf("aborted_tombstone mask = %x; want %x", gotMask, tombstoneMask)
	}

	// Shared file has tombstoneMask stripped, retaining bit 1.
	if gotMask, ok := hfs.fileBuildLabels["/path/to/shared_tombstone"]; !ok {
		t.Errorf("shared_tombstone was unexpectedly deleted from fileBuildLabels")
	} else if gotMask != (1 << 1) {
		t.Errorf("shared_tombstone mask = %b; want %b", gotMask, uint64(1<<1))
	}

	// Unrelated active file is unchanged.
	if gotMask, ok := hfs.fileBuildLabels["/path/to/active_file"]; !ok {
		t.Errorf("active_file was unexpectedly deleted from fileBuildLabels")
	} else if gotMask != (1 << 0) {
		t.Errorf("active_file mask = %b; want %b", gotMask, uint64(1<<0))
	}

	// Active dictionary labels (0 and 1) must remain untouched.
	if _, ok := hfs.buildLabelDictionary[0]; !ok {
		t.Errorf("label 0 was unexpectedly removed from buildLabelDictionary")
	}
	if _, ok := hfs.buildLabelDictionary[1]; !ok {
		t.Errorf("label 1 was unexpectedly removed from buildLabelDictionary")
	}
}

func TestUpdateLedger_RetainsAbortedFiles(t *testing.T) {
	t.Parallel()

	hfs := &HashFS{
		buildLabelDictionary: map[uint32]*pb.BuildLabelMetadata{
			0: {BuildLabel: "label-0"},
			1: {BuildLabel: "label-1"},
		},
		fileBuildLabels: map[string]uint64{
			"/path/to/deleted_file": 1 << 0,
			"/path/to/aborted_file": 1 << 0,
			"/path/to/shared_file":  (1 << 0) | (1 << 1),
		},
	}
	hfs.clean.Store(true)

	opts := BuildLabelGCOptions{DryRun: false}
	evictMask := uint64(1 << 0)
	successfullyDeleted := []string{"/path/to/deleted_file"}

	hfs.updateLedger(opts, evictMask, successfullyDeleted)

	// Successfully deleted file is removed.
	if _, ok := hfs.fileBuildLabels["/path/to/deleted_file"]; ok {
		t.Errorf("deleted_file remained in fileBuildLabels")
	}

	// Aborted exclusive file retains its mask so Siso continues tracking it for future GC sweeps.
	if gotMask, ok := hfs.fileBuildLabels["/path/to/aborted_file"]; !ok {
		t.Errorf("aborted_file was unexpectedly purged from fileBuildLabels")
	} else if gotMask != (1 << 0) {
		t.Errorf("aborted_file mask = %b; want %b", gotMask, uint64(1<<0))
	}

	// Shared file has evictMask stripped.
	if gotMask, ok := hfs.fileBuildLabels["/path/to/shared_file"]; !ok {
		t.Errorf("shared_file was unexpectedly deleted from fileBuildLabels")
	} else if gotMask != (1 << 1) {
		t.Errorf("shared_file mask = %b; want %b", gotMask, uint64(1<<1))
	}

	// Evicted label is NOT removed from dictionary because aborted_file survived.
	if _, ok := hfs.buildLabelDictionary[0]; !ok {
		t.Errorf("label 0 was unexpectedly stripped from dictionary while aborted_file survives")
	}

	// Non-evicted label remains in dictionary.
	if _, ok := hfs.buildLabelDictionary[1]; !ok {
		t.Errorf("label 1 was unexpectedly removed from buildLabelDictionary")
	}
}

func TestUpdateLedger_DryRun(t *testing.T) {
	t.Parallel()

	hfs := &HashFS{
		buildLabelDictionary: map[uint32]*pb.BuildLabelMetadata{
			0: {BuildLabel: "label-0"},
		},
		fileBuildLabels: map[string]uint64{
			"/path/to/file": 1 << 0,
		},
	}
	hfs.clean.Store(true)

	opts := BuildLabelGCOptions{DryRun: true}
	evictMask := uint64(1 << 0)
	successfullyDeleted := []string{"/path/to/file"}

	hfs.updateLedger(opts, evictMask, successfullyDeleted)

	if _, ok := hfs.fileBuildLabels["/path/to/file"]; !ok {
		t.Errorf("DryRun modified fileBuildLabels")
	}
	if _, ok := hfs.buildLabelDictionary[0]; !ok {
		t.Errorf("DryRun modified buildLabelDictionary")
	}
	if !hfs.clean.Load() {
		t.Errorf("DryRun modified hfs.clean; want true")
	}
}
