// Copyright 2025 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package e2etests

import (
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/build/ninjabuild"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/reapi/digest"
	"go.chromium.org/build/siso/reapi/reapitest"
)

func TestBuild_RemoteWorkDir(t *testing.T) {
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
			cmd := &rpb.Command{}
			err := fakere.FetchProto(ctx, action.CommandDigest, cmd)
			if err != nil {
				return nil, err
			}
			tree := reapitest.InputTree{CAS: fakere.CAS, Root: action.InputRootDigest}
			_, err = tree.LookupDirectoryNode(ctx, cmd.WorkingDirectory)
			if err != nil {
				return nil, status.Errorf(codes.FailedPrecondition, "working directory is not an input directory %s: %v", cmd.WorkingDirectory, err)
			}
			return &rpb.ActionResult{
				ExitCode: 0,
				OutputFiles: []*rpb.OutputFile{
					{
						Path:   "out",
						Digest: digest.Empty.Proto(),
					},
				},
			}, nil
		},
	}

	stats, err := runNinjaTest(t, fakere)
	if err != nil {
		t.Fatalf("ninja %v: want nil err", err)
	}
	if stats.Done != stats.Total || stats.Total != 1 || stats.Remote != 1 {
		t.Errorf("done=%d total=%d remote=%d; want done=1 total=1 remote=1; %#v", stats.Done, stats.Total, stats.Remote, stats)
	}
}
