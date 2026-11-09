// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package e2etests

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"go.chromium.org/build/hashigo/digest"
	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/blob"
	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/build/ninjabuild"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/reapi/reapitest"
)

// TestBuild_OutputLocalGraph exercises -output_local_strategy=graph end to end.
// A single remote step (A) produces out0, consumed by a local step (B), and
// out1, consumed by nobody (the .dwo analog). graph must materialize out0 to
// local disk and leave out1 in CAS. The original per-step classifier would
// materialize out1 too, so the "out1 not on disk" assertion guards the
// per-output decision.
func TestBuild_OutputLocalGraph(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	ctx := t.Context()
	dir := tempDir(t)
	setupFiles(t, dir, t.Name(), nil)

	out0Data := []byte("out0-data")
	out1Data := []byte("out1-data")
	fakere := &reapitest.Fake{
		ExecuteFunc: func(fakere *reapitest.Fake, action *rpb.Action) (*rpb.ActionResult, error) {
			out0Digest, err := fakere.Put(ctx, out0Data)
			if err != nil {
				return &rpb.ActionResult{ExitCode: 1, StderrRaw: fmt.Appendf(nil, "put out0: %v", err)}, nil
			}
			out1Digest, err := fakere.Put(ctx, out1Data)
			if err != nil {
				return &rpb.ActionResult{ExitCode: 1, StderrRaw: fmt.Appendf(nil, "put out1: %v", err)}, nil
			}
			return &rpb.ActionResult{
				ExitCode: 0,
				OutputFiles: []*rpb.OutputFile{
					{Path: "out0", Digest: out0Digest},
					{Path: "out1", Digest: out1Digest},
				},
			}, nil
		},
	}

	// Wire the "graph" strategy exactly as Command.Run does: one shared
	// LocallyNeededSet backs both the hashfs predicate and the scheduler.
	locallyNeeded := build.NewLocallyNeededSet()
	runNinjaTest := func(t *testing.T) (build.Stats, error) {
		t.Helper()
		var ds build.DataSource
		defer func() {
			if err := ds.Close(ctx); err != nil {
				t.Error(err)
			}
		}()
		ds.Client = reapitest.New(ctx, t, fakere)
		ds.Cache = ds.Client.CacheStore()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:   ".siso_fs_state",
			OutputLocal: locallyNeeded.OutputLocal,
			DataSource:  ds,
		})
		defer cleanup()
		opt.REAPIClient = ds.Client
		opt.LocallyNeeded = locallyNeeded
		return ninjabuild.Run(ctx, graph, opt, nil, ninjabuild.RunNinjaOpts{})
	}

	stats, err := runNinjaTest(t)
	if err != nil {
		t.Fatalf("ninja err: %v", err)
	}
	if stats.Done != stats.Total {
		t.Errorf("done=%d total=%d; want done==total: %#v", stats.Done, stats.Total, stats)
	}

	// out0: consumed by the local step, so materialized to local disk.
	if got, err := os.ReadFile(filepath.Join(dir, "out/siso/out0")); err != nil {
		t.Errorf("out0 should be on local disk: %v", err)
	} else if string(got) != string(out0Data) {
		t.Errorf("out0 content=%q; want %q", got, out0Data)
	}

	// out2: the local step's own output, on disk.
	if _, err := os.ReadFile(filepath.Join(dir, "out/siso/out2")); err != nil {
		t.Errorf("out2 should be on local disk: %v", err)
	}

	// out1: no consumer, so NOT materialized, left in CAS, not on local disk.
	if _, err := os.ReadFile(filepath.Join(dir, "out/siso/out1")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("out1 should NOT be on local disk (left in CAS); ReadFile err=%v", err)
	}

	// ...but out1 is still known to hashfs with its digest, i.e. in CAS, not lost.
	st, err := hashfs.Load(ctx, hashfs.Option{StateFile: filepath.Join(dir, "out/siso/.siso_fs_state")})
	if err != nil {
		t.Fatalf("hashfs.Load=%v; want nil err", err)
	}
	m := hashfs.StateMap(digest.SHA256, st)
	e1, ok := m[filepath.ToSlash(filepath.Join(dir, "out/siso/out1"))]
	if !ok {
		t.Errorf("out1 not found in hashfs state: %v", m)
	} else {
		want := blob.FromBytes(digest.SHA256, "", out1Data).Digest()
		if e1.Digest.Hash != want.Hash || e1.Digest.SizeBytes != want.SizeBytes {
			t.Errorf("out1 digest=%s; want %s", e1.Digest, want)
		}
	}
}
