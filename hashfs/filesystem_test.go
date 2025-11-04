// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package hashfs

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestReadDir_ReadsNEntries(t *testing.T) {
	cmpOpts := []cmp.Option{
		cmp.AllowUnexported(DirEntry{}),
		cmp.AllowUnexported(FileInfo{}),
	}
	makeDir := func() *Dir {
		return &Dir{
			ents: []DirEntry{
				{fi: FileInfo{fname: "foo"}},
				{fi: FileInfo{fname: "bar"}},
				{fi: FileInfo{fname: "baz"}},
				{fi: FileInfo{fname: "qux"}},
				{fi: FileInfo{fname: "quux"}},
			},
		}
	}
	allEntries := []fs.DirEntry{
		DirEntry{fi: FileInfo{fname: "foo"}},
		DirEntry{fi: FileInfo{fname: "bar"}},
		DirEntry{fi: FileInfo{fname: "baz"}},
		DirEntry{fi: FileInfo{fname: "qux"}},
		DirEntry{fi: FileInfo{fname: "quux"}},
	}

	// Read all with n = 0.
	entries, err := makeDir().ReadDir(0)

	if err != nil {
		t.Fatalf("dir.ReadDir(0)=_, %v; want nil err", err)
	}
	if diff := cmp.Diff(allEntries, entries, cmpOpts...); diff != "" {
		t.Errorf("dir.ReadDir(0) diff -want +got:\n%s", diff)
	}

	// Read all with n = -1.
	entries, err = makeDir().ReadDir(-1)

	if err != nil {
		t.Fatalf("dir.ReadDir(-1)=_, %v; want nil err", err)
	}
	if diff := cmp.Diff(allEntries, entries, cmpOpts...); diff != "" {
		t.Errorf("dir.ReadDir(-1) diff -want +got:\n%s", diff)
	}

	// Read len(allEntries).
	entries, err = makeDir().ReadDir(5)

	if err != nil {
		t.Fatalf("dir.ReadDir(5)=_, %v; want nil err", err)
	}
	if diff := cmp.Diff(allEntries, entries, cmpOpts...); diff != "" {
		t.Errorf("dir.ReadDir(5) diff -want +got:\n%s", diff)
	}

	// Read n > len(allEntries) should still return n.
	entries, err = makeDir().ReadDir(6)

	if err != nil {
		t.Fatalf("dir.ReadDir(6)=_, %v; want nil err", err)
	}
	if diff := cmp.Diff(allEntries, entries, cmpOpts...); diff != "" {
		t.Errorf("dir.ReadDir(6) diff -want +got:\n%s", diff)
	}
}

func TestReadDir_ReadsRemainingEntries(t *testing.T) {
	cmpOpts := []cmp.Option{
		cmp.AllowUnexported(DirEntry{}),
		cmp.AllowUnexported(FileInfo{}),
	}
	dir := &Dir{
		ents: []DirEntry{
			{fi: FileInfo{fname: "foo"}},
			{fi: FileInfo{fname: "bar"}},
			{fi: FileInfo{fname: "baz"}},
			{fi: FileInfo{fname: "qux"}},
			{fi: FileInfo{fname: "quux"}},
		},
	}
	allEntries := []fs.DirEntry{
		DirEntry{fi: FileInfo{fname: "foo"}},
		DirEntry{fi: FileInfo{fname: "bar"}},
		DirEntry{fi: FileInfo{fname: "baz"}},
		DirEntry{fi: FileInfo{fname: "qux"}},
		DirEntry{fi: FileInfo{fname: "quux"}},
	}

	// Read n where n < len(allEntries).
	entries, err := dir.ReadDir(3)

	if err != nil {
		t.Fatalf("dir.ReadDir(3)=_, %v; want nil err", err)
	}
	if diff := cmp.Diff(allEntries[0:3], entries, cmpOpts...); diff != "" {
		t.Errorf("dir.ReadDir(3) diff -want +got:\n%s", diff)
	}

	// Read remaining len(allEntries) - n.
	entries, err = dir.ReadDir(2)

	if err != nil {
		t.Fatalf("dir.ReadDir(2)=_, %v; want nil err", err)
	}
	if diff := cmp.Diff(allEntries[3:], entries, cmpOpts...); diff != "" {
		t.Errorf("dir.ReadDir(2) diff -want +got:\n%s", diff)
	}

	// Read again when n > 0 should be EOF.
	entries, err = dir.ReadDir(1)

	if len(entries) != 0 {
		t.Fatalf("len(entries) = %d; want 0", len(entries))
	}
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v; want io.EOF", err)
	}

	// Read again when n = 0 should be nil.
	entries, err = dir.ReadDir(0)

	if err != nil {
		t.Fatalf("dir.ReadDir(0)=_, %v; want nil err", err)
	}
	if len(entries) != 0 {
		t.Fatalf("len(entries) = %d; want 0", len(entries))
	}

	// Read again when n = -1 should be nil.
	entries, err = dir.ReadDir(-1)

	if err != nil {
		t.Fatalf("dir.ReadDir(0)=_, %v; want nil err", err)
	}
	if len(entries) != 0 {
		t.Fatalf("len(entries) = %d; want 0", len(entries))
	}
}

