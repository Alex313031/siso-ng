// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package e2etests

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/build/ninjabuild"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/reapi/digest"
	"go.chromium.org/build/siso/reapi/reapitest"
)

func TestBuild_RemovedArtifact(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
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
	if !runInSubProcess(t) {
		return
	}
	ctx := t.Context()
	dir := tempDir(t)

	runNinjaTest := func(t *testing.T, refake *reapitest.Fake) (build.Stats, error) {
		t.Helper()
		var ds build.DataSource
		defer func() {
			err := ds.Close(ctx)
			if err != nil {
				t.Error(err)
			}
		}()
		ds.Client = reapitest.New(ctx, t, refake)
		ds.Cache = ds.Client.CacheStore()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:   ".siso_fs_state",
			DataSource:  ds,
			OutputLocal: func(context.Context, string) bool { return false },
		})
		defer cleanup()
		opt.REAPIClient = ds.Client
		return ninjabuild.Run(ctx, graph, opt, nil, ninjabuild.RunNinjaOpts{})
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
	stats, err := runNinjaTest(t, fakere)
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
	stats, err = runNinjaTest(t, fakere)
	if err != nil {
		t.Fatalf("ninja %v: want nil err", err)
	}
	if stats.Done != stats.Total || stats.Total != 2 || stats.Remote != 0 || stats.Local != 1 || stats.Skipped != 1 {
		t.Errorf("done=%d total=%d remote=%d local=%d skipped=%d; done=total=2 remote=0 local=1 skipped=1; %#v", stats.Done, stats.Total, stats.Remote, stats.Local, stats.Skipped, stats)
	}
}

// TestBuild_RemovedArtifactRacing checks that a racing build self-heals
// when an output recorded as local-ready in .siso_fs_state has been
// removed from disk behind siso's back (e.g. by a pre-build cleanup
// step) and the remote racer wins before the local racer starts the
// command. b/522434556
//
// The build graph has a slow local-only step (slow.out) that occupies
// the single local semaphore slot, so the racing step's local racer
// is still waiting for the semaphore when the remote racer wins and
// must not rely on the stale local-ready hashfs entry for remote.out.
func TestBuild_RemovedArtifactRacing(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	ctx := t.Context()
	dir := tempDir(t)

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

	var ds build.DataSource
	defer func() {
		err := ds.Close(ctx)
		if err != nil {
			t.Error(err)
		}
	}()
	ds.Client = reapitest.New(ctx, t, fakere)
	err := ds.Client.Init(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ds.Cache = ds.Client.CacheStore()

	// runBuild runs a full build of "all". prepare, if set, runs after
	// setupBuild loaded .siso_fs_state but before the build starts, to
	// simulate state changing behind siso's back.
	runBuild := func(t *testing.T, prepare func()) (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:   ".siso_fs_state",
			DataSource:  ds,
			OutputLocal: func(context.Context, string) bool { return true },
		})
		defer cleanup()
		bcache, err := build.NewCache(ctx, build.CacheOptions{
			Store:      ds.Cache,
			EnableRead: true,
		})
		if err != nil {
			return build.Stats{}, err
		}
		opt.Cache = bcache
		opt.RECacheEnableRead = true
		opt.RECacheEnableWrite = true
		opt.REAPIClient = ds.Client
		opt.REExecEnable = true
		opt.FailuresAllowed = 0
		// One local slot, so slow.out starves the racing step's local racer.
		opt.Limits.Local = 1
		if prepare != nil {
			prepare()
		}
		return ninjabuild.Run(ctx, graph, opt, []string{"all"}, ninjabuild.RunNinjaOpts{})
	}

	setupFiles(t, dir, t.Name(), nil)

	t.Logf("-- first build")
	stats, err := runBuild(t, nil)
	if err != nil {
		t.Fatalf("first build %v: want nil err", err)
	}
	if stats.Done != stats.Total || stats.Total != 4 {
		t.Errorf("done=%d total=%d; want done=total=4; %#v", stats.Done, stats.Total, stats)
	}

	build.SetExperimentForTest("racing")
	defer build.SetExperimentForTest("")

	t.Logf("-- second build with racing + remote.out removed behind siso's back")
	// Touch foo.txt so remote.out and slow.out become dirty.
	touchFile(t, dir, "foo.txt")
	stats, err = runBuild(t, func() {
		// Remove remote.out after hashfs.SetState loaded .siso_fs_state,
		// so its hashfs entry stays local-ready while the file is gone.
		err := os.Remove(filepath.Join(dir, "out/siso/remote.out"))
		if err != nil {
			t.Fatal(err)
		}
	})
	if err != nil {
		t.Fatalf("second build %v: want nil err; %#v", err, stats)
	}
	t.Logf("second build stats: %#v", stats)
	for _, out := range []string{"remote.out", "slow.out", "out"} {
		_, err = os.Stat(filepath.Join(dir, "out/siso", out))
		if err != nil {
			t.Errorf("missing %s after racing build: %v", out, err)
		}
	}

	t.Logf("-- third build. expect null build")
	stats, err = runBuild(t, nil)
	if err != nil {
		t.Fatalf("third build %v: want nil err", err)
	}
	if stats.Skipped != stats.Total || stats.Total != 4 {
		t.Errorf("skipped=%d total=%d; want skipped=total=4; %#v", stats.Skipped, stats.Total, stats)
	}
}

