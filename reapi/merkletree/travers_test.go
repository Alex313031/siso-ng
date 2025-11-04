// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package merkletree_test

import (
	"path/filepath"
	"testing"

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/golang/glog"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/testing/protocmp"

	"go.chromium.org/build/siso/reapi/digest"
	"go.chromium.org/build/siso/reapi/merkletree"
)

func TestTraverse(t *testing.T) {
	ctx := t.Context()
	defer glog.Flush()

	ds := digest.NewStore()
	mt := merkletree.New(ds)

	file1 := digest.FromBytes("file1", []byte{1})
	file2 := digest.FromBytes("file2", []byte{2})

	// Set test entries.
	for _, e := range []merkletree.Entry{
		{
			Name: "file1",
			Data: file1,
		},
		{
			Name:   "link1",
			Target: "file1",
		},
		{
			Name: "dir1",
		},
		{
			Name: "dir2/file2",
			Data: file2,
		},
		{
			Name: "dir2/dir3",
		},
		{
			Name: "dir2/dir3/file1",
			Data: digest.FromBytes("file1", []byte{1}),
		},
		{
			Name:   "dir2/dir3/link2",
			Target: "../file2",
		},
	} {
		err := mt.Set(e)
		if err != nil {
			t.Fatalf("failed to set %v", e)
		}
	}
	_, err := mt.Build(ctx)
	if err != nil {
		t.Fatalf("failed to build merkletree: %v", err)
	}

	files, links, dirs := merkletree.Traverse(ctx, "root", mt.RootDirectory(), ds)

	// Assertions
	wantFiles := []*rpb.OutputFile{
		{
			Path:   filepath.Join("root", "file1"),
			Digest: file1.Digest().Proto(),
		},
		{
			Path:   filepath.Join("root", "dir2", "file2"),
			Digest: file2.Digest().Proto(),
		},
		{
			Path:   filepath.Join("root", "dir2", "dir3", "file1"),
			Digest: file1.Digest().Proto(),
		},
	}
	ignoreOpts := []cmp.Option{
		protocmp.Transform(),
		protocmp.IgnoreFields(new(rpb.OutputFile), "digest"),
	}
	if diff := cmp.Diff(wantFiles, files, ignoreOpts...); diff != "" {
		t.Errorf("merkletree.Traverse(...) = files, _, _: -want +got\n%s", diff)
	}

	wantLinks := []*rpb.OutputSymlink{
		{
			Path:   filepath.Join("root", "link1"),
			Target: "file1",
		},
		{
			Path:   filepath.Join("root", "dir2", "dir3", "link2"),
			Target: "../file2",
		},
	}
	ignoreOpts = []cmp.Option{
		protocmp.Transform(),
	}
	if diff := cmp.Diff(wantLinks, links, ignoreOpts...); diff != "" {
		t.Errorf("merkletree.Traverse(...) = _, _, _: -want +got\n%s", diff)
	}

	wantDirs := []*rpb.OutputDirectory{
		{
			Path: filepath.Join("root", "dir1"),
		},
		{
			Path: filepath.Join("root", "dir2"),
		},
		{
			Path: filepath.Join("root", "dir2", "dir3"),
		},
	}
	ignoreOpts = []cmp.Option{
		protocmp.Transform(),
		protocmp.IgnoreFields(new(rpb.OutputDirectory), "tree_digest"),
	}
	if diff := cmp.Diff(wantDirs, dirs, ignoreOpts...); diff != "" {
		t.Errorf("merkletree.Traverse(...) = _, _, dirs: -want +got\n%s", diff)
	}
}
