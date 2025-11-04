// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package hashfs

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"go.chromium.org/build/siso/hashfs/osfs"
)

func TestDirectoryLookup_Symlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no symlink on windows")
		return
	}
	ctx := t.Context()
	dir := t.TempDir()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}

	setupFile := func(fname string) {
		t.Helper()
		fname = filepath.Join(dir, fname)
		err := os.MkdirAll(filepath.Dir(fname), 0755)
		if err != nil {
			t.Fatal(err)
		}
		err = os.WriteFile(fname, nil, 0644)
		if err != nil {
			t.Fatal(err)
		}
	}
	setupSymlink := func(fname, target string) {
		t.Helper()
		fname = filepath.Join(dir, fname)
		err := os.MkdirAll(filepath.Dir(fname), 0755)
		if err != nil {
			t.Fatal(err)
		}
		err = os.Symlink(target, fname)
		if err != nil {
			t.Fatal(err)
		}
	}

	fileName := "build/mac_files/xcode_binaries/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX.sdk/somefile"
	setupFile(fileName)
	symlinkName := filepath.Join(filepath.Dir(filepath.Dir(fileName)), "MacOSX13.3.sdk")
	setupSymlink(symlinkName, "MacOSX.sdk")

	d := &directory{}
	osfs := osfs.New(ctx, "fs", osfs.Option{})

	fname := filepath.Join(dir, symlinkName)
	_, _, _, ok := d.lookup(ctx, fname)
	if ok {
		t.Fatalf("d.lookup(ctx, %q): %t; want false", fname, ok)
	}
	e := newLocalEntry()
	e.init(ctx, fname, nil, osfs)
	_, err = d.store(ctx, fname, e)
	if err != nil {
		t.Fatalf("d.store(ctx, %q) %v; want nil err", fname, err)
	}

	fname = filepath.Join(dir, fileName)
	_, _, _, ok = d.lookup(ctx, fname)
	if ok {
		t.Fatalf("d.lookup(ctx, %q): %t; want false", fname, ok)
	}
	e = newLocalEntry()
	e.init(ctx, fname, nil, osfs)
	_, err = d.store(ctx, fname, e)
	if err != nil {
		t.Fatalf("d.store(ctx, %q) %v; want nil err", fname, err)
	}

	t.Log(fname)
	_, _, _, ok = d.lookup(ctx, fname)
	if !ok {
		t.Fatalf("d.lookup(ctx, %q) %t; want true", fname, ok)
	}
	fname = filepath.Dir(fname)
	t.Log(fname)
	_, _, _, ok = d.lookup(ctx, fname)
	if !ok {
		t.Fatalf("d.lookup(ctx, %q) %t; want true", fname, ok)
	}

	fname = filepath.Join(dir, symlinkName)
	t.Log(fname)
	_, _, _, ok = d.lookup(ctx, fname)
	if !ok {
		t.Fatalf("d.lookup(ctx, %q) %t; want true", fname, ok)
	}
	fname = filepath.Join(fname, "somefile")
	t.Log(fname)
	_, _, _, ok = d.lookup(ctx, fname)
	if !ok {
		t.Fatalf("d.lookup(ctx, %q) %t; want true", fname, ok)
	}
}
