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

// TestBuild_DirOutputRemoteShrinkNested covers a directory output with a
// subdirectory. A subdirectory is recorded as a member only because
// expandDirOutputs keeps the subdir nodes from the tree, so entriesFromResult
// stamps the directory's cmdhash on each; without that gendir/sub carries no
// cmdhash, looks stale, and is removed with everything under it.
//
// Removing a member also drops its hashfs record, so a later flush cannot
// re-create a wrongly removed member and hide the bug. Build 1 emits
// gendir/{a, sub/{b,c}}, build 2 emits gendir/{a, sub/b}; sub/b must survive
// and sub/c must go.
func TestBuild_DirOutputRemoteShrinkNested(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	ctx := t.Context()
	dir := tempDir(t)
	setupFiles(t, dir, "TestBuild_DirOutputRemoteShrinkPruned", nil)

	fileNode := func(re *reapitest.Fake, name, key string) (*rpb.FileNode, error) {
		content := []byte("content-" + key)
		if _, err := re.Put(ctx, content); err != nil {
			return nil, err
		}
		return &rpb.FileNode{
			Name:   name,
			Digest: blob.FromBytes(digest.SHA256, key, content).Digest().Proto(),
		}, nil
	}

	// dirResult builds gendir/ with a top-level "a" plus a "sub" subdirectory
	// holding the named members.
	dirResult := func(re *reapitest.Fake, subMembers ...string) (*rpb.ActionResult, error) {
		a, err := fileNode(re, "a", "gendir/a")
		if err != nil {
			return nil, err
		}
		var subFiles []*rpb.FileNode
		for _, m := range subMembers {
			f, err := fileNode(re, m, "gendir/sub/"+m)
			if err != nil {
				return nil, err
			}
			subFiles = append(subFiles, f)
		}
		sub := &rpb.Directory{Files: subFiles}
		subData, err := blob.FromProtoMessage(digest.SHA256, sub)
		if err != nil {
			return nil, err
		}
		root := &rpb.Directory{
			Files: []*rpb.FileNode{a},
			Directories: []*rpb.DirectoryNode{
				{Name: "sub", Digest: subData.Digest().Proto()},
			},
		}
		// ParseTree registers Children in the blob store, so merkletree.Traverse
		// can resolve the "sub" DirectoryNode.
		tree := &rpb.Tree{Root: root, Children: []*rpb.Directory{sub}}
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
			members := []string{"b", "c"}
			if execCount >= 2 {
				members = []string{"b"} // build 2: gendir/sub/c is dropped
			}
			res, err := dirResult(re, members...)
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

	if err := runNinja(t); err != nil {
		t.Fatalf("build 1: %v", err)
	}
	for _, m := range []string{"a", "sub/b", "sub/c"} {
		if _, err := os.Stat(filepath.Join(dir, "out/siso/gendir", m)); err != nil {
			t.Fatalf("after build 1, gendir/%s: %v; want present", m, err)
		}
	}

	if err := os.WriteFile(filepath.Join(dir, "input"), []byte("v2\n"), 0644); err != nil {
		t.Fatal(err)
	}

	outputLocal = false
	if err := runNinja(t); err != nil {
		t.Fatalf("build 2: %v", err)
	}
	if execCount < 2 {
		t.Fatalf("step did not re-execute in build 2 (execCount=%d); the shrink path was not exercised", execCount)
	}
	for _, m := range []string{"a", "sub", "sub/b"} {
		if _, err := os.Stat(filepath.Join(dir, "out/siso/gendir", m)); err != nil {
			t.Errorf("after build 2, gendir/%s: %v; want present; a recorded subdirectory member must survive the prune", m, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "out/siso/gendir/sub/c")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("gendir/sub/c still on disk after the dir output shrank (err=%v); the nested stale member was not pruned", err)
	}
}
