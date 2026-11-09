// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package osfs

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"go.chromium.org/build/hashigo/digest"

	"go.chromium.org/build/siso/toolsupport/cartfsutil"
	cartfspb "go.chromium.org/build/siso/toolsupport/cartfsutil/proto/server"
)

type fakeCartfsServer struct {
	cartfspb.UnimplementedCartfsServer
	dir     string
	digests map[string]digest.Digest
	errs    map[string]error
}

func (f *fakeCartfsServer) GetState(ctx context.Context, req *cartfspb.GetStateRequest) (*cartfspb.GetStateResponse, error) {
	return &cartfspb.GetStateResponse{
		State:      cartfspb.CartfsState_STATE_RUNNING,
		MountPoint: f.dir,
	}, nil
}

func (f *fakeCartfsServer) GetDigest(ctx context.Context, req *cartfspb.GetDigestRequest) (*cartfspb.GetDigestResponse, error) {
	relpath := filepath.ToSlash(req.GetPath())
	if err, ok := f.errs[relpath]; ok {
		return nil, err
	}
	if d, ok := f.digests[relpath]; ok {
		return &cartfspb.GetDigestResponse{
			Hash: d.Hash,
			Size: uint64(d.SizeBytes),
		}, nil
	}
	return nil, status.Error(codes.NotFound, "not found")
}

func setupFakeCartFS(ctx context.Context, t *testing.T, dir string) (*cartfsutil.Client, *fakeCartfsServer) {
	t.Helper()
	fake := &fakeCartfsServer{
		dir:     dir,
		digests: make(map[string]digest.Digest),
		errs:    make(map[string]error),
	}
	lis := bufconn.Listen(1 << 20)
	serv := grpc.NewServer()
	cartfspb.RegisterCartfsServer(serv, fake)
	go func() {
		_ = serv.Serve(lis)
	}()
	t.Cleanup(func() {
		serv.Stop()
	})

	client, err := cartfsutil.New(ctx, "passthrough:///bufnet", grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return lis.DialContext(ctx)
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		client.Close()
	})
	return client, fake
}

func TestFileDigestFromFS_UnsupportedDigestFunction(t *testing.T) {
	ctx := t.Context()
	gitsha1Fn, err := digest.ParseFunction("gitsha1")
	if err != nil {
		t.Fatalf("ParseFunction(gitsha1): %v", err)
	}

	ofs := New(ctx, "test", Option{
		DigestFunction:  gitsha1Fn,
		DigestXattrName: "user.sha256",
	})
	if ofs.digestXattrName == "" {
		t.Skip("no xattr support")
	}

	_, err = ofs.FileDigestFromFS(ctx, "foo.txt", 10)
	if !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("FileDigestFromFS got err = %v, want %v", err, errors.ErrUnsupported)
	}

	fsc := ofs.FileSource("foo.txt", 10)
	_, err = fsc.FileDigestFromFS(ctx)
	if !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("FileSource.FileDigestFromFS got err = %v, want %v", err, errors.ErrUnsupported)
	}
}

func TestFileDigestFromFS_FromXattr_KnownSize(t *testing.T) {
	ctx := t.Context()
	origXattr := xattrLGet
	t.Cleanup(func() { xattrLGet = origXattr })

	wantDigest := digest.Digest{
		Hash:      "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef",
		SizeBytes: 100,
	}

	xattrLGet = func(name, attr string) ([]byte, error) {
		if name == "foo.txt" && attr == "user.sha256" {
			return []byte(wantDigest.Hash), nil
		}
		return nil, errors.New("xattr not found")
	}

	ofs := New(ctx, "test", Option{
		DigestXattrName: "user.sha256",
	})
	if ofs.digestXattrName == "" {
		t.Skip("no xattr support")
	}

	got, err := ofs.FileDigestFromFS(ctx, "foo.txt", 100)
	if err != nil {
		t.Fatalf("FileDigestFromFS: %v", err)
	}
	if got != wantDigest {
		t.Errorf("FileDigestFromFS got %v, want %v", got, wantDigest)
	}

	fsc := ofs.FileSource("foo.txt", 100)
	got, err = fsc.FileDigestFromFS(ctx)
	if err != nil {
		t.Fatalf("FileSource.FileDigestFromFS: %v", err)
	}
	if got != wantDigest {
		t.Errorf("FileSource.FileDigestFromFS got %v, want %v", got, wantDigest)
	}
}

