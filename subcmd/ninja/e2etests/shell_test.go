// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package e2etests

import (
	"runtime"
	"testing"

	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/build/ninjabuild"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/reapi/reapitest"
)

func TestBuild_HermeticShell_Local(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("This test doesn't work on windows")
	}
	if !runInSubProcess(t) {
		return
	}
	ctx := t.Context()
	dir := tempDir(t)

	setupFiles(t, dir, t.Name(), nil)

	runNinja := func(t *testing.T) (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile: ".siso_fs_state",
		})
		defer cleanup()
		return ninjabuild.Run(ctx, graph, opt, nil, ninjabuild.RunNinjaOpts{})
	}

	t.Logf("-- initial build with hermetic shell")
	stats, err := runNinja(t)
	if err != nil {
		t.Fatalf("first build failed: %v", err)
	}
	if stats.Done != 2 || stats.Local != 2 {
		t.Errorf("first build: done=%d local=%d; want done=2 local=2", stats.Done, stats.Local)
	}

	t.Logf("-- second build (should be no-op)")
	stats, err = runNinja(t)
	if err != nil {
		t.Fatalf("second build failed: %v", err)
	}
	if stats.Done != 2 || stats.Skipped != 2 || stats.Local != 0 {
		t.Errorf("second build: done=%d skipped=%d local=%d; want done=2 skipped=2 local=0", stats.Done, stats.Skipped, stats.Local)
	}

	t.Logf("-- touch hermetic shell binary")
	touchFile(t, dir, "bin/sh")

	t.Logf("-- third build (only nontrivial should rerun, trivial should be skipped)")
	stats, err = runNinja(t)
	if err != nil {
		t.Fatalf("third build failed: %v", err)
	}
	if stats.Done != 2 || stats.Skipped != 1 || stats.Local != 1 {
		t.Errorf("third build: done=%d skipped=%d local=%d; want done=2 skipped=1 local=1", stats.Done, stats.Skipped, stats.Local)
	}

	t.Logf("-- touch common input in.txt")
	touchFile(t, dir, "in.txt")

	t.Logf("-- fourth build (both should rerun)")
	stats, err = runNinja(t)
	if err != nil {
		t.Fatalf("fourth build failed: %v", err)
	}
	if stats.Done != 2 || stats.Local != 2 {
		t.Errorf("fourth build: done=%d local=%d; want done=2 local=2", stats.Done, stats.Local)
	}
}

func TestBuild_HermeticShell_Remote(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("This test doesn't work on windows")
	}
	if !runInSubProcess(t) {
		return
	}
	ctx := t.Context()
	dir := tempDir(t)

	setupFiles(t, dir, t.Name(), nil)

	var sawNontrivialShell bool
	var sawNontrivialInCAS bool
	var sawTrivialShell bool

	fakere := &reapitest.Fake{
		ExecuteFunc: func(fakere *reapitest.Fake, action *rpb.Action) (*rpb.ActionResult, error) {
			cmd := &rpb.Command{}
			err := fakere.FetchProto(ctx, action.CommandDigest, cmd)
			if err != nil {
				return nil, err
			}
			tree := reapitest.InputTree{CAS: fakere.CAS, Root: action.InputRootDigest}
			node, err := tree.LookupFileNode(ctx, "bin/sh")

			// Check command arguments
			if len(cmd.Arguments) >= 3 && cmd.Arguments[0] == "../../bin/sh" && cmd.Arguments[1] == "-c" {
				sawNontrivialShell = true
				if err == nil && node != nil {
					sawNontrivialInCAS = true
				}
			} else if len(cmd.Arguments) >= 1 && cmd.Arguments[0] == "cp" {
				if err == nil && node != nil {
					sawTrivialShell = true
				}
			}

			outDigest, err := fakere.Put(ctx, []byte("fake output\n"))
			if err != nil {
				return nil, err
			}

			outPath := "out_trivial"
			if len(cmd.Arguments) >= 3 && cmd.Arguments[0] == "../../bin/sh" {
				outPath = "out_nontrivial"
			}
			return &rpb.ActionResult{
				ExitCode: 0,
				OutputFiles: []*rpb.OutputFile{
					{
						Path:   outPath,
						Digest: outDigest,
					},
				},
			}, nil
		},
	}

	runNinja := func(t *testing.T) (build.Stats, error) {
		t.Helper()
		var ds build.DataSource
		defer func() {
			err := ds.Close(ctx)
			if err != nil {
				t.Error(err)
			}
		}()
		ds.Client = reapitest.New(ctx, t, fakere)
		ds.Cache = ds.Client.CacheStore()

		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:  ".siso_fs_state",
			DataSource: ds,
		})
		defer cleanup()
		opt.REAPIClient = ds.Client
		return ninjabuild.Run(ctx, graph, opt, nil, ninjabuild.RunNinjaOpts{})
	}

	stats, err := runNinja(t)
	if err != nil {
		t.Fatalf("ninja failed: %v", err)
	}
	if stats.Done != 2 || stats.Remote != 2 {
		t.Errorf("stats: done=%d remote=%d; want done=2 remote=2", stats.Done, stats.Remote)
	}

	if !sawNontrivialShell {
		t.Errorf("sawNontrivialShell = false; want nontrivial command to use ../../bin/sh")
	}
	if !sawNontrivialInCAS {
		t.Errorf("sawNontrivialInCAS = false; want nontrivial action to have bin/sh uploaded to CAS")
	}
	if sawTrivialShell {
		t.Errorf("sawTrivialShell = true; want trivial action to NOT have bin/sh uploaded to CAS")
	}
}
