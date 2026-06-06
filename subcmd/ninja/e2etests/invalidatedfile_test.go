// Copyright 2025 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package e2etests

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/build/ninjabuild"
	"go.chromium.org/build/siso/hashfs"
	pb "go.chromium.org/build/siso/hashfs/proto"
	"go.chromium.org/build/siso/reapi/digest"
)

func TestBuild_InvalidatedFile(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	ctx := t.Context()
	dir := tempDir(t)

	runNinjaTest := func(t *testing.T) (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:   ".siso_fs_state",
			OutputLocal: func(context.Context, string) bool { return true },
		})
		defer cleanup()
		return ninjabuild.Run(ctx, graph, opt, []string{"out"}, ninjabuild.RunNinjaOpts{})
	}

	setupFiles(t, dir, t.Name(), nil)
	t.Logf("-- first build")
	stats, err := runNinjaTest(t)
	if err != nil {
		t.Fatalf("ninja %v", err)
	}
	if stats.Done != stats.Total || stats.Total != 1 {
		t.Errorf("done=%d total=%d; want done=1 total=1; %#v", stats.Done, stats.Total, stats)
	}

	t.Logf("-- confirm no-op")
	stats, err = runNinjaTest(t)
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
	fi, err := os.Stat(filepath.Join(dir, "in"))
	if err != nil {
		t.Fatalf("stat in: %v", err)
	}
	state, err := hashfs.Load(ctx, hashfs.Option{
		StateFile: filepath.Join(dir, "out/siso/.siso_fs_state"),
	})
	if err != nil {
		t.Fatalf("failed to load .siso_fs_state: %v", err)
	}
	stm := hashfs.StateMap(state)
	var buf bytes.Buffer

	// We don't need real now, just need a time newer than fi.ModTime().
	now := fi.ModTime().Add(1 * time.Second)
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
	stats, err = runNinjaTest(t)
	if err != nil {
		t.Fatalf("ninja %v", err)
	}
	if stats.Done != stats.Total || stats.Total != 1 || stats.Skipped != 0 {
		t.Errorf("done=%d total=%d skipped=%d; want done=1 total=1 skipped=0; %#v", stats.Done, stats.Total, stats.Skipped, stats)
	}
}

func TestBuild_InvalidatedBuildNinja(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	ctx := t.Context()
	dir := tempDir(t)

	runNinjaTest := func(t *testing.T) (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile: ".siso_fs_state",
		})
		defer cleanup()
		stats, err := ninjabuild.Run(ctx, graph, opt, []string{"out"}, ninjabuild.RunNinjaOpts{})
		if err == nil {
			opt.HashFS.SetBuildTargets(ctx, []string{"out"}, true)
		}
		return stats, err
	}

	setupFiles(t, dir, t.Name(), nil)
	t.Logf("-- first build")
	stats, err := runNinjaTest(t)
	if err != nil {
		t.Fatalf("ninja %v", err)
	}
	if stats.Done != stats.Total || stats.Total != 1 {
		t.Errorf("done=%d total=%d; want done=1 total=1; %#v", stats.Done, stats.Total, stats)
	}

	t.Logf("-- confirm no-op")
	stats, err = runNinjaTest(t)
	if err != nil {
		t.Fatalf("ninja %v", err)
	}
	if stats.Done != stats.Total || stats.Skipped != stats.Total || stats.Total != 1 {
		t.Errorf("done=%d total=%d skipped=%d; want done=1 total=1 skipped=1; %#v", stats.Done, stats.Total, stats.Skipped, stats)
	}

	t.Logf("-- modify build.ninja")
	modifyFile(t, dir, "out/siso/build.ninja", func(buf []byte) []byte {
		return bytes.Replace(buf,
			[]byte("command = python3 ../../cp.py "),
			[]byte("command = python3 ../../cp.py -r "),
			1,
		)
	})

	t.Logf("-- check hashfs is not clean")
	func() {
		var hashfsSetStateLog syncBuffer
		hfs, err := hashfs.New(ctx, hashfs.Option{
			SetStateLogger: &hashfsSetStateLog,
		})
		if err != nil {
			t.Fatalf("hashfs.New %v", err)
		}
		defer func() {
			err := hfs.Close(ctx)
			if err != nil {
				t.Fatalf("hfs.Close %v", err)
			}
			if s := hashfsSetStateLog.buf.String(); s != "" {
				t.Log(s)
			}
		}()
		st, err := hashfs.Load(ctx, hashfs.Option{
			StateFile: filepath.Join(dir, "out/siso/.siso_fs_state"),
		})
		if err != nil {
			t.Fatalf("hashfs.Load %v", err)
		}
		err = hfs.SetState(ctx, st)
		if err != nil {
			t.Fatalf("hfs.SetState %v", err)
		}
		err = hfs.WaitReady(ctx)
		if err != nil {
			t.Fatalf("hfs.WaitReady %v", err)
		}
		if hfs.IsClean([]string{"out"}) {
			t.Errorf("hfs.IsClean=true; want false")
		}
	}()
	t.Logf("-- third build")
	stats, err = runNinjaTest(t)
	if err != nil {
		t.Fatalf("ninja %v", err)
	}
	if stats.Done != stats.Total || stats.Total != 1 || stats.Skipped != 0 {
		t.Errorf("done=%d total=%d skipped=%d; want done=1 total=1 skipped=0; %#v", stats.Done, stats.Total, stats.Skipped, stats)
	}
}
