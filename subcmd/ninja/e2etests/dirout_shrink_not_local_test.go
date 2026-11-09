// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package e2etests

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/proto"

	"go.chromium.org/build/hashigo/digest"
	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/blob"
	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/build/ninjabuild"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/reapi/reapitest"
)

// TestBuild_DirOutputShrinkPrunedWhenNotLocal covers a directory output that an
// earlier build materialized and a later build rebuilds with output_local off.
// Build 1 materializes gendir/{a,b}; build 2 shrinks it to gendir/{a}, with new
// contents for a, and output_local off. NeedFlush is false in build 2, so a
// prune gated on it would leave gendir/b on disk, and a flush never removes
// it. Because the tree already exists on disk, build 2 flushes it anyway, so
// gendir/a must hold the new contents, not a mix of the old and new trees.
func TestBuild_DirOutputShrinkPrunedWhenNotLocal(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	ctx := t.Context()
	dir := tempDir(t)
	setupFiles(t, dir, "TestBuild_DirOutputRemoteShrinkPruned", nil)

	dirResult := func(re *reapitest.Fake, version int, members ...string) (*rpb.ActionResult, error) {
		var files []*rpb.FileNode
		for _, m := range members {
			content := fmt.Appendf(nil, "content-%s-v%d", m, version)
			if _, err := re.Put(ctx, content); err != nil {
				return nil, err
			}
			files = append(files, &rpb.FileNode{
				Name:   m,
				Digest: blob.FromBytes(digest.SHA256, "gendir/"+m, content).Digest().Proto(),
			})
		}
		tree := &rpb.Tree{Root: &rpb.Directory{Files: files}}
		treeBytes, err := proto.Marshal(tree)
		if err != nil {
			return nil, err
		}
		if _, err := re.Put(ctx, treeBytes); err != nil {
			return nil, err
		}
		treeDigest := blob.FromBytes(digest.SHA256, "gendir-tree", treeBytes)
		return &rpb.ActionResult{
			OutputDirectories: []*rpb.OutputDirectory{
				{Path: "gendir", TreeDigest: treeDigest.Digest().Proto()},
			},
		}, nil
	}

	execCount := 0
	fakere := &reapitest.Fake{
		ExecuteFunc: func(re *reapitest.Fake, action *rpb.Action) (*rpb.ActionResult, error) {
			execCount++
			members := []string{"a", "b"}
			if execCount >= 2 {
				members = []string{"a"} // build 2: gendir/ shrinks (b dropped)
			}
			res, err := dirResult(re, execCount, members...)
			if err != nil {
				return &rpb.ActionResult{ExitCode: 1, StderrRaw: fmt.Appendf(nil, "dirResult: %v", err)}, nil
			}
			return res, nil
		},
	}

	outputLocal := true
	runNinja := func(t *testing.T) error {
		t.Helper()
		var ds build.DataSource
		defer func() {
			if err := ds.Close(ctx); err != nil {
				t.Error(err)
			}
		}()
		ds.Client = reapitest.New(ctx, t, fakere)
		ds.Cache = ds.Client.CacheStore()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:   ".siso_fs_state",
			DataSource:  ds,
			OutputLocal: func(context.Context, string) bool { return outputLocal },
		})
		defer cleanup()
		opt.REAPIClient = ds.Client
		_, err := ninjabuild.Run(ctx, graph, opt, nil, ninjabuild.RunNinjaOpts{})
		return err
	}

	// Build 1 materializes gendir/{a,b}.
	if err := runNinja(t); err != nil {
		t.Fatalf("build 1: %v", err)
	}
	for _, m := range []string{"a", "b"} {
		if _, err := os.Stat(filepath.Join(dir, "out/siso/gendir", m)); err != nil {
			t.Fatalf("after build 1, gendir/%s: %v; want present", m, err)
		}
	}

	// Change the input so the step re-executes (cache miss).
	if err := os.WriteFile(filepath.Join(dir, "input"), []byte("v2\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Build 2 shrinks gendir/ to {a} with materialization left to consumers.
	outputLocal = false
	if err := runNinja(t); err != nil {
		t.Fatalf("build 2: %v", err)
	}
	if execCount < 2 {
		t.Fatalf("step did not re-execute in build 2 (execCount=%d); the shrink path was not exercised", execCount)
	}
	if _, err := os.Stat(filepath.Join(dir, "out/siso/gendir/b")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("gendir/b still on disk after the dir output shrank (err=%v); the prune was skipped because output_local is off", err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "out/siso/gendir/a")); err != nil || string(got) != "content-a-v2" {
		t.Errorf("after build 2, gendir/a = %q, %v; want %q; the tree on disk was not refreshed", got, err, "content-a-v2")
	}
}
