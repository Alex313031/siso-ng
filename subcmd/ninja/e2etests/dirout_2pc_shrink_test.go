// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package e2etests

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/build/ninjabuild"
	"go.chromium.org/build/siso/execute/localexec"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/reapi"
	"go.chromium.org/build/siso/reapi/reapitest"
)

// TestBuild_TwoPhaseCaching_DirOutputShrinkPruned proves that a two phase
// cache hit removes the members of a directory output that the cached tree
// doesn't have. This is a catchup build: the local step gendir produces gen/,
// and a later build hits the cache with a smaller gen/ while the previous
// build's bigger tree is still on disk. Local execution wipes gen/ before it
// runs (resetDirOutputs), but a cache hit returns before that, so without
// pruning the stale member stays on disk and every consumer of gen/ sees it.
//
// Builds (all in one workspace, sharing one cache):
//  1. names={a,b}: 2PC miss, runs locally, writes the {a,b} entry.
//  2. names={a}: 2PC miss, runs locally, writes the {a} entry.
//  3. names={a,b}: 2PC hit, gen/ = {a,b} again.
//  4. names={a}: 2PC hit; gen/b from build 3 must be gone.
func TestBuild_TwoPhaseCaching_DirOutputShrinkPruned(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("two phase caching is available on linux only")
	}
	if !runInSubProcess(t) {
		return
	}
	requirePython3(t)
	ctx := t.Context()
	dir := tempDir(t)
	t.Setenv("TMPDIR", tempDir(t))
	t.Setenv("TMP", tempDir(t))

	build.SetExperimentForTest("two-phase-caching")

	fakere := &reapitest.Fake{}
	var ds build.DataSource
	defer func() {
		err := ds.Close(ctx)
		if err != nil {
			t.Error(err)
		}
	}()
	localCache, err := reapi.NewLocalCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ds.Client = reapitest.NewWithOption(ctx, t, fakere, reapi.Option{
		LocalCache:                    localCache,
		DisableTwoPhaseCachingMethods: true,
	})
	err = ds.Client.Init(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ds.Cache = ds.Client.CacheStore()

	setupFiles(t, dir, t.Name(), nil)
	origHelper := localexec.SetSpawnHelper(mockTapHelper{})
	defer localexec.SetSpawnHelper(origHelper)

	gen := filepath.Join(dir, "out/siso/gen")

	// run builds with names set to members, and returns whether gendir
	// was a two phase cache hit.
	run := func(t *testing.T, members string) bool {
		t.Helper()
		modifyFile(t, dir, "base/names", func([]byte) []byte { return []byte(members) })
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:   ".siso_fs_state",
			OutputLocal: func(context.Context, string) bool { return true },
			DataSource:  ds,
		})
		defer cleanup()
		bcache, err := build.NewCache(ctx, build.CacheOptions{
			Store:      ds.Cache,
			EnableRead: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		var metrics syncBuffer
		opt.Cache = bcache
		opt.RECacheEnableRead = true
		opt.RECacheEnableWrite = true
		opt.REAPIClient = ds.Client
		opt.REExecEnable = true
		opt.MetricsJSONWriter = &metrics
		if _, err := ninjabuild.Run(ctx, graph, opt, []string{"all"}, ninjabuild.RunNinjaOpts{}); err != nil {
			t.Fatalf("ninja err: %v", err)
		}
		dec := json.NewDecoder(bytes.NewReader(metrics.buf.Bytes()))
		for dec.More() {
			var m build.StepMetric
			if err := dec.Decode(&m); err != nil {
				t.Fatalf("decode metrics: %v", err)
			}
			if m.Output() == "gen" || m.Output() == "gen/" {
				return m.TwoPhaseCacheHit
			}
		}
		t.Fatalf("no metrics for gen/")
		return false
	}
	check := func(t *testing.T, want, stale []string) {
		t.Helper()
		for _, name := range want {
			if _, err := os.Stat(filepath.Join(gen, name)); err != nil {
				t.Errorf("gen/%s: %v; want present", name, err)
			}
		}
		for _, name := range stale {
			if _, err := os.Stat(filepath.Join(gen, name)); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("gen/%s still on disk (stat err=%v); the cache hit didn't remove the member its tree dropped", name, err)
			}
		}
	}

	t.Logf("-- build 1: names={a,b}, runs locally and fills the cache")
	if hit := run(t, "a\nb\n"); hit {
		t.Fatalf("build 1: gendir was a two phase cache hit; want local execution")
	}
	check(t, []string{"a", "b"}, nil)

	t.Logf("-- build 2: names={a}, runs locally and fills the cache")
	if hit := run(t, "a\n"); hit {
		t.Fatalf("build 2: gendir was a two phase cache hit; want local execution")
	}
	check(t, []string{"a"}, []string{"b"})

	t.Logf("-- build 3: names={a,b}, cache hit restores gen/{a,b}")
	if hit := run(t, "a\nb\n"); !hit {
		t.Fatalf("build 3: gendir was not a two phase cache hit")
	}
	check(t, []string{"a", "b"}, nil)

	t.Logf("-- build 4: names={a}, cache hit must remove gen/b")
	if hit := run(t, "a\n"); !hit {
		t.Fatalf("build 4: gendir was not a two phase cache hit")
	}
	check(t, []string{"a"}, []string{"b"})
}
