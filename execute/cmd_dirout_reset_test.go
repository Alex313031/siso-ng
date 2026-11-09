// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package execute

import (
	"bytes"
	"context"
	"io"
	"testing"

	"google.golang.org/protobuf/proto"

	"go.chromium.org/build/hashigo/digest"
	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/blob"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/path"
)

// fakeTreeSource is a hashfs.DataSource serving preloaded blobs keyed by digest, so expandDirOutputs can flatten a directory output without a real CAS.
type fakeTreeSource struct {
	blobs map[digest.Digest][]byte
}

func (s fakeTreeSource) Source(_ context.Context, d digest.Digest, _ string) blob.Source {
	return fakeBlob{b: s.blobs[d]}
}

type fakeBlob struct{ b []byte }

func (b fakeBlob) Open(_ context.Context) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(b.b)), nil
}

func (b fakeBlob) String() string { return "fake-blob" }

// TestSetActionResult_ResetsDirOutputsExpanded verifies installing a fresh action result via SetActionResult lets that result's directory outputs be flattened again.
// expandDirOutputs flattens once per result (re-entry guard); a single *Cmd can probe a cached result then execute for real, so a new result must clear the guard or its dir output materializes empty.
func TestSetActionResult_ResetsDirOutputsExpanded(t *testing.T) {
	ctx := t.Context()

	d1 := digest.Digest{Hash: "filehash", SizeBytes: 5}
	tree := &rpb.Tree{
		Root: &rpb.Directory{
			Files: []*rpb.FileNode{
				{Name: "hello.txt", Digest: d1.Proto()},
			},
		},
	}
	treeBytes, err := proto.Marshal(tree)
	if err != nil {
		t.Fatal(err)
	}
	treeDg := blob.FromBytes(digest.SHA256, "tree", treeBytes).Digest()

	ds := fakeTreeSource{blobs: map[digest.Digest][]byte{treeDg: treeBytes}}

	// Each result carries only the bare directory node, as a fresh remote/cache
	// result does before expansion flattens its inner files.
	newResult := func() *rpb.ActionResult {
		return &rpb.ActionResult{
			OutputDirectories: []*rpb.OutputDirectory{
				{Path: "gendir", TreeDigest: treeDg.Proto()},
			},
		}
	}

	hfs, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}
	defer hfs.Close(ctx)
	c := &Cmd{
		WorkDir:    "out/Default",
		OutputDirs: []path.Path{"out/Default/gendir"},
		outfiles: map[path.Path]bool{
			"out/Default/gendir": true,
		},
		CmdHash: []byte("cmdhash"),
		HashFS:  hfs,
	}

	// First attempt (cache probe): flattens gendir/hello.txt and sets the guard.
	c.SetActionResult(newResult(), true)
	if err := c.expandDirOutputs(ctx, ds); err != nil {
		t.Fatalf("first expandDirOutputs: %v", err)
	}
	if got := len(c.actionResult.GetOutputFiles()); got != 1 {
		t.Fatalf("first expand: OutputFiles=%d, want 1 (setup error, not the bug under test)", got)
	}

	// Real execution installs a fresh result for the same *Cmd, which must flatten again.
	c.SetActionResult(newResult(), false)

	if err := c.expandDirOutputs(ctx, ds); err != nil {
		t.Fatalf("second expandDirOutputs: %v", err)
	}

	files := c.actionResult.GetOutputFiles()
	if len(files) == 0 {
		t.Fatalf("fresh result's dir output not flattened: OutputFiles empty; want gendir/hello.txt (SetActionResult did not reset dirOutputsExpanded, so it materializes empty)")
	}
	if got := files[0].GetPath(); got != "gendir/hello.txt" {
		t.Errorf("flattened file = %q; want gendir/hello.txt", got)
	}
}
