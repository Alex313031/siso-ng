// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package e2etests

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/build/ninjabuild"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/reapi/reapitest"
)

func TestBuild_Depfile_OutputLocalMinimum(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	ctx := t.Context()
	dir := tempDir(t)

	runNinja := func(t *testing.T, ds build.DataSource) (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:   ".siso_fs_state",
			DataSource:  ds,
			OutputLocal: func(context.Context, string) bool { return false }, // minimum
		})
		defer cleanup()
		opt.REAPIClient = ds.Client
		return ninjabuild.Run(ctx, graph, opt, nil, ninjabuild.RunNinjaOpts{})
	}

	var depfileContent = []byte("obj/foo.o: ../../foo.s ../../foo.inc\n")

	setupFiles(t, dir, t.Name(), nil)
	fakere := &reapitest.Fake{
		ExecuteFunc: func(fakere *reapitest.Fake, action *rpb.Action) (*rpb.ActionResult, error) {
			od, err := fakere.Put(ctx, []byte("foo.o content"))
			if err != nil {
				msg := fmt.Sprintf("failed to write obj/foo.o: %v", err)
				t.Log(msg)
				return &rpb.ActionResult{
					ExitCode:  1,
					StderrRaw: []byte(msg),
				}, nil
			}
			dd, err := fakere.Put(ctx, depfileContent)
			if err != nil {
				msg := fmt.Sprintf("failed to write obj/foo.o.d: %v", err)
				t.Log(msg)
				return &rpb.ActionResult{
					ExitCode:  1,
					StderrRaw: []byte(msg),
				}, nil
			}
			return &rpb.ActionResult{
				ExitCode: 0,
				OutputFiles: []*rpb.OutputFile{
					{
						Path:   "obj/foo.o",
						Digest: od,
					},
					{
						Path:   "obj/foo.o.d",
						Digest: dd,
					},
				},
			}, nil
		},
	}
	var ds build.DataSource
	defer func() {
		err := ds.Close(ctx)
		if err != nil {
			t.Error(err)
		}
	}()
	ds.Client = reapitest.New(ctx, t, fakere)
	ds.Cache = ds.Client.CacheStore()

	t.Logf("-- first build")
	stats, err := runNinja(t, ds)
	if err != nil {
		t.Errorf("ninja %v: want nil err", err)
	}
	if stats.Remote != 1 || stats.Done != stats.Total {
		t.Errorf("remote=%d done=%d total=%d; want remote=1 done=total; %#v", stats.Remote, stats.Done, stats.Total, stats)
	}

	buf, err := os.ReadFile(filepath.Join(dir, "out/siso/obj/foo.o.d"))
	if err != nil {
		t.Errorf("obj/foo.o.d not found: %v", err)
	}
	if !bytes.Equal(buf, depfileContent) {
		t.Errorf("wrong obj/foo.o.d content=%q; want=%q", buf, depfileContent)
	}

	t.Logf("-- confirm no-op")
	stats, err = runNinja(t, ds)
	if err != nil {
		t.Errorf("ninja %v; want nil err", err)
	}
	if stats.Skipped != stats.Done || stats.Done != stats.Total {
		t.Errorf("skipped=%d done=%d total=%d; want skipped=done=total; %#v", stats.Skipped, stats.Done, stats.Total, stats)
	}
}

func TestBuild_Depfile_AsOutput(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	ctx := t.Context()
	dir := tempDir(t)

	runNinjaTest := func(t *testing.T) (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile: ".siso_fs_state",
		})
		defer cleanup()
		return ninjabuild.Run(ctx, graph, opt, nil, ninjabuild.RunNinjaOpts{})
	}

	setupFiles(t, dir, t.Name(), nil)
	stats, err := runNinjaTest(t)
	if err != nil {
		t.Errorf("ninja %v; want nil err", err)
	}
	if stats.Done != stats.Total || stats.Local != 1 || stats.Total != 1 {
		t.Errorf("done=%d total=%d local=%d; want done=1 total=1 local=1 %#v", stats.Done, stats.Total, stats.Local, stats)
	}
}

