// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package e2etests

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/build/ninjabuild"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/reapi/reapitest"
)

// graphRemoteFake returns a fake REAPI whose remote action (A) produces out0
// and out1 with the given contents. Reuses the TestBuild_OutputLocalGraph
// testdata: A is remote, B is local and consumes out0, out1 has no consumer.
func graphRemoteFake(t *testing.T, out0, out1 []byte) *reapitest.Fake {
	return &reapitest.Fake{
		ExecuteFunc: func(fakere *reapitest.Fake, action *rpb.Action) (*rpb.ActionResult, error) {
			d0, err := fakere.Put(t.Context(), out0)
			if err != nil {
				return &rpb.ActionResult{ExitCode: 1, StderrRaw: fmt.Appendf(nil, "put out0: %v", err)}, nil
			}
			d1, err := fakere.Put(t.Context(), out1)
			if err != nil {
				return &rpb.ActionResult{ExitCode: 1, StderrRaw: fmt.Appendf(nil, "put out1: %v", err)}, nil
			}
			return &rpb.ActionResult{
				ExitCode:    0,
				OutputFiles: []*rpb.OutputFile{{Path: "out0", Digest: d0}, {Path: "out1", Digest: d1}},
			}, nil
		},
	}
}

// TestBuild_OutputLocalGraph_RequestedSibling: requesting out1 (no consumer)
// alongside out2 must materialize out1 to local disk, even though its edge
// sibling out0 feeds a local step.
func TestBuild_OutputLocalGraph_RequestedSibling(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	ctx := t.Context()
	dir := tempDir(t)
	setupFiles(t, dir, "TestBuild_OutputLocalGraph", nil)
	fakere := graphRemoteFake(t, []byte("out0-data"), []byte("out1-data"))

	var ds build.DataSource
	defer func() {
		if err := ds.Close(ctx); err != nil {
			t.Error(err)
		}
	}()
	ds.Client = reapitest.New(ctx, t, fakere)
	ds.Cache = ds.Client.CacheStore()
	set := build.NewLocallyNeededSet()
	opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
		StateFile: ".siso_fs_state", OutputLocal: set.OutputLocal, DataSource: ds,
	})
	defer cleanup()
	opt.REAPIClient = ds.Client
	opt.LocallyNeeded = set

	if _, err := ninjabuild.Run(ctx, graph, opt, []string{"out2", "out1"}, ninjabuild.RunNinjaOpts{}); err != nil {
		t.Fatalf("ninja: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "out/siso/out1")); err != nil {
		t.Errorf("requested out1 should be on local disk: %v", err)
	} else if string(got) != "out1-data" {
		t.Errorf("out1 content=%q; want %q", got, "out1-data")
	}
}

// TestBuild_OutputLocalGraph_NullRebuildReuse: after a build that leaves out1
// in CAS, a fresh-process null rebuild must reuse it (Has returns false until
// Freeze, so state load keeps the CAS-only entry) and re-run nothing.
func TestBuild_OutputLocalGraph_NullRebuildReuse(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	ctx := t.Context()
	dir := tempDir(t)
	setupFiles(t, dir, "TestBuild_OutputLocalGraph", nil)
	fakere := graphRemoteFake(t, []byte("out0-data"), []byte("out1-data"))

	// fresh LocallyNeededSet + REAPI client each invocation, like ninja.go.
	run := func() build.Stats {
		var ds build.DataSource
		defer func() {
			if err := ds.Close(ctx); err != nil {
				t.Error(err)
			}
		}()
		ds.Client = reapitest.New(ctx, t, fakere)
		ds.Cache = ds.Client.CacheStore()
		set := build.NewLocallyNeededSet()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile: ".siso_fs_state", OutputLocal: set.OutputLocal, DataSource: ds,
		})
		defer cleanup()
		opt.REAPIClient = ds.Client
		opt.LocallyNeeded = set
		stats, err := ninjabuild.Run(ctx, graph, opt, []string{"out2"}, ninjabuild.RunNinjaOpts{})
		if err != nil {
			t.Fatalf("ninja: %v", err)
		}
		return stats
	}

	run() // first build: out0 materialized, out1 left in CAS.
	s2 := run()
	if s2.Remote != 0 || s2.Local != 0 {
		t.Errorf("null rebuild re-ran steps remote=%d local=%d; want 0 (out1's producer reused)", s2.Remote, s2.Local)
	}
}
