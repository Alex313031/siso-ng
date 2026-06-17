// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package e2etests

import (
	"fmt"
	"testing"

	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/build/ninjabuild"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/reapi/digest"
	"go.chromium.org/build/siso/reapi/reapitest"
)

func TestBuild_MissingInputDir(t *testing.T) {
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
			StateFile:  ".siso_fs_state",
			DataSource: ds,
		})
		defer cleanup()
		opt.REAPIClient = ds.Client
		return ninjabuild.Run(ctx, graph, opt, nil, ninjabuild.RunNinjaOpts{})
	}

	setupFiles(t, dir, t.Name(), nil)
	fakere := &reapitest.Fake{
		ExecuteFunc: func(fakere *reapitest.Fake, aciton *rpb.Action) (*rpb.ActionResult, error) {
			depfile, err := fakere.Put(ctx, []byte("obj/foo/foo.o: ../../foo/foo.c"))
			if err != nil {
				return &rpb.ActionResult{
					ExitCode:  1,
					StderrRaw: fmt.Appendf(nil, "failed to write obj/foo/foo.o.d: %v", err),
				}, nil
			}
			return &rpb.ActionResult{
				ExitCode: 0,
				OutputFiles: []*rpb.OutputFile{
					{
						Path:   "obj/foo/foo.o",
						Digest: digest.Empty.Proto(),
					},
					{
						Path:   "obj/foo/foo.o.d",
						Digest: depfile,
					},
				},
			}, nil
		},
	}
	stats, err := runNinjaTest(t, fakere)
	if err != nil {
		t.Fatalf("ninja %v", err)
	}
	if stats.Done != stats.Total || stats.Total != 1 || stats.Remote != 1 || stats.Local != 0 {
		t.Errorf("done=%d total=%d remote=%d local=%d; want done=1 total=1 remote=1 local=0; %#v", stats.Done, stats.Total, stats.Remote, stats.Local, stats)
	}
}
