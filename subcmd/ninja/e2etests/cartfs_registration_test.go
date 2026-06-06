// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package e2etests

import (
	"context"
	"fmt"
	"net"
	"runtime"
	"testing"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/testing/protocmp"

	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/build/ninjabuild"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/reapi/reapitest"
	"go.chromium.org/build/siso/toolsupport/cartfsutil"
	cartfspb "go.chromium.org/build/siso/toolsupport/cartfsutil/proto/server"
)

func TestBuild_CartfsRegistration(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("cartfs is only available on linux now")
	}
	if !runInSubProcess(t) {
		return
	}
	ctx := t.Context()
	dir := tempDir(t)

	runNinjaTest := func(t *testing.T, refake *reapitest.Fake, cartfsClient *cartfsutil.Client) (build.Stats, error) {
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
			CartFS:     cartfsClient,
		})
		defer cleanup()
		opt.REAPIClient = ds.Client
		return ninjabuild.Run(ctx, graph, opt, nil, ninjabuild.RunNinjaOpts{})
	}

	var outDigest *rpb.Digest
	setupFiles(t, dir, t.Name(), nil)
	fakere := &reapitest.Fake{
		ExecuteFunc: func(fakere *reapitest.Fake, action *rpb.Action) (*rpb.ActionResult, error) {
			var err error
			outDigest, err = fakere.Put(ctx, []byte("foo.out content"))
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

	cartfs := &fakeCartfsServer{
		dir: dir,
	}
	cartfsClient := fakeCartfsClient(ctx, t, cartfs)

	stats, err := runNinjaTest(t, fakere, cartfsClient)
	if err != nil {
		t.Fatalf("ninja: %v", err)
	}
	if stats.Done != stats.Total || stats.Done != 1 {
		t.Errorf("done=%d total=%d; want done=total=1", stats.Done, stats.Total)
	}
	want := []*cartfspb.RegisterFilesRequest{
		{
			Registrations: []*cartfspb.FileRegistrationInfo{
				{
					Path:    "out/siso/gen/foo.out",
					Hash:    outDigest.GetHash(),
					Size:    uint64(outDigest.GetSizeBytes()),
					Urgency: cartfspb.ContentPullUrgency_CONTENT_PULL_URGENCY_ON_ACCESS,
				},
			},
		},
	}
	if diff := cmp.Diff(want, cartfs.reqs, protocmp.Transform()); diff != "" {
		t.Errorf("registration reqs -want +got:\n%s", diff)
	}
}

type fakeCartfsServer struct {
	cartfspb.UnimplementedCartfsServer
	dir  string
	t    *testing.T
	reqs []*cartfspb.RegisterFilesRequest
}

func fakeCartfsClient(ctx context.Context, t *testing.T, fake *fakeCartfsServer) *cartfsutil.Client {
	t.Helper()
	fake.t = t
	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := lis.Addr().String()
	t.Logf("fake cartfs at %s", addr)
	serv := grpc.NewServer()
	cartfspb.RegisterCartfsServer(serv, fake)
	done := make(chan error)
	go func() {
		done <- serv.Serve(lis)
	}()
	t.Cleanup(func() {
		serv.Stop()
		err := <-done
		t.Logf("-- server finished: %v", err)
	})

	client, err := cartfsutil.New(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func (f *fakeCartfsServer) GetState(ctx context.Context, req *cartfspb.GetStateRequest) (*cartfspb.GetStateResponse, error) {
	return &cartfspb.GetStateResponse{
		State:      cartfspb.CartfsState_STATE_RUNNING,
		MountPoint: f.dir,
	}, nil
}

func (f *fakeCartfsServer) RegisterFiles(ctx context.Context, req *cartfspb.RegisterFilesRequest) (*cartfspb.RegisterFilesResponse, error) {
	f.t.Logf("register %v\n", req)
	f.reqs = append(f.reqs, req)
	return &cartfspb.RegisterFilesResponse{}, nil
}
