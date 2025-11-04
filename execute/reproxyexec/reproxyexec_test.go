// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package reproxyexec

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	lpb "github.com/bazelbuild/reclient/api/log"
	ppb "github.com/bazelbuild/reclient/api/proxy"
	cpb "github.com/bazelbuild/remote-apis-sdks/go/api/command"
	"github.com/golang/glog"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"go.chromium.org/build/siso/execute"
	"go.chromium.org/build/siso/execute/reproxyexec/reproxytest"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/o11y/iometrics"
	"go.chromium.org/build/siso/reapi/digest"
)

func setupDir(t *testing.T, root, dname string) {
	t.Helper()
	fullname := filepath.Join(root, dname)
	err := os.MkdirAll(fullname, 0755)
	if err != nil {
		t.Fatal(err)
	}
}

func setupFile(t *testing.T, dir, fname string) {
	t.Helper()
	setupDir(t, dir, filepath.Dir(fname))
	fullname := filepath.Join(dir, fname)
	err := os.WriteFile(fullname, nil, 0644)
	if err != nil {
		t.Fatal(err)
	}
}

func TestRun_Unauthenticated(t *testing.T) {
	defer glog.Flush()
	ctx := t.Context()
	dir := t.TempDir()

	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		err := hashFS.Close(ctx)
		if err != nil {
			t.Fatal(err)
		}
	}()

	setupFile(t, dir, "base/base.cc")
	setupFile(t, dir, "third_party/llvm-build/Release+Asserts/bin/clang++")
	setupDir(t, dir, "out/siso/obj/base")

	s := reproxytest.NewServer(ctx, t, &reproxytest.Fake{
		RunCommandFunc: func(ctx context.Context, req *ppb.RunRequest) (*ppb.RunResponse, error) {
			return &ppb.RunResponse{}, status.Error(codes.Unauthenticated, "Unable to authenticate with RBE")
		},
	})
	t.Cleanup(s.Close)
	re := REProxyExec{
		connAddress: s.Addr(),
	}
	err = re.Run(ctx, &execute.Cmd{
		ID:       "stepid",
		ExecRoot: dir,
		Dir:      "out/siso",
		HashFS:   hashFS,
		Args:     []string{"../../third_party/llvm-build/Release+Asserts/bin/clang++", "-c", "../../base/base.cc", "-o", "obj/base/base.o"},
		Inputs:   []string{"base/base.cc", "third_party/llvm-build/Release+Asserts/bin/clang++"},
		Outputs:  []string{"out/siso/obj/base/base.o"},
		REProxyConfig: &execute.REProxyConfig{
			ExecStrategy: "remote_local_fallback",
			Labels: map[string]string{
				"type":     "compile",
				"compiler": "clang",
				"lang":     "cpp",
			},
			Platform: map[string]string{
				"container-image": "gcr.io/chops-public-images-prod/xxx@sha256:xxx",
				"OSFamily":        "Linux",
			},
		},
	})
	if status.Code(err) != codes.Unauthenticated {
		t.Errorf("Run(ctx, cmd)=%v; want %v", err, codes.Unauthenticated)
	}
}

func TestRun_RemoteSuccess(t *testing.T) {
	defer glog.Flush()
	ctx := t.Context()
	dir := t.TempDir()

	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		err := hashFS.Close(ctx)
		if err != nil {
			t.Fatal(err)
		}
	}()

	setupFile(t, dir, "base/base.cc")
	setupFile(t, dir, "third_party/llvm-build/Release+Asserts/bin/clang++")
	setupDir(t, dir, "out/siso/obj/base")
	testOut := []byte("fake data")
	testOutDigest := digest.FromBytes("", testOut).Digest()
	s := reproxytest.NewServer(ctx, t, &reproxytest.Fake{
		RunCommandFunc: func(ctx context.Context, req *ppb.RunRequest) (*ppb.RunResponse, error) {
			err := os.WriteFile(filepath.Join(req.Command.ExecRoot, req.Command.GetOutput().GetOutputFiles()[0]), testOut, 0644)
			if err != nil {
				t.Fatalf("failed to write output files. %v", err)
			}
			return &ppb.RunResponse{
				Result: &cpb.CommandResult{
					Status: cpb.CommandResultStatus_SUCCESS,
				},
				ActionLog: &lpb.LogRecord{
					CompletionStatus: lpb.CompletionStatus_STATUS_REMOTE_EXECUTION,
					RemoteMetadata: &lpb.RemoteMetadata{
						Result: &cpb.CommandResult{
							Status: cpb.CommandResultStatus_SUCCESS,
						},
						OutputFileDigests: map[string]string{
							"obj/base/base.o": testOutDigest.String(),
						},
					},
				},
			}, nil
		},
	})
	t.Cleanup(s.Close)
	re := REProxyExec{
		connAddress: s.Addr(),
	}

	err = re.Run(ctx, &execute.Cmd{
		ID:       "stepid",
		ExecRoot: dir,
		Dir:      "out/siso",
		HashFS:   hashFS,
		Args:     []string{"../../third_party/llvm-build/Release+Asserts/bin/clang++", "-c", "../../base/base.cc", "-o", "obj/base/base.o"},
		Inputs:   []string{"base/base.cc", "third_party/llvm-build/Release+Asserts/bin/clang++"},
		Outputs:  []string{"out/siso/obj/base/base.o"},
		REProxyConfig: &execute.REProxyConfig{
			ExecStrategy: "remote_local_fallback",
			Labels: map[string]string{
				"type":     "compile",
				"compiler": "clang",
				"lang":     "cpp",
			},
			Platform: map[string]string{
				"container-image": "gcr.io/chops-public-images-prod/xxx@sha256:xxx",
				"OSFamily":        "Linux",
			},
		},
	})
	if err != nil {
		t.Errorf("Run(ctx, cmd)=%v; want nil", err)
	}

	// No IO operations should be taken by HashFS.
	wantStats := iometrics.Stats{}
	if diff := cmp.Diff(wantStats, hashFS.OS.Stats()); diff != "" {
		t.Errorf("hashFS.OS.IOMetrics.Stats(): diff -want +got:\n%s", diff)
	}

	// Check if the output digests are taken from RunResponse.
	entries, err := hashFS.Entries(ctx, dir, []string{"out/siso/obj/base/base.o"})
	if err != nil {
		t.Errorf("hashFS.Entries()=%v; want nil", err)
	}
	outEntry := entries[0]
	gotDg := outEntry.Data.Digest().String()
	wantDg := testOutDigest.String()
	if gotDg != wantDg {
		t.Errorf("output digest: got %q; want %q", gotDg, wantDg)
	}
	f, err := outEntry.Data.Open(ctx)
	if err != nil {
		t.Fatalf("failed to open output %q: %v", outEntry.Name, err)
	}
	defer func() {
		err := f.Close()
		if err != nil {
			t.Fatal(err)
		}
	}()
	buf, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("failed to read output: %q, %v", outEntry.Name, err)
	}
	if !bytes.Equal(buf, testOut) {
		t.Errorf("output content is different: got %q, want %q", buf, testOut)
	}
}

