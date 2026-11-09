// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package e2etests

import (
	"fmt"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/google/go-cmp/cmp"

	"go.chromium.org/build/hashigo/digest"
	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/build/ninjabuild"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/reapi/reapitest"
	"go.chromium.org/build/siso/toolsupport/abfsutil"
)

func TestBuild_ABFS(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("abfs is only available on linux now")
	}
	if !runInSubProcess(t) {
		return
	}
	ctx := t.Context()
	dir := tempDir(t)

	runNinjaTest := func(t *testing.T, refake *reapitest.Fake, abfsClient *abfsutil.Client) (build.Stats, error) {
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
			ABFS:       abfsClient,
		})
		defer cleanup()
		opt.REAPIClient = ds.Client
		return ninjabuild.Run(ctx, graph, opt, nil, ninjabuild.RunNinjaOpts{})
	}

	outData := []byte("foo.out content")
	var outDigest *rpb.Digest
	setupFiles(t, dir, t.Name(), nil)
	fakere := &reapitest.Fake{
		ExecuteFunc: func(fakere *reapitest.Fake, action *rpb.Action) (*rpb.ActionResult, error) {
			var err error
			outDigest, err = fakere.Put(ctx, outData)
			if err != nil {
				msg := fmt.Sprintf("failed to write gen/foo.out: %v", err)
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
						Path:   "gen/foo.out",
						Digest: outDigest,
					},
				},
			}, nil
		},
	}

	endpoint := filepath.Join(t.TempDir(), "abfs.sock")
	fake := &abfsutil.Fake{
		Dir: dir,
		Store: map[digest.Digest][]byte{
			{Hash: outDigest.GetHash(), SizeBytes: outDigest.GetSizeBytes()}: outData,
		},
	}
	abfsClient := fake.Start(ctx, t, endpoint)

	stats, err := runNinjaTest(t, fakere, abfsClient)
	if err != nil {
		t.Fatalf("ninja: %v", err)
	}
	if stats.Done != stats.Total || stats.Done != 1 {
		t.Errorf("done=%d total=%d; want done=total=1", stats.Done, stats.Total)
	}
	wantGetRBEDigests := map[string]int{
		"foo.in":               1,
		"out/siso/build.ninja": 1,
		"tools/cp.py":          1,
	}
	if diff := cmp.Diff(wantGetRBEDigests, fake.GetRBEDigests()); diff != "" {
		t.Errorf("get_rbe_digests: diff -want +got:\n%s", diff)
	}

	wantSetRBEDigests := map[string]int{
		"out/siso/gen/foo.out": 1,
	}
	if diff := cmp.Diff(wantSetRBEDigests, fake.SetRBEDigests()); diff != "" {
		t.Errorf("set_rbe_digests: diff -want +got:\n%s", diff)
	}
}