// TestBuild_RemovedArtifactRestatContent checks that a non-racing remote
// execution self-heals when an output recorded as local-ready in
// .siso_fs_state has been removed from disk behind siso's back (e.g. by
// a pre-build cleanup step) and the step has restat_content with
// content-identical outputs.
//
// In that case computeOutputEntries preserves the previous mtime, so
// hashfs keeps the stale local-ready entry with mtimeUpdated=false and
// Flush silently does nothing: the step succeeds without the output
// ever being re-materialized on disk. Unlike the default path, no
// chtimes failure (and thus no local fallback) ever fires. b/522434556
func TestBuild_RemovedArtifactRestatContent(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	ctx := t.Context()
	dir := tempDir(t)

	// Same content as tools/action.py writes; must be non-empty since
	// restat_content treats empty outputs as always changed.
	content := []byte("hello\n")
	fakere := &reapitest.Fake{
		ExecuteFunc: func(fakere *reapitest.Fake, action *rpb.Action) (*rpb.ActionResult, error) {
			dg, err := fakere.Put(ctx, content)
			if err != nil {
				return nil, err
			}
			return &rpb.ActionResult{
				ExitCode: 0,
				OutputFiles: []*rpb.OutputFile{
					{
						Path:   "remote.out",
						Digest: dg,
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
	err := ds.Client.Init(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ds.Cache = ds.Client.CacheStore()

	// runBuild runs a full build of "all". prepare, if set, runs after
	// setupBuild loaded .siso_fs_state but before the build starts, to
	// simulate state changing behind siso's back. Cache read stays
	// disabled so the step deterministically goes through execRemote
	// and its RecordPreOutputs/restat_content handling.
	runBuild := func(t *testing.T, prepare func()) (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:   ".siso_fs_state",
			DataSource:  ds,
			OutputLocal: func(context.Context, string) bool { return true },
		})
		defer cleanup()
		opt.REAPIClient = ds.Client
		opt.REExecEnable = true
		opt.FailuresAllowed = 0
		if prepare != nil {
			prepare()
		}
		return ninjabuild.Run(ctx, graph, opt, []string{"all"}, ninjabuild.RunNinjaOpts{})
	}

	setupFiles(t, dir, t.Name(), nil)

	t.Logf("-- first build")
	stats, err := runBuild(t, nil)
	if err != nil {
		t.Fatalf("first build %v: want nil err", err)
	}
	if stats.Done != stats.Total || stats.Total != 3 {
		t.Errorf("done=%d total=%d; want done=total=3; %#v", stats.Done, stats.Total, stats)
	}

	t.Logf("-- second build with remote.out removed behind siso's back")
	// Touch foo.txt so remote.out becomes dirty but its content (and
	// therefore the restat_content digest) stays the same.
	touchFile(t, dir, "foo.txt")
	stats, err = runBuild(t, func() {
		// Remove remote.out after hashfs.SetState loaded .siso_fs_state,
		// so its hashfs entry stays local-ready while the file is gone.
		err := os.Remove(filepath.Join(dir, "out/siso/remote.out"))
		if err != nil {
			t.Fatal(err)
		}
	})
	if err != nil {
		t.Fatalf("second build %v: want nil err; %#v", err, stats)
	}
	t.Logf("second build stats: %#v", stats)
	for _, out := range []string{"remote.out", "out"} {
		_, err = os.Stat(filepath.Join(dir, "out/siso", out))
		if err != nil {
			t.Errorf("missing %s after build: %v", out, err)
		}
	}
}
