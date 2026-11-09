// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package e2etests

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.chromium.org/build/siso/build/ninjabuild"
	"go.chromium.org/build/siso/hashfs"
	pb "go.chromium.org/build/siso/hashfs/proto"
)

func TestBuild_BuildLabelMask(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	ctx := t.Context()
	dir := tempDir(t)

	setupFiles(t, dir, t.Name(), nil)

	// Helper to run ninja
	runNinjaTest := func(t *testing.T, buildLabel string) error {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile: ".siso_fs_state",
		})
		// Add the target
		opt.BuildLabel = buildLabel
		// Assures state is flushed
		defer cleanup()
		_, err := ninjabuild.Run(ctx, graph, opt, nil, ninjabuild.RunNinjaOpts{})
		return err
	}

	getState := func(t *testing.T) *pb.State {
		t.Helper()
		state, err := hashfs.Load(ctx, hashfs.Option{
			StateFile: filepath.Join(dir, "out/siso/.siso_fs_state"),
		})
		if err != nil {
			t.Fatalf("failed to load state: %v", err)
		}
		return state
	}

	getLabelMask := func(state *pb.State, fname string) uint64 {
		for _, lbl := range state.FileBuildLabels {
			if filepath.Base(lbl.Path) == filepath.Base(fname) {
				return lbl.Mask
			}
		}
		return 0
	}

	getBitID := func(state *pb.State, label string) *uint32 {
		for _, entry := range state.BuildLabelDictionary {
			if entry != nil && entry.Metadata != nil && entry.Metadata.BuildLabel == label {
				return &entry.Id
			}
		}
		return nil
	}

	assertMasks := func(t *testing.T, state *pb.State, paths []string, expectedMask uint64) {
		t.Helper()
		for _, p := range paths {
			if m := getLabelMask(state, p); m != expectedMask {
				t.Errorf("%s has mask %d, want exact mask %d", p, m, expectedMask)
			}
		}
	}

	// 1. Normal execution & early handleStep execution
	err := runNinjaTest(t, "target1")
	if err != nil {
		t.Fatalf("ninja err target1: %v", err)
	}

	state := getState(t)
	bitIDPtr1 := getBitID(state, "target1")
	if bitIDPtr1 == nil {
		t.Fatalf("target1 missing from label dictionary")
	}
	expectedMask1 := uint64(1) << *bitIDPtr1

	// checking obj/gen.txt and manifest (paths are relative to out/siso or absolute)
	assertMasks(t, state, []string{"build.ninja", "obj/gen.txt", "obj/copy.txt", "obj/downstream.txt"}, expectedMask1)

	// Wait 10ms for mtime check
	time.Sleep(10 * time.Millisecond)

	// Invalidate the input but KEEP the same content.
	// This will make 'gen' dirty, and run_step.go will evaluate it.
	// Since content is the same, hash is the same, meaning 'obj/downstream.txt' will be SKIPPED!
	// (Siso's needToRun will return false for 'obj/downstream.txt')
	err = os.Chtimes(filepath.Join(dir, "in/source.txt"), time.Now(), time.Now())
	if err != nil {
		t.Fatal(err)
	}

	// 2. Cached/Skipped execution (second run with target2)
	err = runNinjaTest(t, "target2")
	if err != nil {
		t.Fatalf("ninja err target2: %v", err)
	}

	state2 := getState(t)
	bitIDPtr2 := getBitID(state2, "target2")
	if bitIDPtr2 == nil {
		t.Fatalf("target2 missing from label dictionary")
	}
	expectedMask2 := uint64(1) << *bitIDPtr2

	assertMasks(t, state2, []string{"build.ninja", "obj/gen.txt", "obj/copy.txt", "obj/downstream.txt"}, expectedMask1|expectedMask2)
}