func TestRun_LocalFallback(t *testing.T) {
	defer glog.Flush()
	ctx := t.Context()
	dir := t.TempDir()

	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		err := hashFS.Close(ctx)
		if err != nil {
			t.Fatal(err)
		}
	}()

	setupFile(t, dir, "base/base.cc")
	setupFile(t, dir, "third_party/llvm-build/Release+Asserts/bin/clang++")
	setupDir(t, dir, "out/siso/obj/base")

	testOut := []byte("local fallback!")
	s := reproxytest.NewServer(ctx, t, &reproxytest.Fake{
		RunCommandFunc: func(ctx context.Context, req *ppb.RunRequest) (*ppb.RunResponse, error) {
			err := os.WriteFile(filepath.Join(req.Command.ExecRoot, req.Command.GetOutput().GetOutputFiles()[0]), testOut, 0644)
			if err != nil {
				t.Fatalf("failed to write output files. %v", err)
			}
			return &ppb.RunResponse{
				Result: &cpb.CommandResult{
					Status: cpb.CommandResultStatus_SUCCESS,
				},
				ActionLog: &lpb.LogRecord{
					CompletionStatus: lpb.CompletionStatus_STATUS_LOCAL_FALLBACK,
				},
			}, nil
		},
	})
	t.Cleanup(s.Close)
	re := REProxyExec{
		connAddress: s.Addr(),
	}

	err = re.Run(ctx, &execute.Cmd{
		ID:       "stepid",
		ExecRoot: dir,
		Dir:      "out/siso",
		HashFS:   hashFS,
		Args:     []string{"../../third_party/llvm-build/Release+Asserts/bin/clang++", "-c", "../../base/base.cc", "-o", "obj/base/base.o"},
		Inputs:   []string{"base/base.cc", "third_party/llvm-build/Release+Asserts/bin/clang++"},
		Outputs:  []string{"out/siso/obj/base/base.o"},
		REProxyConfig: &execute.REProxyConfig{
			ExecStrategy: "remote_local_fallback",
			Labels: map[string]string{
				"type":     "compile",
				"compiler": "clang",
				"lang":     "cpp",
			},
			Platform: map[string]string{
				"container-image": "gcr.io/chops-public-images-prod/xxx@sha256:xxx",
				"OSFamily":        "Linux",
			},
		},
	})
	if err != nil {
		t.Errorf("Run(ctx, cmd)=%v; want nil", err)
	}

	// HashFS may or may not reads out/siso/obj/base/base.o to calculate the digest at this point.

	// Check the output digests.
	entries, err := hashFS.Entries(ctx, dir, []string{"out/siso/obj/base/base.o"})
	if err != nil {
		t.Errorf("hashFS.Entries()=%v; want nil", err)
	}
	outEntry := entries[0]
	gotDg := outEntry.Data.Digest().String()
	wantDg := digest.FromBytes("", testOut).Digest().String()
	if gotDg != wantDg {
		t.Errorf("output digest: got %s; want %s", gotDg, wantDg)
	}
	f, err := outEntry.Data.Open(ctx)
	if err != nil {
		t.Fatalf("failed to open output %q: %v", outEntry.Name, err)
	}
	defer func() {
		err := f.Close()
		if err != nil {
			t.Fatal(err)
		}
	}()
	buf, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("failed to read output: %q, %v", outEntry.Name, err)
	}
	if !bytes.Equal(buf, testOut) {
		t.Errorf("output content is different: got %q, want %q", buf, testOut)
	}
}
