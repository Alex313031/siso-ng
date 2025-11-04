// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/reapi/digest"
	"go.chromium.org/build/siso/reapi/reapitest"
)

func TestBuild_RemovedArtifact(t *testing.T) {
	ctx := t.Context()
	dir := tempDir(t)

	func() {
		t.Logf("first build")
		setupFiles(t, dir, t.Name(), nil)

		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:   ".siso_fs_state",
			OutputLocal: func(context.Context, string) bool { return true },
		})
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
		_, err = os.Stat(filepath.Join(dir, "out/siso/output1"))
		if err != nil {
			t.Errorf("build failed for output1?: %v", err)
		}
		_, err = os.Stat(filepath.Join(dir, "out/siso/output2"))
		if err != nil {
			t.Errorf("build failed for output2?: %v", err)
		}
	}()

	t.Logf("remove out/siso/output1 on the disk")
	err := os.Remove(filepath.Join(dir, "out/siso/output1"))
	if err != nil {
		t.Fatal(err)
	}

	func() {
		t.Logf("second build")
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:   ".siso_fs_state",
			OutputLocal: func(context.Context, string) bool { return true },
		})
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
		_, err = os.Stat(filepath.Join(dir, "out/siso/output1"))
		if err != nil {
			t.Errorf("build failed for output1?: %v", err)
		}
		_, err = os.Stat(filepath.Join(dir, "out/siso/output2"))
		if err != nil {
			t.Errorf("build failed for output2?: %v", err)
		}
	}()
}

func TestBuild_RemovedArtifactOutputLocalMinimum(t *testing.T) {
	ctx := t.Context()
	dir := tempDir(t)

	ninja := func(t *testing.T, refake *reapitest.Fake) (build.Stats, error) {
		t.Helper()
		var ds dataSource
		defer func() {
			err := ds.Close(ctx)
			if err != nil {
				t.Error(err)
			}
		}()
		ds.client = reapitest.New(ctx, t, refake)
		ds.cache = ds.client.CacheStore()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:   ".siso_fs_state",
			DataSource:  ds,
			OutputLocal: func(context.Context, string) bool { return false },
		})
		defer cleanup()
		opt.REAPIClient = ds.client
		return runNinja(ctx, "out", graph, opt, nil, runNinjaOpts{})
	}
	setupFiles(t, dir, t.Name(), nil)
	fakere := &reapitest.Fake{
		ExecuteFunc: func(fakere *reapitest.Fake, action *rpb.Action) (*rpb.ActionResult, error) {
			return &rpb.ActionResult{
				ExitCode: 0,
				OutputFiles: []*rpb.OutputFile{
					{
						Path:   "remote.out",
						Digest: digest.Empty.Proto(),
					},
				},
			}, nil
		},
	}

	t.Logf("-- first build")
	stats, err := ninja(t, fakere)
	if err != nil {
		t.Fatalf("ninja %v: want nil err", err)
	}
	if stats.Done != stats.Total || stats.Total != 2 || stats.Remote != 1 || stats.Local != 1 || stats.Skipped != 0 {
		t.Errorf("done=%d total=%d remote=%d local=%d skipped=%d; done=total=2 remote=1 local=1 skipped=0; %#v", stats.Done, stats.Total, stats.Remote, stats.Local, stats.Skipped, stats)
	}

	t.Logf("-- remove remote.out and out")
	err = os.Remove(filepath.Join(dir, "out/siso/remote.out"))
	if err != nil {
		t.Errorf("remove remote.out: %v", err)
	}
	err = os.Remove(filepath.Join(dir, "out/siso/out"))
	if err != nil {
		t.Errorf("remove out: %v", err)
	}
	t.Logf("-- second build. expect skip remote (remote.out), but run local (out)")
	stats, err = ninja(t, fakere)
	if err != nil {
		t.Fatalf("ninja %v: want nil err", err)
	}
	if stats.Done != stats.Total || stats.Total != 2 || stats.Remote != 0 || stats.Local != 1 || stats.Skipped != 1 {
		t.Errorf("done=%d total=%d remote=%d local=%d skipped=%d; done=total=2 remote=0 local=1 skipped=1; %#v", stats.Done, stats.Total, stats.Remote, stats.Local, stats.Skipped, stats)
	}
}