func TestFileDigestFromFS_FromXattr_UnknownSize(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	fname := filepath.Join(dir, "testfile.txt")
	content := []byte("hello world xattr")
	if err := os.WriteFile(fname, content, 0644); err != nil {
		t.Fatal(err)
	}

	origXattr := xattrLGet
	t.Cleanup(func() { xattrLGet = origXattr })

	wantHash := "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"
	xattrLGet = func(name, attr string) ([]byte, error) {
		if name == fname && attr == "user.sha256" {
			return []byte(wantHash), nil
		}
		return nil, errors.New("xattr not found")
	}

	ofs := New(ctx, "test", Option{
		DigestXattrName: "user.sha256",
	})
	if ofs.digestXattrName == "" {
		t.Skip("no xattr support")
	}

	got, err := ofs.FileDigestFromFS(ctx, fname, -1)
	if err != nil {
		t.Fatalf("FileDigestFromFS: %v", err)
	}
	wantDigest := digest.Digest{
		Hash:      wantHash,
		SizeBytes: int64(len(content)),
	}
	if got != wantDigest {
		t.Errorf("FileDigestFromFS got %v, want %v", got, wantDigest)
	}
}

func TestFileDigestFromFS_FromXattr_UnknownSize_StatError(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	fname := filepath.Join(dir, "nonexistent.txt")

	origXattr := xattrLGet
	t.Cleanup(func() { xattrLGet = origXattr })

	xattrLGet = func(name, attr string) ([]byte, error) {
		if name == fname && attr == "user.sha256" {
			return []byte("dummyhash"), nil
		}
		return nil, errors.New("xattr not found")
	}

	ofs := New(ctx, "test", Option{
		DigestXattrName: "user.sha256",
	})
	if ofs.digestXattrName == "" {
		t.Skip("no xattr support")
	}

	_, err := ofs.FileDigestFromFS(ctx, fname, -1)
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("FileDigestFromFS got err = %v, want os.ErrNotExist", err)
	}
}

func TestFileDigestFromFS_FromCartFS(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("cartfs is only available on linux now")
	}
	ctx := t.Context()
	dir := t.TempDir()

	cartfsClient, fakeCartFS := setupFakeCartFS(ctx, t, dir)

	origXattr := xattrLGet
	t.Cleanup(func() { xattrLGet = origXattr })

	xattrLGet = func(name, attr string) ([]byte, error) {
		return nil, errors.New("xattr not supported")
	}

	ofs := New(ctx, "test", Option{
		DigestXattrName: "user.sha256",
		CartFS:          cartfsClient,
	})
	if ofs.digestXattrName == "" {
		t.Skip("no xattr supported")
	}

	relpath := filepath.Join("sub", "file.txt")
	fname := filepath.Join(dir, relpath)

	wantDigest := digest.Digest{
		Hash:      "cartfshash1234567890abcdef1234567890abcdef1234",
		SizeBytes: 42,
	}
	fakeCartFS.digests[filepath.ToSlash(relpath)] = wantDigest

	got, err := ofs.FileDigestFromFS(ctx, fname, -1)
	if err != nil {
		t.Fatalf("FileDigestFromFS: %v", err)
	}
	if got != wantDigest {
		t.Errorf("FileDigestFromFS got %v, want %v", got, wantDigest)
	}

	fsc := ofs.FileSource(fname, -1)
	got, err = fsc.FileDigestFromFS(ctx)
	if err != nil {
		t.Fatalf("FileSource.FileDigestFromFS: %v", err)
	}
	if got != wantDigest {
		t.Errorf("FileSource.FileDigestFromFS got %v, want %v", got, wantDigest)
	}
}

func TestFileDigestFromFS_FromCartFS_Error(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("cartfs is only available on linux now")
	}
	ctx := t.Context()
	dir := t.TempDir()

	cartfsClient, fakeCartFS := setupFakeCartFS(ctx, t, dir)

	origXattr := xattrLGet
	t.Cleanup(func() { xattrLGet = origXattr })

	xattrLGet = func(name, attr string) ([]byte, error) {
		return nil, errors.New("xattr not supported")
	}

	ofs := New(ctx, "test", Option{
		DigestXattrName: "user.sha256",
		CartFS:          cartfsClient,
	})
	if ofs.digestXattrName == "" {
		t.Skip("no xattr suppor")
	}

	relpath := "missing.txt"
	fname := filepath.Join(dir, relpath)

	fakeCartFS.errs[filepath.ToSlash(relpath)] = status.Error(codes.NotFound, "file not found in cartfs")

	_, err := ofs.FileDigestFromFS(ctx, fname, -1)
	if err == nil {
		t.Errorf("FileDigestFromFS got nil err, want error")
	}
}

func TestFileDigestFromFS_NoXattr_NoCartFS(t *testing.T) {
	ctx := t.Context()

	ofs := New(ctx, "test", Option{})

	_, err := ofs.FileDigestFromFS(ctx, "foo.txt", 10)
	if !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("FileDigestFromFS got err = %v, want %v", err, errors.ErrUnsupported)
	}
}
