// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	pb "github.com/bazelbuild/reclient/api/proxy"
	cpb "github.com/bazelbuild/remote-apis-sdks/go/api/command"
	"github.com/google/go-cmp/cmp"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/execute/reproxyexec/reproxytest"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/reapi"
	"go.chromium.org/build/siso/toolsupport/ninjautil"
)

func TestBuild_DepsMSVC(t *testing.T) {
	ctx := t.Context()
	dir := tempDir(t)

	func() {
		t.Logf("first build")
		setupFiles(t, dir, t.Name(), nil)
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{})
		defer cleanup()

		b, err := build.New(ctx, graph, opt)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			err := b.Close()
			if err != nil {
				t.Fatalf("b.Close()=%v; want nil err", err)
			}
		}()
		err = b.Build(ctx, "build", "all")
		if err != nil {
			t.Fatalf(`b.Build(ctx, "build", "all")=%v; want nil err`, err)
		}
	}()

	func() {
		t.Logf("first_check_deps")
		depsLog, cleanup := openDepsLog(ctx, t, dir)
		defer cleanup()
		deps, mtime, err := depsLog.RetrievePaths(ctx, "foo.o")
		if err != nil {
			t.Fatalf(`depsLog.RetrievePaths(ctx, "foo.o")=%v, %v, %v; want nil err`, deps, mtime, err)
		}
		want := []string{
			"../../base/foo.h",
			"../../base/other.h",
			"../../base/foo.cc",
		}
		if diff := cmp.Diff(want, deps); diff != "" {
			t.Errorf("deps for foo.o: diff -want +got:\n%s", diff)
		}
	}()

	func() {
		t.Logf("second build")
		setupFiles(t, dir, t.Name()+"_second", []string{"base/other.h"})
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{})
		defer cleanup()

		b, err := build.New(ctx, graph, opt)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			err := b.Close()
			if err != nil {
				t.Fatalf("b.Close()=%v; want nil err", err)
			}
		}()
		err = b.Build(ctx, "build", "all")
		if err != nil {
			t.Fatalf(`b.Build(ctx, "build", "all")=%v; want nil err`, err)
		}
	}()

	func() {
		t.Logf("second_check_deps")
		depsLog, cleanup := openDepsLog(ctx, t, dir)
		defer cleanup()
		deps, mtime, err := depsLog.RetrievePaths(ctx, "foo.o")
		if err != nil {
			t.Fatalf(`depsLog.RetrievePaths(ctx, "foo.o")=%v, %v, %v; want nil err`, deps, mtime, err)
		}
		want := []string{
			"../../base/foo.h",
			"../../other/other.h",
			"../../base/foo.cc",
		}
		if diff := cmp.Diff(want, deps); diff != "" {
			t.Errorf("deps for foo.o: diff -want +got:\n%s", diff)
		}
	}()
}

