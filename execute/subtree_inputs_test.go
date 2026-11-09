// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package execute

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"google.golang.org/protobuf/proto"

	"go.chromium.org/build/hashigo/digest"
	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/blob"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/path"
	"go.chromium.org/build/siso/reapi/merkletree"
)

func testTree(name string) merkletree.TreeEntry {
	return merkletree.TreeEntry{
		Name:   path.Path(name),
		Digest: blob.FromBytes(digest.SHA256, name, []byte(name)).Digest(),
	}
}

// TestCmdDigestSubtreeInputs checks which inputs dropSubtreeInputs drops,
// and that the input root of Cmd.Digest is the same as when no input is
// dropped.
func TestCmdDigestSubtreeInputs(t *testing.T) {
	inputs := []path.Path{
		"src/a.cc",
		"tc",
		"tc/",
		"tc/bin/clang",
		"tc/include/x.h",
		"tc/include/sub/y.h",
		"tc2/z.h",
		"tcx/z.h",
		"tc/../src/b.h",
		"sys/usr/include/c.h",
		"sys/usr/lib/d.so",
		"sys/e",
		"out/gen/f.h",
	}
	for _, tc := range []struct {
		name         string
		workDir      path.Path
		canonicalize bool
		chroot       bool
		remoteInputs map[path.Path]path.Path
		trees        []merkletree.TreeEntry
		inputs       []path.Path
		want         []path.Path // inputs after dropSubtreeInputs
	}{
		{
			name:   "no_subtrees",
			inputs: inputs,
			want:   inputs,
		},
		{
			name:   "one_subtree",
			trees:  []merkletree.TreeEntry{testTree("tc")},
			inputs: inputs,
			want:   []path.Path{"src/a.cc", "tc2/z.h", "tcx/z.h", "sys/usr/include/c.h", "sys/usr/lib/d.so", "sys/e", "out/gen/f.h"},
		},
		{
			name:   "two_subtrees",
			trees:  []merkletree.TreeEntry{testTree("tc"), testTree("sys")},
			inputs: inputs,
			want:   []path.Path{"src/a.cc", "tc2/z.h", "tcx/z.h", "out/gen/f.h"},
		},
		{
			// SetTree rejects the inner subtree.
			name:   "inner_after_outer",
			trees:  []merkletree.TreeEntry{testTree("sys"), testTree("sys/usr/include")},
			inputs: inputs,
			want:   []path.Path{"src/a.cc", "tc", "tc/", "tc/bin/clang", "tc/include/x.h", "tc/include/sub/y.h", "tc2/z.h", "tcx/z.h", "tc/../src/b.h", "out/gen/f.h"},
		},
		{
			// SetTree rejects the outer subtree.
			name:   "outer_after_inner",
			trees:  []merkletree.TreeEntry{testTree("sys/usr/include"), testTree("sys")},
			inputs: inputs,
			want:   []path.Path{"src/a.cc", "tc", "tc/", "tc/bin/clang", "tc/include/x.h", "tc/include/sub/y.h", "tc2/z.h", "tcx/z.h", "tc/../src/b.h", "sys/usr/lib/d.so", "sys/e", "out/gen/f.h"},
		},
		{
			name:   "same_subtree_twice",
			trees:  []merkletree.TreeEntry{testTree("tc"), testTree("tc")},
			inputs: inputs,
			want:   []path.Path{"src/a.cc", "tc2/z.h", "tcx/z.h", "sys/usr/include/c.h", "sys/usr/lib/d.so", "sys/e", "out/gen/f.h"},
		},
		{
			name:   "unclean_subtree",
			trees:  []merkletree.TreeEntry{testTree("tc"), testTree("./sys")},
			inputs: inputs,
			want:   inputs,
		},
		{
			// build/debug/foo.h moves to out/x/foo.h, outside the subtree.
			name:         "canonicalize",
			workDir:      "build/debug",
			canonicalize: true,
			trees:        []merkletree.TreeEntry{testTree("build")},
			inputs:       []path.Path{"src/a.cc", "build/a.h", "build/debug/foo.h"},
			want:         []path.Path{"src/a.cc", "build/debug/foo.h"},
		},
		{
			// "build/" expands to build/debug/foo.h, which moves to
			// out/x/foo.h, outside the subtree.
			name:         "canonicalize_dir_input",
			workDir:      "build/debug",
			canonicalize: true,
			trees:        []merkletree.TreeEntry{testTree("build")},
			inputs:       []path.Path{"src/a.cc", "build/a.h", "build/"},
			want:         []path.Path{"src/a.cc", "build/"},
		},
		{
			// chrootDir cleans ".d/../src/x.h" to a path outside ".d".
			name:   "remote_chroot",
			chroot: true,
			trees:  []merkletree.TreeEntry{testTree(".d")},
			inputs: []path.Path{"src/a.cc", ".d/../src/x.h"},
			want:   []path.Path{"src/a.cc", ".d/../src/x.h"},
		},
		{
			name:         "remote_inputs",
			remoteInputs: map[path.Path]path.Path{"tc/include/x.h": "src/x.h", "src/y.h": "tc/bin/clang"},
			trees:        []merkletree.TreeEntry{testTree("tc")},
			inputs:       []path.Path{"src/a.cc", "tc/bin/clang", "tc/include/x.h"},
			want:         []path.Path{"src/a.cc", "tc/bin/clang", "tc/include/x.h"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			dir, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			for _, f := range []string{
				"src/a.cc", "src/b.h", "src/x.h",
				"tc/bin/clang", "tc/include/x.h", "tc/include/sub/y.h",
				"tc2/z.h", "tcx/z.h",
				"sys/usr/include/c.h", "sys/usr/lib/d.so", "sys/e",
				"out/gen/f.h",
				"build/a.h", "build/debug/foo.h",
			} {
				fname := filepath.Join(dir, f)
				if err := os.MkdirAll(filepath.Dir(fname), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(fname, []byte(f), 0644); err != nil {
					t.Fatal(err)
				}
			}
			hashFS, err := hashfs.New(ctx, hashfs.Option{})
			if err != nil {
				t.Fatal(err)
			}
			defer hashFS.Close(ctx)
			if err := hashFS.WaitReady(ctx); err != nil {
				t.Fatal(err)
			}
			workDir := tc.workDir
			if workDir == "" {
				workDir = "out"
			}
			c := &Cmd{
				ID:              tc.name,
				Args:            []string{"cc"},
				WorkspaceRoot:   dir,
				WorkDir:         workDir,
				Pure:            true,
				HashFS:          hashFS,
				CanonicalizeDir: tc.canonicalize,
				RemoteInputs:    tc.remoteInputs,
				TreeInputs:      tc.trees,
				Inputs:          tc.inputs,
			}
			if tc.chroot {
				c.Platform = map[string]string{"dockerChrootPath": "."}
			}
			if got := c.dropSubtreeInputs(slices.Clone(tc.inputs)); !slices.Equal(got, tc.want) {
				t.Errorf("dropSubtreeInputs=%q; want %q", got, tc.want)
			}

			ds := blob.NewStore()
			ad, err := c.Digest(ctx, ds)
			if err != nil {
				t.Fatal(err)
			}
			action := &rpb.Action{}
			if err := unmarshalBlob(ctx, ds, ad, action); err != nil {
				t.Fatal(err)
			}
			got := digest.FromProto(action.InputRootDigest)

			// Same as Cmd.Digest, but inputTree drops nothing
			// without TreeInputs.
			nc := *c
			nc.TreeInputs = nil
			ents, err := nc.inputTree(ctx)
			if err != nil {
				t.Fatal(err)
			}
			trees := c.TreeInputs
			if c.CanonicalizeDir {
				ents, trees = c.canonicalizeDir(ctx, ents, trees)
			}
			if c.RemoteChroot() {
				ents, trees = c.chrootDir(ctx, ents, trees)
			}
			want, err := treeDigest(ctx, hashFS.DigestFunction(), trees, ents, blob.NewStore())
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Errorf("input root=%v; want %v", got, want)
			}
		})
	}
}

func unmarshalBlob(ctx context.Context, ds *blob.Store, d digest.Digest, m proto.Message) error {
	data, ok := ds.Get(d)
	if !ok {
		return fmt.Errorf("%v not found", d)
	}
	r, err := data.Open(ctx)
	if err != nil {
		return err
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	return proto.Unmarshal(b, m)
}
