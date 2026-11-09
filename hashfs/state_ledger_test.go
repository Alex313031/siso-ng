// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package hashfs

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/testing/protocmp"

	pb "go.chromium.org/build/siso/hashfs/proto"
	"go.chromium.org/build/siso/path"
)

func TestBuildLabelLedger_PersistenceAndValidation(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()

	// 1. Create physical files so they are valid active entries when HashFS loads.
	// HashFS SetState uses Lstat on the local disk.
	active1 := filepath.Join(dir, "active1.txt")
	active2 := filepath.Join(dir, "active2.txt")
	active3 := filepath.Join(dir, "active3.txt")
	input1 := filepath.Join(dir, "forgotten_input.txt")
	dirFile1 := filepath.Join(dir, "active_dir", "file1.txt")
	dirFile2 := filepath.Join(dir, "active_dir", "subdir", "file2.txt")
	if err := os.WriteFile(active1, []byte("data1"), 0644); err != nil {
		t.Fatalf("failed to create active1.txt: %v", err)
	}
	if err := os.WriteFile(active2, []byte("data2"), 0644); err != nil {
		t.Fatalf("failed to create active2.txt: %v", err)
	}
	if err := os.WriteFile(active3, []byte("data3"), 0644); err != nil {
		t.Fatalf("failed to create active3.txt: %v", err)
	}
	if err := os.WriteFile(input1, []byte("input1"), 0644); err != nil {
		t.Fatalf("failed to create forgotten_input.txt: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(dirFile2), 0755); err != nil {
		t.Fatalf("failed to create dir: %v", err)
	}
	if err := os.WriteFile(dirFile1, []byte("dir1"), 0644); err != nil {
		t.Fatalf("failed to create dir/file1.txt: %v", err)
	}
	if err := os.WriteFile(dirFile2, []byte("dir2"), 0644); err != nil {
		t.Fatalf("failed to create dir/subdir/file2.txt: %v", err)
	}

	statFile := func(path string) os.FileInfo {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("failed to stat %s: %v", path, err)
		}
		return fi
	}

	fi1 := statFile(active1)
	fi2 := statFile(active2)
	fi3 := statFile(active3)
	fiInput := statFile(input1)
	fiDir1 := statFile(dirFile1)
	fiDir2 := statFile(dirFile2)

	expectedPath1 := filepath.ToSlash(active1)
	expectedPath2 := filepath.ToSlash(active2)
	expectedPath3 := filepath.ToSlash(active3)
	expectedPathInput := filepath.ToSlash(input1)
	expectedPathDir1 := filepath.ToSlash(dirFile1)
	expectedPathDir2 := filepath.ToSlash(dirFile2)
	expectedDir := filepath.ToSlash(filepath.Dir(dirFile1))
	expectedGhost := filepath.ToSlash(filepath.Join(dir, "ghost.txt"))

	// We create a pb.State containing the entries, plus our build label tracking maps.
	state := &pb.State{
		Entries: []*pb.Entry{
			{
				Id:      &pb.FileID{ModTime: fi1.ModTime().UnixNano()},
				Name:    expectedPath1,
				Digest:  &pb.Digest{Hash: "d1", SizeBytes: 5},
				CmdHash: []byte("fakehash1"), // Marked as previously generated
			},
			{
				Id:      &pb.FileID{ModTime: fi2.ModTime().UnixNano()},
				Name:    expectedPath2,
				Digest:  &pb.Digest{Hash: "d2", SizeBytes: 5},
				CmdHash: []byte("fakehash2"),
			},
			{
				Id:      &pb.FileID{ModTime: fi3.ModTime().UnixNano()},
				Name:    expectedPath3,
				Digest:  &pb.Digest{Hash: "d3", SizeBytes: 5},
				CmdHash: []byte("fakehash3"),
			},
			{
				Id:     &pb.FileID{ModTime: fiInput.ModTime().UnixNano()},
				Name:   expectedPathInput,
				Digest: &pb.Digest{Hash: "d4", SizeBytes: 6},
			},
			{
				Id:      &pb.FileID{ModTime: fiDir1.ModTime().UnixNano()},
				Name:    expectedPathDir1,
				Digest:  &pb.Digest{Hash: "d5", SizeBytes: 4},
				CmdHash: []byte("fakehash5"),
			},
			{
				Id:      &pb.FileID{ModTime: fiDir2.ModTime().UnixNano()},
				Name:    expectedPathDir2,
				Digest:  &pb.Digest{Hash: "d6", SizeBytes: 4},
				CmdHash: []byte("fakehash6"),
			},
		},
		BuildLabelDictionary: []*pb.BuildLabelDictionaryEntry{
			{Id: 1, Metadata: &pb.BuildLabelMetadata{BuildLabel: "base:base", LastBuildTimestamp: time.Now().UnixNano()}},
		},
		FileBuildLabels: []*pb.FileBuildLabel{
			{Path: expectedPath1, Mask: 1},
			{Path: expectedPath2, Mask: 1},
			{Path: expectedPathInput, Mask: 1},
			{Path: expectedPathDir1, Mask: 1},
			{Path: expectedPathDir2, Mask: 1},
			{Path: expectedGhost, Mask: 1},
		},
	}

	var hashfsSetStateLog syncBuffer
	opt := Option{
		BuildLabel:     "test-label",
		StateFile:      filepath.Join(dir, "test_hashfs.state"),
		CompressLevel:  0,
		SetStateLogger: &hashfsSetStateLog,
	}
	hfs, err := New(ctx, opt)
	if err != nil {
		t.Fatalf("hashfs.New: %v", err)
	}
	defer func() {
		err := hfs.Close(ctx)
		if err != nil {
			t.Fatalf("hfs.Close: %v", err)
		}
		if s := hashfsSetStateLog.String(); s != "" {
			t.Log(s)
		}
	}()

	if err := hfs.WaitReady(ctx); err != nil {
		t.Fatalf("WaitReady=%v; want nil", err)
	}

	// 2. SetState: Load Validation occurs here.
	// The ghost.txt entry should be automatically dropped from hfs.fileBuildLabels.
	err = hfs.SetState(ctx, state)
	if err != nil {
		t.Fatalf("SetState failed: %v", err)
	}
	if err := hfs.WaitReady(ctx); err != nil {
		t.Fatalf("WaitReady failed: %v", err)
	}

	func() {
		hfs.ledgerMu.Lock()
		defer hfs.ledgerMu.Unlock()
		hfs.fileBuildLabels[expectedPath1] = 1
		hfs.fileBuildLabels[expectedPath2] = 2
		hfs.fileBuildLabels[expectedPath3] = 3
		hfs.fileBuildLabels[expectedPathInput] = 4
		hfs.fileBuildLabels[expectedPathDir1] = 5
		hfs.fileBuildLabels[expectedPathDir2] = 6
	}()

	// 3. Runtime Hook Validation: Remove a file, ensure it's deleted from build label tracking maps
	err = hfs.Remove(ctx, "", path.Path(expectedPath2))
	if err != nil {
		t.Fatalf("Remove active2.txt failed: %v", err)
	}

	hfs.ForgetOutputs(ctx, "", []path.Path{path.Path(expectedPath3)})
	hfs.Forget(ctx, "", []path.Path{path.Path(expectedPathInput)})

	err = hfs.RemoveAll(ctx, "", path.Path(expectedDir))
	if err != nil {
		t.Fatalf("RemoveAll active_dir failed: %v", err)
	}

	// Check that runtime hooks fired
	hfs.ledgerMu.RLock()
	_, ok1 := hfs.fileBuildLabels[expectedPath1]
	_, ok2 := hfs.fileBuildLabels[expectedPath2]
	_, ok3 := hfs.fileBuildLabels[expectedPath3]
	_, okInput := hfs.fileBuildLabels[expectedPathInput]
	_, okDir1 := hfs.fileBuildLabels[expectedPathDir1]
	_, okDir2 := hfs.fileBuildLabels[expectedPathDir2]
	hfs.ledgerMu.RUnlock()

	if !ok1 {
		t.Errorf("active1.txt should NOT have been deleted")
	}
	if ok2 {
		t.Errorf("active2.txt was not deleted from fileBuildLabels by Remove()")
	}
	if ok3 {
		t.Errorf("active3.txt was not deleted from fileBuildLabels by ForgetOutputs()")
	}
	if okInput {
		t.Errorf("forgotten_input.txt was not deleted from fileBuildLabels by Forget()")
	}
	if okDir1 || okDir2 {
		t.Errorf("nested files under active_dir were not deleted from fileBuildLabels by RemoveAll()")
	}

	// 4. Save Validation: State() export
	outState := hfs.State(ctx)

	// BuildLabelDictionary should be persisted exactly.
	if diff := cmp.Diff(state.BuildLabelDictionary, outState.BuildLabelDictionary, protocmp.Transform()); diff != "" {
		t.Errorf("BuildLabelDictionary mismatch (-want +got):\n%s", diff)
	}

	// FileBuildLabels should contain ONLY expectedPath1.
	// "ghost.txt" was purged by Load Validation.
	// "active2.txt" was purged by Runtime Deletion hook.
	wantFileLabels := []*pb.FileBuildLabel{
		{Path: expectedPath1, Mask: 1},
	}

	if diff := cmp.Diff(wantFileLabels, outState.FileBuildLabels, protocmp.Transform()); diff != "" {
		t.Errorf("FileBuildLabels mismatch (-want +got):\n%s", diff)
	}
}

func TestBuildLabelLedger_LoadLegacyEmptyState(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()

	// 1. Create a dummy state representing a legacy state file (maps are nil/empty)
	state := &pb.State{
		Entries: []*pb.Entry{
			{
				Name: "some_file.txt",
			},
		},
		BuildLabelDictionary: nil,
		FileBuildLabels:      nil,
	}

	t.Chdir(dir)

	hfs, err := New(ctx, Option{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer hfs.Close(ctx)

	// 2. Load the state - should succeed and allocate empty maps without panicking
	err = hfs.SetState(ctx, state)
	if err != nil {
		t.Fatalf("SetState failed: %v", err)
	}

	hfs.ledgerMu.RLock()
	dictionaryLen := len(hfs.buildLabelDictionary)
	labelsLen := len(hfs.fileBuildLabels)
	hfs.ledgerMu.RUnlock()

	if dictionaryLen != 0 {
		t.Errorf("Got buildLabelDictionary size %d, want 0", dictionaryLen)
	}
	if labelsLen != 0 {
		t.Errorf("Got fileBuildLabels size %d, want 0", labelsLen)
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (n int, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