func TestBuild_Depfile_SandboxedRestriction(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	if runtime.GOOS != "linux" {
		t.Skip("skip: no sandbox support on non-linux")
		return
	}
	ctx := t.Context()
	dir := tempDir(t)

	// Initialize workspace from testdata folder.
	setupFiles(t, dir, t.Name(), nil)

	runNinjaTest := func(t *testing.T) (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile: ".siso_fs_state",
		})
		defer cleanup()
		return ninjabuild.Run(ctx, graph, opt, nil, ninjabuild.RunNinjaOpts{})
	}

	t.Logf("-- attempting to build with sandboxing restriction active")
	stats, err := runNinjaTest(t)

	expectedErr := build.DepfileAddsUnsandboxedFileError{Inputs: []string{"../../undeclared.h"}}
	if !errors.Is(err, expectedErr) {
		t.Errorf("got error %v, want %v", err, expectedErr)
	}

	if stats.Done != 2 || stats.Local != 1 || stats.Total != 2 {
		t.Errorf("done=%d total=%d local=%d; want done=2 total=2 local=1 %#v", stats.Done, stats.Total, stats.Local, stats)
	}
}

func TestBuild_Depfile_SandboxedRestriction_FirstRunWithoutSandboxing(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	if runtime.GOOS != "linux" {
		t.Skip("skip: no sandbox support on non-linux")
		return
	}
	ctx := t.Context()
	dir := tempDir(t)

	// Initialize workspace from testdata folder.
	setupFiles(t, dir, t.Name(), nil)

	runNinjaTest := func(t *testing.T) (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile: ".siso_fs_state",
		})
		defer cleanup()
		return ninjabuild.Run(ctx, graph, opt, nil, ninjabuild.RunNinjaOpts{})
	}

	t.Logf("-- attempting to build without sandboxing restriction active")
	stats, err := runNinjaTest(t)

	if err != nil {
		t.Fatalf("got %v, want nil err", err)
	}
	if stats.Done != 2 || stats.Local != 1 || stats.Total != 2 {
		t.Errorf("done=%d total=%d local=%d; want done=2 total=2 local=1 %#v", stats.Done, stats.Total, stats.Local, stats)
	}

	modifyFile(t, filepath.Join(dir, "out/siso"), "build.ninja", func(b []byte) []byte {
		return bytes.ReplaceAll(b, []byte("sandbox_disabled = true"), nil)
	})

	t.Logf("-- attempting to build with sandboxing restriction active")
	stats, err = runNinjaTest(t)

	expectedErr := build.DepfileAddsUnsandboxedFileError{Inputs: []string{"../../undeclared.h"}}
	if !errors.Is(err, expectedErr) {
		t.Errorf("got error %v, want %v", err, expectedErr)
	}
	if stats.Done != 2 || stats.Local != 1 || stats.Total != 2 {
		t.Errorf("done=%d total=%d local=%d; want done=2 total=2 local=1 %#v", stats.Done, stats.Total, stats.Local, stats)
	}
}

func TestBuild_Depfile_SandboxedRestriction_Multiple(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	if runtime.GOOS != "linux" {
		t.Skip("skip: no sandbox support on non-linux")
		return
	}
	ctx := t.Context()
	dir := tempDir(t)

	// Initialize workspace from testdata folder.
	setupFiles(t, dir, t.Name(), nil)

	runNinjaTest := func(t *testing.T) (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile: ".siso_fs_state",
		})
		defer cleanup()
		return ninjabuild.Run(ctx, graph, opt, nil, ninjabuild.RunNinjaOpts{})
	}

	t.Logf("-- attempting to build with sandboxing restriction active")
	stats, err := runNinjaTest(t)

	expectedErr := build.DepfileAddsUnsandboxedFileError{Inputs: []string{"../../undeclared.h", "../../undeclared2.h"}}
	if !errors.Is(err, expectedErr) {
		t.Errorf("got error %v, want %v", err, expectedErr)
	}

	if stats.Done != 2 || stats.Local != 1 || stats.Total != 2 {
		t.Errorf("done=%d total=%d local=%d; want done=2 total=2 local=1 %#v", stats.Done, stats.Total, stats.Local, stats)
	}
}