func TestBuild_DepsMSVC_Reproxy(t *testing.T) {
	ctx := t.Context()
	dir := tempDir(t)
	func() {
		t.Logf("first build")
		setupFiles(t, dir, t.Name(), nil)
		s := reproxytest.NewServer(ctx, t, &reproxytest.Fake{
			RunCommandFunc: func(ctx context.Context, req *pb.RunRequest) (*pb.RunResponse, error) {
				err := os.WriteFile(filepath.Join(dir, "out/siso/foo.o"), nil, 0644)
				if err != nil {
					return &pb.RunResponse{
						Stderr: []byte(err.Error()),
						Result: &cpb.CommandResult{
							Status:   cpb.CommandResultStatus_LOCAL_ERROR,
							ExitCode: 1,
						},
					}, nil
				}
				return &pb.RunResponse{
					Result: &cpb.CommandResult{
						Status: cpb.CommandResultStatus_SUCCESS,
					},
					Stdout: []byte(`
Note: including file: ../../base/foo.h
Note: including file:   ../../base/other.h
`),
				}, nil
			},
		})
		defer s.Close()

		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{})
		defer cleanup()
		opt.ReproxyAddr = s.Addr()

		b, err := build.New(ctx, graph, opt)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			err := b.Close()
			if err != nil {
				t.Fatalf("b.Close()=%v; want nil err", err)
			}
		}()
		err = b.Build(ctx, "build", "all")
		if err != nil {
			t.Fatalf(`b.Build(ctx, "build", "all")=%v; want nil err`, err)
		}
	}()

	func() {
		t.Logf("first_check_deps")
		depsLog, cleanup := openDepsLog(ctx, t, dir)
		defer cleanup()
		deps, mtime, err := depsLog.RetrievePaths(ctx, "foo.o")
		if err != nil {
			t.Fatalf(`depsLog.RetrievePaths(ctx, "foo.o")=%v, %v, %v; want nil err`, deps, mtime, err)
		}
		want := []string{
			"../../base/foo.h",
			"../../base/other.h",
			"../../base/foo.cc",
		}
		if diff := cmp.Diff(want, deps); diff != "" {
			t.Errorf("deps for foo.o: diff -want +got:\n%s", diff)
		}
	}()

	func() {
		t.Logf("second build")
		setupFiles(t, dir, t.Name()+"_second", []string{"base/other.h"})
		s := reproxytest.NewServer(ctx, t, &reproxytest.Fake{
			RunCommandFunc: func(ctx context.Context, req *pb.RunRequest) (*pb.RunResponse, error) {
				err := os.WriteFile(filepath.Join(dir, "out/siso/foo.o"), nil, 0644)
				if err != nil {
					return &pb.RunResponse{
						Stderr: []byte(err.Error()),
						Result: &cpb.CommandResult{
							Status:   cpb.CommandResultStatus_LOCAL_ERROR,
							ExitCode: 1,
						},
					}, nil
				}
				return &pb.RunResponse{
					Result: &cpb.CommandResult{
						Status: cpb.CommandResultStatus_SUCCESS,
					},
					Stdout: []byte(`
Note: including file: ../../base/foo.h
Note: including file:   ../../base/other2.h
`),
				}, nil
			},
		})
		defer s.Close()

		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{})
		defer cleanup()
		opt.ReproxyAddr = s.Addr()

		b, err := build.New(ctx, graph, opt)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			err := b.Close()
			if err != nil {
				t.Fatalf("b.Close()=%v; want nil err", err)
			}
		}()
		err = b.Build(ctx, "build", "all")
		if err != nil {
			t.Fatalf(`b.Build(ctx, "build", "all")=%v; want nil err`, err)
		}
	}()

	func() {
		t.Logf("second_check_deps")
		depsLog, cleanup := openDepsLog(ctx, t, dir)
		defer cleanup()
		deps, mtime, err := depsLog.RetrievePaths(ctx, "foo.o")
		if err != nil {
			t.Fatalf(`depsLog.RetrievePaths(ctx, "foo.o")=%v, %v, %v; want nil err`, deps, mtime, err)
		}
		want := []string{
			"../../base/foo.h",
			"../../base/other2.h",
			"../../base/foo.cc",
		}
		if diff := cmp.Diff(want, deps); diff != "" {
			t.Errorf("deps for foo.o: diff -want +got:\n%s", diff)
		}
	}()
}

func TestBuild_DepsMSVC_fastlocal(t *testing.T) {
	ctx := t.Context()
	dir := tempDir(t)

	limits := build.DefaultLimits(ctx)
	testLimits := limits
	testLimits.FastLocal = 1
	build.SetDefaultForTest(testLimits)
	defer build.SetDefaultForTest(limits)

	func() {
		t.Logf("first build")
		setupFiles(t, dir, t.Name(), nil)
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{})
		defer cleanup()
		opt.REAPIClient = &reapi.Client{}
		opt.Limits = testLimits

		b, err := build.New(ctx, graph, opt)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			err := b.Close()
			if err != nil {
				t.Fatalf("b.Close()=%v; want nil err", err)
			}
		}()
		err = b.Build(ctx, "build", "all")
		if err != nil {
			t.Fatalf(`b.Build(ctx, "build", "all")=%v; want nil err`, err)
		}
	}()

	func() {
		t.Logf("first_check_deps")
		depsLog, cleanup := openDepsLog(ctx, t, dir)
		defer cleanup()
		deps, mtime, err := depsLog.RetrievePaths(ctx, "foo.o")
		if err != nil {
			t.Fatalf(`depsLog.RetrievePaths(ctx, "foo.o")=%v, %v, %v; want nil err`, deps, mtime, err)
		}
		want := []string{
			"../../base/foo.h",
			"../../base/other.h",
			"../../base/foo.cc",
		}
		if diff := cmp.Diff(want, deps); diff != "" {
			t.Errorf("deps for foo.o: diff -want +got:\n%s", diff)
		}
	}()

	func() {
		t.Logf("second build")
		setupFiles(t, dir, t.Name()+"_second", []string{"base/other.h"})
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{})
		defer cleanup()
		opt.REAPIClient = &reapi.Client{}
		opt.Limits = testLimits

		b, err := build.New(ctx, graph, opt)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			err := b.Close()
			if err != nil {
				t.Fatalf("b.Close()=%v; want nil err", err)
			}
		}()
		err = b.Build(ctx, "build", "all")
		if err != nil {
			t.Fatalf(`b.Build(ctx, "build", "all")=%v; want nil err`, err)
		}
	}()

	func() {
		t.Logf("second_check_deps")
		depsLog, cleanup := openDepsLog(ctx, t, dir)
		defer cleanup()
		deps, mtime, err := depsLog.RetrievePaths(ctx, "foo.o")
		if err != nil {
			t.Fatalf(`depsLog.RetrievePaths(ctx, "foo.o")=%v, %v, %v; want nil err`, deps, mtime, err)
		}
		want := []string{
			"../../base/foo.h",
			"../../other/other.h",
			"../../base/foo.cc",
		}
		if diff := cmp.Diff(want, deps); diff != "" {
			t.Errorf("deps for foo.o: diff -want +got:\n%s", diff)
		}
	}()
}

