// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package reapi_test

import (
	"maps"
	"slices"
	"testing"

	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/reapi/digest"
	"go.chromium.org/build/siso/reapi/merkletree"
	"go.chromium.org/build/siso/reapi/reapitest"
)

func TestWalkDir(t *testing.T) {
	ctx := t.Context()
	ds := digest.NewStore()
	tree := merkletree.New(ds)
	for _, s := range []string{
		"file1",
		"subdir1/file1",
		"subdir2/file1",
		"subdir2/subdir2.1/file1",
	} {
		tree.Set(merkletree.Entry{
			Name: s,
			Data: digest.FromBytes("empty", nil),
		})
	}
	d, err := tree.Build(ctx)
	if err != nil {
		t.Fatal(err)
	}

	fakere := &reapitest.Fake{}
	cl := reapitest.New(ctx, t, fakere)

	t.Logf("-- upload tree %s", d)
	n, err := cl.UploadAll(ctx, ds)
	if err != nil {
		t.Fatalf("UploadAll()=%d, %v; want _, nil", n, err)
	}

	names := func(dir *rpb.Directory) (files, dirs, symlinks []string) {
		for _, file := range dir.Files {
			files = append(files, file.GetName())
		}
		for _, subdir := range dir.Directories {
			dirs = append(dirs, subdir.GetName())
		}
		for _, symlink := range dir.Symlinks {
			symlinks = append(symlinks, symlink.GetName())
		}
		return files, dirs, symlinks
	}
	seen := make(map[string]bool)

	err = cl.WalkDir(ctx, d, func(dname string, dir *rpb.Directory) error {
		seen[dname] = true
		files, dirs, _ := names(dir)
		switch dname {
		case "":
			wantFiles := []string{"file1"}
			wantDirs := []string{"subdir1", "subdir2"}
			if !slices.Equal(files, wantFiles) || !slices.Equal(dirs, wantDirs) {
				t.Errorf("dir:%q files=%q dirs=%q; want: files=%q dirs=%q",
					dname, files, dirs, wantFiles, wantDirs)
			}
		case "subdir1", "subdir2/subdir2.1":
			wantFiles := []string{"file1"}
			var wantDirs []string
			if !slices.Equal(files, wantFiles) || !slices.Equal(dirs, wantDirs) {
				t.Errorf("dir:%q files=%q dirs=%q; want: files=%q dirs=%q",
					dname, files, dirs, wantFiles, wantDirs)
			}

		case "subdir2":
			wantFiles := []string{"file1"}
			wantDirs := []string{"subdir2.1"}
			if !slices.Equal(files, wantFiles) || !slices.Equal(dirs, wantDirs) {
				t.Errorf("dir:%q files=%q dirs=%q; want: files=%q dirs=%q",
					dname, files, dirs, wantFiles, wantDirs)
			}
		}
		return nil
	})
	if err != nil {
		t.Errorf("WalkDir=%v; want nil", err)
	}
	wantSeen := map[string]bool{
		"":                  true,
		"subdir1":           true,
		"subdir2":           true,
		"subdir2/subdir2.1": true,
	}
	if !maps.Equal(seen, wantSeen) {
		t.Errorf("seen=%v; want=%v", seen, wantSeen)
	}
}
