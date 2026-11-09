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

// TestBuild_DirOutputRemoteShrinkPruned proves the remote/cache materialization
// path removes stale members when a directory output shrinks across builds. The
// fake RBE returns gendir/{a,b} on the first run and gendir/{a} on the second
// (the input changes between builds, forcing re-execution). With output_local
// materialization, build 2 must leave only gendir/a on disk: the flush adds the
// new tree but never removes members, so without RemoveStaleDirMembers the stale
// gendir/b from build 1 would linger. gendir/a stays a member but its contents
// change, and the flush must rewrite it.
func TestBuild_DirOutputRemoteShrinkPruned(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	ctx := t.Context()
	dir := tempDir(t)
	setupFiles(t, dir, t.Name(), nil)

	// dirResult builds an ActionResult with gendir/ containing the named
	// members, each with contents specific to version.
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

	// post runs after a successful build, before hashfs shuts down.
	runNinja := func(t *testing.T, post func(t *testing.T, hfs *hashfs.HashFS)) error {
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
			OutputLocal: func(context.Context, string) bool { return true }, // materialize
		})
		defer cleanup()
		opt.REAPIClient = ds.Client
		_, err := ninjabuild.Run(ctx, graph, opt, nil, ninjabuild.RunNinjaOpts{})
		if err == nil && post != nil {
			post(t, opt.HashFS)
		}
		return err
	}

	// Build 1: gendir/{a,b}, both materialized.
	if err := runNinja(t, nil); err != nil {
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

	// Build 2: gendir/ shrinks to {a}. The stale gendir/b must be pruned from
	// disk AND stay out of the hashfs view: the prune must run before the
	// effective-mtime walk, whose ReadDir would re-merge the stale on-disk
	// member into hashfs as a phantom entry.
	if err := runNinja(t, func(t *testing.T, hfs *hashfs.HashFS) {
		if _, err := hfs.Stat(ctx, dir, "out/siso/gendir/b"); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("hashfs Stat(gendir/b) after build 2 = %v; want %v (phantom entry for a pruned member)", err, fs.ErrNotExist)
		}
	}); err != nil {
		t.Fatalf("build 2: %v", err)
	}
	if execCount < 2 {
		t.Fatalf("step did not re-execute in build 2 (execCount=%d); the shrink path was not exercised", execCount)
	}
	if _, err := os.Stat(filepath.Join(dir, "out/siso/gendir/a")); err != nil {
		t.Errorf("after build 2, gendir/a: %v; want present", err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "out/siso/gendir/a")); err != nil || string(got) != "content-a-v2" {
		t.Errorf("after build 2, gendir/a = %q, %v; want %q", got, err, "content-a-v2")
	}
	if _, err := os.Stat(filepath.Join(dir, "out/siso/gendir/b")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("gendir/b still on disk after the dir output shrank (err=%v); the stale member was not pruned", err)
	}
}