// regression test for b/322270122
func TestBuild_DepsMSVC_InstallerRC(t *testing.T) {
	ctx := t.Context()
	dir := tempDir(t)

	ninja := func(t *testing.T, dryRun bool) (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile: ".siso_fs_state",
		})
		defer cleanup()
		opt.DryRun = dryRun
		return runNinja(ctx, "build.ninja", graph, opt, nil, runNinjaOpts{})
	}

	deps := func(t *testing.T, output string) []string {
		t.Helper()
		depsLog, err := ninjautil.NewDepsLog(ctx, filepath.Join(dir, "out/siso/.siso_deps"))
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := depsLog.Close(); err != nil {
				t.Errorf("depsLog.Close=%v", err)
			}
		}()
		deps, _, err := depsLog.RetrievePaths(ctx, output)
		if err != nil {
			t.Fatalf("deps %s: %v", output, err)
		}
		return deps
	}
	checkFSState := func(t *testing.T, fname string) bool {
		t.Helper()
		st, err := hashfs.Load(ctx, hashfs.Option{StateFile: filepath.Join(dir, "out/siso/.siso_fs_state")})
		if err != nil {
			t.Fatal(err)
		}
		m := hashfs.StateMap(st)
		_, ok := m[filepath.ToSlash(filepath.Join(dir, fname))]
		return ok
	}

	setupFiles(t, dir, t.Name(), nil)
	t.Logf("first build")
	stats, err := ninja(t, false)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Done != 6 || stats.Local != 5 {
		t.Errorf("done=%d local=%d; want done=5 local=4", stats.Done, stats.Local)
	}
	got := deps(t, "gen/installer/packed_files.res")
	want := []string{
		filepath.ToSlash(filepath.Join(dir, "out/siso/gen/installer/foo/base.dll")),
		filepath.ToSlash(filepath.Join(dir, "out/siso/gen/installer/bar/base.dll")),
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("deps gen/installer/packed_files.res -want +got:\n%s", diff)
	}
	if _, err := os.Lstat(filepath.Join(dir, "out/siso/gen/installer/foo/base.dll")); err != nil {
		t.Errorf("stat(out/siso/gen/installer/foo/base.dll)=%v", err)
	}

	t.Logf("second build to make sure foo/base.dll captured by siso")
	stats, err = ninja(t, false)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Done != stats.Total || stats.Local != 0 || stats.Skipped != stats.Total {
		t.Errorf("done=%d local=%d skip=%d; want done=%d local=0 skip=%d", stats.Done, stats.Local, stats.Skipped, stats.Total, stats.Total)
	}
	if _, err := os.Lstat(filepath.Join(dir, "out/siso/gen/installer/foo/base.dll")); err != nil {
		t.Errorf("stat(out/siso/gen/installer/foo/base.dll)=%v", err)
	}
	if !checkFSState(t, "out/siso/gen/installer/foo/base.dll") {
		t.Errorf("out/siso/gen/installer/foo/base.dll not in .siso_fs_state")
	}

	t.Logf("change build to generate different temp files")
	err = os.Rename(filepath.Join(dir, "out/siso/build.ninja"), filepath.Join(dir, "out/siso/build.ninja.old"))
	if err != nil {
		t.Fatal(err)
	}
	err = os.Rename(filepath.Join(dir, "out/siso/build.ninja.new"), filepath.Join(dir, "out/siso/build.ninja"))
	if err != nil {
		t.Fatal(err)
	}
	// foo/base.dll disappears by create_installer_archive.py

	t.Logf("third build")
	stats, err = ninja(t, false)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Done != 5 || stats.Local != 3 {
		t.Errorf("done=%d local=%d; want done=5 local=3", stats.Done, stats.Local)
	}

	got = deps(t, "gen/installer/packed_files.res")
	// deps for foo/base.dll should be disappeared
	want = []string{
		filepath.ToSlash(filepath.Join(dir, "out/siso/gen/installer/bar/base.dll")),
		"gen/installer/bar/base.dll",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("deps gen/installer/packed_files.res -want +got:\n%s", diff)
	}
	if _, err := os.Lstat(filepath.Join(dir, "out/siso/gen/installer/foo/base.dll")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stat(out/siso/gen/installer/foo/base.dll)=%v", err)
	}
	if checkFSState(t, "out/siso/gen/installer/foo/base.dll") {
		t.Errorf("out/siso/gen/installer/foo/base.dll should be removed from .siso_fs_state")
	}

	t.Logf("confirm no-op")
	stats, err = ninja(t, true)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Done != stats.Total || stats.Local != 0 || stats.Skipped != stats.Total {
		t.Errorf("done=%d local=%d skip=%d; want done=%d local=0 skip=%d", stats.Done, stats.Local, stats.Skipped, stats.Total, stats.Total)
	}
}
