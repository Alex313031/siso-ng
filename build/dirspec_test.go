// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"go.chromium.org/build/hashigo/digest"
	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/hashfs"
)

func TestDirSpec(t *testing.T) {
	dir := &rpb.Directory{
		Files:       []*rpb.FileNode{{Name: "f"}, {Name: "g"}},
		Directories: []*rpb.DirectoryNode{{Name: "d"}},
		Symlinks:    []*rpb.SymlinkNode{{Name: "s", Target: "f"}},
	}
	dd := digest.Digest{Hash: "0123", SizeBytes: 4}
	b := &Builder{path: NewPath("/ws", "out")}

	spec := b.dirSpec("a/b", dd, dir)
	want := []string{"a/b/f", "a/b/g", "a/b/d", "a/b/s"}
	if diff := cmp.Diff(want, spec.names); diff != "" {
		t.Errorf("dirSpec(%q).names diff -want +got:\n%s", "a/b", diff)
	}
	if got := b.dirSpec("a/b", dd, dir); got != spec {
		t.Errorf("dirSpec(%q, %v) was not shared", "a/b", dd)
	}
	if got := b.dirSpec("a/c", dd, dir); got == spec {
		t.Errorf("dirSpec(%q, %v) shared with %q", "a/c", dd, "a/b")
	}

	root := b.dirSpec("", dd, dir)
	if diff := cmp.Diff([]string{"f", "g", "d", "s"}, root.names); diff != "" {
		t.Errorf("dirSpec(%q).names diff -want +got:\n%s", "", diff)
	}

	// Without a digest there is no key, so nothing is shared.
	s1 := b.dirSpec("a/b", digest.Digest{}, dir)
	s2 := b.dirSpec("a/b", digest.Digest{}, dir)
	if s1 == s2 {
		t.Errorf("dirSpec with zero digest was shared")
	}
}

// TestNewDirSpecMatchDir checks the fields matchInputRoot passes to
// hashfs.MatchDir, for a directory with and without symlinks.
func TestNewDirSpecMatchDir(t *testing.T) {
	fdig := digest.Digest{Hash: "abcd", SizeBytes: 3}
	for _, tc := range []struct {
		name  string
		dname string
		dir   *rpb.Directory
		want  *dirSpec
	}{
		{
			// Only Entries can check symlinks, so no children.
			name:  "symlink",
			dname: "a/b",
			dir: &rpb.Directory{
				Files:       []*rpb.FileNode{{Name: "f", Digest: fdig.Proto()}},
				Directories: []*rpb.DirectoryNode{{Name: "d"}},
				Symlinks:    []*rpb.SymlinkNode{{Name: "s", Target: "f"}},
			},
			want: &dirSpec{
				names:   []string{"a/b/f", "a/b/d", "a/b/s"},
				nfiles:  1,
				fulldir: "/ws/a/b",
			},
		},
		{
			name:  "no_symlink_at_the_root",
			dname: "",
			dir: &rpb.Directory{
				Files:       []*rpb.FileNode{{Name: "f", Digest: fdig.Proto(), IsExecutable: true}},
				Directories: []*rpb.DirectoryNode{{Name: "d"}},
			},
			want: &dirSpec{
				names:   []string{"f", "d"},
				nfiles:  1,
				fulldir: "/ws",
				children: []hashfs.DirChild{
					{Name: "f", Digest: fdig, IsExecutable: true},
					{Name: "d"},
				},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := newDirSpec("/ws", tc.dname, tc.dir)
			if diff := cmp.Diff(tc.want, got, cmp.AllowUnexported(dirSpec{})); diff != "" {
				t.Errorf("newDirSpec(%q) diff -want +got:\n%s", tc.dname, diff)
			}
		})
	}
}
