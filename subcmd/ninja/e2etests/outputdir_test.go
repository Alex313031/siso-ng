// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package e2etests

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/build/ninjabuild"
	"go.chromium.org/build/siso/hashfs"
)

func TestBuild_OutputDir(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	ctx := t.Context()
	dir := tempDir(t)

	runNinjaTest := func(t *testing.T) (build.Stats, error) {
		t.Helper()
		w, err := os.Create(filepath.Join(dir, "out/siso/siso_explain"))
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			cerr := w.Close()
			if cerr != nil {
				t.Fatal(err)
			}
		}()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:   ".siso_fs_state",
			OutputLocal: func(context.Context, string) bool { return true },
		})
		defer cleanup()
		opt.ExplainWriter = w
		return ninjabuild.Run(ctx, graph, opt, nil, ninjabuild.RunNinjaOpts{})
	}

	t.Logf("-- setup workspace")
	setupFiles(t, dir, t.Name(), nil)

	t.Logf("-- first build")
	_, err := runNinjaTest(t)
	if err != nil {
		t.Fatalf("ninja err: %v", err)
	}

	for _, fname := range []string{
		"out/siso/test.app/Frameworks/frameworks/Foo.h",
		"out/siso/test.app/Frameworks/frameworks/Foo2.h",
		"out/siso/obj/frameworks/foo.framework/Foo.h",
		"out/siso/obj/frameworks/foo.framework/Foo2.h",
	} {
		_, err := os.Stat(filepath.Join(dir, fname))
		if err != nil {
			t.Errorf("stat(%q)=%v; want nil error", fname, err)
		}
	}
	t.Logf("-- confirm no-op")
	stats, err := runNinjaTest(t)
	if err != nil {
		t.Fatalf("ninja err: %v", err)
	}
	if stats.Done != stats.Total || stats.Local != 0 || stats.Skipped != stats.Total {
		t.Errorf("ninja confirm no-op error: done=%d local=%d skipped=%d; want done=%d local=%d skipped=%d", stats.Done, stats.Local, stats.Skipped, stats.Total, 0, stats.Total)
		buf, err := os.ReadFile(filepath.Join(dir, "out/siso/siso_explain"))
		t.Logf("siso_explain: %v\n%s", err, buf)
	}

	t.Logf("-- change archive file")
	err = os.Remove(filepath.Join(dir, "framework/Foo2.h"))
	if err != nil {
		t.Fatalf("remove framework/Foo2.h: %v", err)
	}
	buf, err := os.ReadFile(filepath.Join(dir, "out/siso/build.ninja"))
	if err != nil {
		t.Fatalf("read build.ninja: %v", err)
	}
	buf = bytes.ReplaceAll(buf, []byte(" ../../framework/Foo2.h"), nil)
	err = os.WriteFile(filepath.Join(dir, "out/siso/build.ninja"), buf, 0644)
	if err != nil {
		t.Fatalf("rewrite build.ninja: %v", err)
	}

	t.Logf("-- second build")
	_, err = runNinjaTest(t)
	if err != nil {
		t.Fatalf("ninja err: %v", err)
	}

	for _, fname := range []string{
		"out/siso/test.app/Frameworks/frameworks/Foo.h",
		"out/siso/obj/frameworks/foo.framework/Foo.h",
	} {
		_, err := os.Stat(filepath.Join(dir, fname))
		if err != nil {
			t.Errorf("stat(%q)=%v; want nil error", fname, err)
		}
	}

	for _, fname := range []string{
		"out/siso/test.app/Frameworks/frameworks/Foo2.h",
		"out/siso/obj/frameworks/foo.framework/Foo2.h",
	} {
		_, err := os.Stat(filepath.Join(dir, fname))
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("stat(%q)=%v; want %v", fname, err, fs.ErrNotExist)
		}
	}

	t.Logf("-- confirm no-op")
	stats, err = runNinjaTest(t)
	if err != nil {
		t.Fatalf("ninja err: %v", err)
	}
	if stats.Done != stats.Total || stats.Local != 0 || stats.Skipped != stats.Total {
		t.Errorf("ninja confirm no-op error: done=%d local=%d skipped=%d; want done=%d local=%d skipped=%d", stats.Done, stats.Local, stats.Skipped, stats.Total, 0, stats.Total)
		buf, err := os.ReadFile(filepath.Join(dir, "out/siso/siso_explain"))
		t.Logf("siso_explain: %v\n%s", err, buf)
	}
}

// TestBuild_DirOutput_SymlinkNotFollowed locks in the review concern that a
// symlink inside a directory output, pointing at an external directory, is
// captured as a symlink and never followed. Following it would record
// unrelated external files as members of the tree and make the output depend
// on files outside the action.
func TestBuild_DirOutput_SymlinkNotFollowed(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	if runtime.GOOS == "windows" {
		t.Skip("symlink not available on windows")
	}
	ctx := t.Context()
	dir := tempDir(t)

	runNinjaTest := func(t *testing.T) (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:   ".siso_fs_state",
			OutputLocal: func(context.Context, string) bool { return true },
		})
		defer cleanup()
		return ninjabuild.Run(ctx, graph, opt, nil, ninjabuild.RunNinjaOpts{})
	}

	setupFiles(t, dir, t.Name(), nil)

	stats, err := runNinjaTest(t)
	if err != nil {
		t.Fatalf("first build: %v", err)
	}
	if stats.Done != stats.Total {
		t.Errorf("first build done=%d total=%d", stats.Done, stats.Total)
	}

	// The symlink member is captured as a symlink (lstat), not dereferenced.
	link := filepath.Join(dir, "out/siso/gen/link")
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat gen/link: %v", err)
	}
	if fi.Mode()&fs.ModeSymlink == 0 {
		t.Fatalf("gen/link mode=%v; want a symlink", fi.Mode())
	}
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("readlink gen/link: %v", err)
	}
	if !strings.HasSuffix(target, string(filepath.Separator)+"ext") {
		t.Errorf("gen/link -> %q; want a link to the external ext dir", target)
	}

	// Rebuild is a no-op: the directory output (symlink member included)
	// round-trips through state.
	stats, err = runNinjaTest(t)
	if err != nil {
		t.Fatalf("no-op build: %v", err)
	}
	if stats.Local != 0 {
		t.Errorf("no-op build local=%d; want 0", stats.Local)
	}

	// Changing a file inside the external dir the symlink points to must not
	// dirty the directory output: the symlink's recorded target is unchanged
	// and its target is never walked. If siso followed the symlink, the
	// external file would be tracked as a member and steps would re-run.
	if err := os.WriteFile(filepath.Join(dir, "ext/file.txt"), []byte("CHANGED\n"), 0644); err != nil {
		t.Fatal(err)
	}
	stats, err = runNinjaTest(t)
	if err != nil {
		t.Fatalf("build after external change: %v", err)
	}
	if stats.Local != 0 {
		t.Errorf("build after external change local=%d; want 0 (symlink target was followed into the external dir)", stats.Local)
	}
}