func TestReadDir_ErrIfEmptyDir(t *testing.T) {
	emptyDir := &Dir{}

	// Read empty.
	entries, err := emptyDir.ReadDir(3)

	if len(entries) != 0 {
		t.Fatalf("len(emptyDir.ReadDir(3)) = %d; want 0", len(entries))
	}
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v; want io.EOF", err)
	}
}

func TestFilesystemSub_SymlinkDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no symlink test on windows")
		return
	}
	ctx := t.Context()
	dir := t.TempDir()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	hfs, err := New(ctx, Option{})
	if err != nil {
		t.Fatalf("New=%v", err)
	}
	defer func() {
		err := hfs.Close(ctx)
		if err != nil {
			t.Errorf("hfs.Close=%v", err)
		}
	}()

	setupDir := func(dirname string) {
		t.Helper()
		fullpath := filepath.Join(dir, dirname)
		err := os.MkdirAll(fullpath, 0755)
		if err != nil {
			t.Fatalf("os.MkdirAll(%q,0755)=%v; want nil err", fullpath, err)
		}
	}
	setupFile := func(fname string) {
		t.Helper()
		setupDir(filepath.Dir(fname))
		fullpath := filepath.Join(dir, fname)
		err := os.WriteFile(fullpath, nil, 0644)
		if err != nil {
			t.Fatalf("os.WriteFile(%q, nil, 0644)=%v; want nil err", fullpath, err)
		}
	}
	setupSymlink := func(symlink, target string) {
		t.Helper()
		setupDir(filepath.Dir(symlink))
		fullpath := filepath.Join(dir, symlink)
		err := os.Symlink(target, fullpath)
		if err != nil {
			t.Fatalf("os.Symlink(%q, %q)=%v; want nil err", target, fullpath, err)
		}
	}

	setupFile("chrome-sdk/tarballs/sysroots/usr/include/stdio.h")
	setupSymlink("chrome-sdk/symlinks/sysroots", "../tarballs/sysroots")

	fsys := hfs.FileSystem(ctx, dir)

	subdir := "chrome-sdk/symlinks/sysroots"
	sub, err := fsys.Sub(subdir)
	if err != nil {
		t.Fatalf("fsys.Sub(%q)=_, %v; want nil err", subdir, err)
	}
	fname := "usr/include/stdio.h"
	_, err = fs.Stat(sub, fname)
	if err != nil {
		t.Errorf("sub.Stat(%q)=_, %v; want nil err", fname, err)
	}

	otherDir := t.TempDir()
	setupDirInOtherDir := func(dirname string) {
		t.Helper()
		fullpath := filepath.Join(otherDir, dirname)
		err := os.MkdirAll(fullpath, 0755)
		if err != nil {
			t.Fatalf("os.MkdirAll(%q,0755)=%v; want nil err", fullpath, err)
		}
	}
	setupFileInOtherDir := func(fname string) {
		t.Helper()
		setupDirInOtherDir(filepath.Dir(fname))
		fullpath := filepath.Join(otherDir, fname)
		err := os.WriteFile(fullpath, nil, 0644)
		if err != nil {
			t.Fatalf("os.WriteFile(%q, nil, 0644)=%v; want nil err", fullpath, err)
		}
	}

	setupFileInOtherDir("MacOSX.sdk/SDKSettings.json")
	setupSymlink("build/xcode_links/MacOSX.sdk", filepath.Join(otherDir, "MacOSX.sdk"))

	subdir = "build/xcode_links/MacOSX.sdk"
	sub, err = fsys.Sub(subdir)
	if err != nil {
		t.Fatalf("fsys.Sub(%q)=_, %v; want nil err", subdir, err)
	}
	fname = "SDKSettings.json"
	_, err = fs.Stat(sub, fname)
	if err != nil {
		t.Errorf("sub.Stat(%q)=_, %v; want nil err", fname, err)
	}
}

func TestResolveSymlinkPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink not available on windows")
		return
	}
	for _, tc := range []struct {
		root, path, target string
		want               string
	}{
		{
			root:   "/b/w",
			path:   "/b/w/foo/bar",
			target: "baz",
			want:   "foo/baz",
		},
		{
			root:   "/b/w",
			path:   "/b/w/foo/bar",
			target: "../baz",
			want:   "baz",
		},
		{
			root:   "/b/w",
			path:   "/b/w/foo/bar",
			target: "../../baz",
			want:   "/b/baz",
		},
		{
			root:   "/b/w",
			path:   "/b/w/foo/bar",
			target: "/b/w/baz",
			want:   "baz",
		},
		{
			root:   "/b/w",
			path:   "/b/w/foo/bar",
			target: "/b/baz",
			want:   "/b/baz",
		},
		{
			root:   "/b/w",
			path:   "/b/x",
			target: "baz",
			want:   "/b/baz",
		},
		{
			root:   "/b/w/build/cros_cache/chrome-sdk/symlinks/sysroot.tar.xz",
			path:   ".",
			target: "../tarballs/sysroot.tar.xz",
			want:   "/b/w/build/cros_cache/chrome-sdk/tarballs/sysroot.tar.xz",
		},
	} {
		got := resolveSymlinkPath(tc.root, tc.path, tc.target)
		if got != tc.want {
			t.Errorf("resolveSymlinkPath(%q,%q,%q)=%q; want=%q", tc.root, tc.path, tc.target, got, tc.want)
		}
	}
}
