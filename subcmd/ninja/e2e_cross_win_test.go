// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	pb "github.com/bazelbuild/reclient/api/proxy"
	cpb "github.com/bazelbuild/remote-apis-sdks/go/api/command"
	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/execute/reproxyexec/reproxytest"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/reapi/digest"
	"go.chromium.org/build/siso/reapi/reapitest"
)

// tools/cp has is_executable even from windows to make it executable.
func TestBuild_CrossWindows_Remote(t *testing.T) {
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
	_, err := ninja(t, fakere)
	if err != nil {
		t.Fatalf("ninja %v; want nil err", err)
	}
}

// tools/cp is passed via toolchain_inputs from windows to make it executable.
func TestBuild_CrossWindows_Reproxy(t *testing.T) {
	ctx := t.Context()
	dir := tempDir(t)

	ninja := func(t *testing.T, refake *reproxytest.Fake) (build.Stats, error) {
		t.Helper()
		s := reproxytest.NewServer(ctx, t, refake)
		defer s.Close()

		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile: ".siso_fs_state",
		})
		defer cleanup()
		opt.ReproxyAddr = s.Addr()
		return runNinja(ctx, "build.ninja", graph, opt, nil, runNinjaOpts{})
	}

	setupFiles(t, dir, t.Name(), nil)
	fakere := &reproxytest.Fake{
		RunCommandFunc: func(ctx context.Context, req *pb.RunRequest) (*pb.RunResponse, error) {
			if runtime.GOOS == "windows" && !slices.Equal(req.GetToolchainInputs(), []string{"tools/cp"}) {
				return &pb.RunResponse{
					Stderr: []byte("../../tools/cp: Permission denied\n"),
					Result: &cpb.CommandResult{
						Status:   cpb.CommandResultStatus_NON_ZERO_EXIT,
						ExitCode: 1,
						Msg:      fmt.Sprintf("tools/cp is not in toolchain_inputs: %q", req.GetToolchainInputs()),
					},
				}, nil
			}
			err := os.WriteFile(filepath.Join(dir, "out/siso/gen/foo.out"), nil, 0644)
			if err != nil {
				return &pb.RunResponse{
					Stderr: []byte(err.Error()),
					Result: &cpb.CommandResult{
						Status:   cpb.CommandResultStatus_NON_ZERO_EXIT,
						ExitCode: 1,
						Msg:      fmt.Sprintf("failed to create gen/foo.out: %v", err),
					},
				}, nil
			}
			return &pb.RunResponse{
				Result: &cpb.CommandResult{
					Status: cpb.CommandResultStatus_SUCCESS,
				},
			}, nil
		},
	}
	_, err := ninja(t, fakere)
	if err != nil {
		t.Fatalf("ninja %v; want nil err", err)
	}
}
