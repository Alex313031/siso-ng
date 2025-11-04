// Copyright 2025 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/hashfs"
	pb "go.chromium.org/build/siso/hashfs/proto"
	"go.chromium.org/build/siso/reapi/digest"
)

func TestBuild_InvalidatedFile(t *testing.T) {
	ctx := t.Context()
	dir := tempDir(t)

	ninja := func(t *testing.T) (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:   ".siso_fs_state",
			OutputLocal: func(context.Context, string) bool { return true },
		})
		defer cleanup()
		return runNinja(ctx, "build.ninja", graph, opt, []string{"out"}, runNinjaOpts{})
	}

	setupFiles(t, dir, t.Name(), nil)
	t.Logf("-- first build")
	stats, err := ninja(t)
	if err != nil {
		t.Fatalf("ninja %v", err)
	}
	if stats.Done != stats.Total || stats.Total != 1 {
		t.Errorf("done=%d total=%d; want done=1 total=1; %#v", stats.Done, stats.Total, stats)
	}

	t.Logf("-- confirm no-op")
	stats, err = ninja(t)
	if err != nil {
		t.Fatalf("ninja %v", err)
	}
	if stats.Done != stats.Total || stats.Skipped != stats.Total || stats.Total != 1 {
		t.Errorf("done=%d total=%d skipped=%d; want done=1 total=1 skipped=1; %#v", stats.Done, stats.Total, stats.Skipped, stats)
	}

	t.Logf("-- emulate interrupted build. journal hashfs, but not local disk")
	// time order: state:in, < disk:out < disk:in < state:out
	err = os.WriteFile(filepath.Join(dir, "in"), []byte("new input"), 0644)
	if err != nil {
		t.Fatalf("update in: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	state, err := hashfs.Load(ctx, hashfs.Option{
		StateFile: filepath.Join(dir, "out/siso/.siso_fs_state"),
	})
	if err != nil {
		t.Fatalf("failed to load .siso_fs_state: %v", err)
	}
	stm := hashfs.StateMap(state)
	var buf bytes.Buffer
	now := time.Now()
	d := digest.FromBytes("new-out", []byte("new input")).Digest()
	fname := filepath.ToSlash(filepath.Join(dir, "out/siso/out"))
	cmdhash := stm[fname].GetCmdHash()
	err = hashfs.JournalEntry(&buf, &pb.Entry{
		Id: &pb.FileID{
			ModTime: now.UnixNano(),
		},
		Name: fname,
		Digest: &pb.Digest{
			Hash:      d.Hash,
			SizeBytes: d.SizeBytes,
		},
		CmdHash:     cmdhash,
		UpdatedTime: now.UnixNano(),
	})
	if err != nil {
		t.Fatalf("failed to create journal data: %v", err)
	}
	err = os.WriteFile(filepath.Join(dir, "out/siso/.siso_fs_state.journal"), buf.Bytes(), 0644)
	if err != nil {
		t.Fatalf("failed to write journal: %v", err)
	}

	t.Logf("-- second build")
	stats, err = ninja(t)
	if err != nil {
		t.Fatalf("ninja %v", err)
	}
	if stats.Done != stats.Total || stats.Total != 1 || stats.Skipped != 0 {
		t.Errorf("done=%d total=%d skipped=%d; want done=1 total=1 skipped=0; %#v", stats.Done, stats.Total, stats.Skipped, stats)
	}
}
