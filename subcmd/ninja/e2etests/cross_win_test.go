// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package e2etests

import (
	"testing"

	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/build/ninjabuild"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/reapi/digest"
	"go.chromium.org/build/siso/reapi/reapitest"
)

// tools/cp has is_executable even from windows to make it executable.
func TestBuild_CrossWindows_Remote(t *testing.T) {
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
		ExecuteFunc: func(fakere *reapitest.Fake, action *rpb.Action) (*rpb.ActionResult, error) {
			tree := reapitest.InputTree{CAS: fakere.CAS, Root: action.InputRootDigest}
			fn, err := tree.LookupFileNode(ctx, "tools/cp")
			if err != nil {
				return &rpb.ActionResult{
					ExitCode:  1,
					StderrRaw: []byte("../../tools/cp: File not found\n"),
				}, nil
			}
			if !fn.IsExecutable {
				return &rpb.ActionResult{
					ExitCode:  1,
					StderrRaw: []byte("../../tools/cp: Permission denied\n"),
				}, nil
			}
			return &rpb.ActionResult{
				ExitCode: 0,
				OutputFiles: []*rpb.OutputFile{
					{
						Path:   "gen/foo.out",
						Digest: digest.Empty.Proto(),
					},
				},
			}, nil
		},
	}
	_, err := runNinjaTest(t, fakere)
	if err != nil {
		t.Fatalf("ninja %v; want nil err", err)
	}
}
