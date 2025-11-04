// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package execute

import (
	"testing"

	"github.com/google/go-cmp/cmp"

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
				Dir: "",
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
				Dir: "",
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
				Dir: "out/Default",
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
			if diff := cmp.Diff(tc.wantEnts, ents, cmp.Comparer(cmpDigestData)); diff != "" {
				t.Errorf("ents: -want +got:\n%s", diff)
			}
			if diff := cmp.Diff(tc.wantTreeInputs, treeInputs); diff != "" {
				t.Errorf("treeInputs: -want +got:\n%s", diff)
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
