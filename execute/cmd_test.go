// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package execute

import (
	"context"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/reapi/digest"
	"go.chromium.org/build/siso/reapi/merkletree"
)

func TestCanonicalizeDir(t *testing.T) {
	for _, tc := range []struct {
		name           string
		cmd            *Cmd
		ents           []merkletree.Entry
		treeInputs     []merkletree.TreeEntry
		wantEnts       []merkletree.Entry
		wantTreeInputs []merkletree.TreeEntry
	}{
		{
			name: "empty-dir",
			cmd: &Cmd{
				WorkDir: "",
			},
			ents: []merkletree.Entry{
				{
					Name: "out/Default",
				},
			},
			treeInputs: []merkletree.TreeEntry{
				{
					Name: "toolchain",
				},
			},
			wantEnts: []merkletree.Entry{
				{
					Name: "out/Default",
				},
			},
			wantTreeInputs: []merkletree.TreeEntry{
				{
					Name: "toolchain",
				},
			},
		},
		{
			name: "dot-dir",
			cmd: &Cmd{
				WorkDir: "",
			},
			ents: []merkletree.Entry{
				{
					Name: "out/Default",
				},
			},
			treeInputs: []merkletree.TreeEntry{
				{
					Name: "toolchain",
				},
			},
			wantEnts: []merkletree.Entry{
				{
					Name: "out/Default",
				},
			},
			wantTreeInputs: []merkletree.TreeEntry{
				{
					Name: "toolchain",
				},
			},
		},
		{
			name: "canonicalize-dir",
			cmd: &Cmd{
				WorkDir: "out/Default",
			},
			ents: []merkletree.Entry{
				{
					Name: "out/Default/file",
				},
				{
					Name: "out/Default/dir/file",
				},
				{
					Name: "file",
				},
				{
					Name: "dir/file",
				},
			},
			treeInputs: []merkletree.TreeEntry{
				{
					Name: "toolchain",
				},
				{
					Name: "out/Default/sdk",
				},
			},
			wantEnts: []merkletree.Entry{
				{
					Name: "out/x/file",
				},
				{
					Name: "out/x/dir/file",
				},
				{
					Name: "file",
				},
				{
					Name: "dir/file",
				},
			},
			wantTreeInputs: []merkletree.TreeEntry{
				{
					Name: "toolchain",
				},
				{
					Name: "out/x/sdk",
				},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			ents, treeInputs := tc.cmd.canonicalizeDir(ctx, tc.ents, tc.treeInputs)
			if diff := cmp.Diff(ents, tc.wantEnts, cmp.Comparer(cmpDigestData)); diff != "" {
				t.Errorf("ents: -got +want:\n%s", diff)
			}
			if diff := cmp.Diff(treeInputs, tc.wantTreeInputs); diff != "" {
				t.Errorf("treeInputs: -got +want:\n%s", diff)
			}
		})
	}
}

func cmpDigestData(a, b digest.Data) bool {
	if a.IsZero() == b.IsZero() {
		return true
	}
	return a.Digest() == b.Digest()
}

func TestEntriesFromResult_Auxiliary(t *testing.T) {
	ctx := t.Context()
	now := time.Now()

	d1 := digest.Digest{Hash: "hash1", SizeBytes: 1}
	d2 := digest.Digest{Hash: "hash2", SizeBytes: 2}
	tree := &rpb.Tree{
		Root: &rpb.Directory{
			Files: []*rpb.FileNode{
				{
					Name:   "file",
					Digest: d1.Proto(),
				},
			},
		},
	}
	treeData, err := digest.FromProtoMessage(tree)
	if err != nil {
		t.Fatal(err)
	}
	d3 := treeData.Digest()

	cmd := &Cmd{
		WorkDir: "out/Default",
		Outputs: []string{
			"out/Default/main.o",
		},
		AuxiliaryLogOutputFiles: []string{
			"out/Default/aux.d",
		},
		AuxiliaryLogOutputDirs: []string{
			"out/Default/aux_dir",
		},
		outfiles: map[string]bool{
			"out/Default/main.o": true,
		},
		actionResult: &rpb.ActionResult{
			OutputFiles: []*rpb.OutputFile{
				{Path: "main.o", Digest: d1.Proto()},
				{Path: "aux.d", Digest: d2.Proto()},
			},
			OutputDirectories: []*rpb.OutputDirectory{
				{Path: "aux_dir", TreeDigest: d3.Proto()},
			},
		},
	}

	ds := mockDataSource{}
	entries, additionalEntries := cmd.entriesFromResult(ctx, ds, now)

	// Verify main outputs (entries)
	wantEntries := []hashfs.UpdateEntry{
		{
			Name: "out/Default/main.o",
			Entry: &merkletree.Entry{
				Name: "out/Default/main.o",
				Data: digest.NewData(ds.Source(ctx, d1, "out/Default/main.o"), d1),
			},
			UpdatedTime: now,
			ModTime:     now,
			IsChanged:   true,
			Mode:        0644,
		},
	}
	if diff := cmp.Diff(entries, wantEntries, cmpopts.IgnoreUnexported(hashfs.UpdateEntry{}, merkletree.Entry{}, digest.Data{}), cmpopts.IgnoreFields(hashfs.UpdateEntry{}, "Action")); diff != "" {
		t.Errorf("entries mismatch (-got +want):\n%s", diff)
	}

	// Verify additional outputs (should be empty for auxiliary outputs)
	if len(additionalEntries) != 0 {
		t.Errorf("additionalEntries: got %q, want empty", additionalEntries)
	}

}

type mockDataSource struct{}

func (mockDataSource) Source(ctx context.Context, d digest.Digest, name string) digest.Source {
	return nil
}

func (mockDataSource) Close(ctx context.Context) error { return nil }
