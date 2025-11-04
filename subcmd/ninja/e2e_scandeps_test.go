// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"errors"
	"testing"

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/reapi/digest"
	"go.chromium.org/build/siso/reapi/reapitest"
	"go.chromium.org/build/siso/scandeps"
)

func TestBuild_ScanDeps_ClangCL_FI(t *testing.T) {
	ctx := t.Context()
	dir := tempDir(t)

	ninja := func(t *testing.T, fakere *reapitest.Fake) (build.Stats, error) {
		t.Helper()
		var ds dataSource
		defer func() {
			err := ds.Close(ctx)
			if err != nil {
				t.Error(err)
			}
		}()
		ds.client = reapitest.New(ctx, t, fakere)
		ds.cache = ds.client.CacheStore()

		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:  ".siso_fs_state",
			DataSource: ds,
		})
		defer cleanup()
		opt.REAPIClient = ds.client
		return runNinja(ctx, "build.ninja", graph, opt, nil, runNinjaOpts{})
	}

	setupFiles(t, dir, t.Name(), nil)
	fakere := &reapitest.Fake{
		ExecuteFunc: func(fakere *reapitest.Fake, action *rpb.Action) (*rpb.ActionResult, error) {
			t.Logf("-- remote action %s", action)
			tree := reapitest.InputTree{CAS: fakere.CAS, Root: action.InputRootDigest}
			_, err := tree.LookupFileNode(ctx, "third_party/ffmpeg/compat/msvcrt/snprintf.h")
			if err != nil {
				t.Logf("-- error: third_party/ffmpeg/compat/msvc/snprintf.h does not exists")
				return &rpb.ActionResult{
					ExitCode:  1,
					StderrRaw: []byte("<built-in>(1,10): fatal error: 'compat/msvcrt/snprintf.h' file not found\n"),
				}, nil
			}
			t.Logf("-- third_party/ffmpeg/compat/msvc/snprintf.h exists")
			return &rpb.ActionResult{
				ExitCode: 0,
				OutputFiles: []*rpb.OutputFile{
					{
						Path:   "obj/third_party/ffmpeg/m.obj",
						Digest: digest.Empty.Proto(),
					},
				},
			}, nil
		},
	}

	stats, err := ninja(t, fakere)
	if err != nil {
		t.Fatalf("ninja err: %v; want nil err", err)
	}
	if stats.Done != stats.Total || stats.Remote != 1 || stats.Local != 0 {
		t.Errorf("stats done=%d total=%d remote=%d local=%d; want remote=1 local=0", stats.Done, stats.Total, stats.Remote, stats.Local)
	}
}

func TestBuild_ScanDeps_Timeout(t *testing.T) {
	ctx := t.Context()
	dir := tempDir(t)

	ninja := func(t *testing.T, fakere *reapitest.Fake) (build.Stats, error) {
		t.Helper()
		var ds dataSource
		defer func() {
			err := ds.Close(ctx)
			if err != nil {
				t.Error(err)
			}
		}()
		ds.client = reapitest.New(ctx, t, fakere)
		ds.cache = ds.client.CacheStore()

		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:  ".siso_fs_state",
			DataSource: ds,
		})
		defer cleanup()
		opt.REAPIClient = ds.client
		return runNinja(ctx, "build.ninja", graph, opt, nil, runNinjaOpts{})
	}
	scandeps.SetErrForTest(errors.New("scandeps err"))
	defer scandeps.SetErrForTest(nil)

	setupFiles(t, dir, "TestBuild_ScanDeps_ClangCL_FI", nil)

	fakere := &reapitest.Fake{
		ExecuteFunc: func(fakere *reapitest.Fake, action *rpb.Action) (*rpb.ActionResult, error) {
			t.Logf("-- remote action %s", action)
			tree := reapitest.InputTree{CAS: fakere.CAS, Root: action.InputRootDigest}
			_, err := tree.LookupFileNode(ctx, "third_party/ffmpeg/compat/msvcrt/snprintf.h")
			if err != nil {
				t.Logf("-- error: third_party/ffmpeg/compat/msvc/snprintf.h does not exists")
				return &rpb.ActionResult{
					ExitCode:  1,
					StderrRaw: []byte("<built-in>(1,10): fatal error: 'compat/msvcrt/snprintf.h' file not found\n"),
				}, nil
			}
			t.Logf("-- third_party/ffmpeg/compat/msvc/snprintf.h exists")
			return &rpb.ActionResult{
				ExitCode: 0,
				OutputFiles: []*rpb.OutputFile{
					{
						Path:   "obj/third_party/ffmpeg/m.obj",
						Digest: digest.Empty.Proto(),
					},
				},
			}, nil
		},
	}

	stats, err := ninja(t, fakere)
	if err != nil {
		t.Fatalf("ninja err: %v; want nil err", err)
	}
	if stats.Done != stats.Total || stats.Remote != 0 || stats.Local != 1 || stats.ScanDepsFailed != 1 {
		t.Errorf("stats done=%d total=%d remote=%d local=%d scanDepsFailed=%d; want remote=0 local=1 scanDepsFailed=1", stats.Done, stats.Total, stats.Remote, stats.Local, stats.ScanDepsFailed)
	}
}
