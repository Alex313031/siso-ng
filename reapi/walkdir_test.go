// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package reapi_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/experimental"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"go.chromium.org/build/hashigo/digest"
	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/auth/cred"
	"go.chromium.org/build/siso/blob"
	"go.chromium.org/build/siso/path"
	"go.chromium.org/build/siso/reapi"
	"go.chromium.org/build/siso/reapi/merkletree"
	"go.chromium.org/build/siso/reapi/reapitest"
)

// casSpy counts the CAS reads WalkDir makes.
type casSpy struct {
	reads atomic.Int32
}

func (s *casSpy) dialOptions() []grpc.DialOption {
	return []grpc.DialOption{
		grpc.WithChainUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			err := invoker(ctx, method, req, reply, cc, opts...)
			if strings.HasSuffix(method, "/BatchReadBlobs") {
				s.reads.Add(1)
			}
			return err
		}),
		grpc.WithChainStreamInterceptor(func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
			if strings.HasSuffix(method, "/GetTree") {
				s.reads.Add(1)
			}
			return streamer(ctx, desc, cc, method, opts...)
		}),
	}
}

// walk walks the tree d and returns a summary of each directory passed
// to the WalkDir callback, keyed by its name.
func walk(ctx context.Context, cl *reapi.Client, d digest.Digest) (map[string]string, error) {
	return walkHook(ctx, cl, d, nil)
}

// walkHook is walk, but also calls hook, if not nil, for each directory.
func walkHook(ctx context.Context, cl *reapi.Client, d digest.Digest, hook func(dname string, dir *rpb.Directory) error) (map[string]string, error) {
	got := make(map[string]string)
	// refs maps each directory name to the digest its parent refers to
	// it by. WalkDir visits a parent before its subdirectories.
	refs := map[string]digest.Digest{"": d}
	err := cl.WalkDir(ctx, d, func(dname string, dd digest.Digest, dir *rpb.Directory) error {
		// dd must be the digest dir is referred to by (d for the root),
		// which is not necessarily the digest of re-marshalling dir.
		if want, ok := refs[dname]; !ok || dd != want {
			return fmt.Errorf("dir %q: dd=%s; want %s", dname, dd, want)
		}
		if _, ok := got[dname]; ok {
			return fmt.Errorf("dir %q visited twice", dname)
		}
		if hook != nil {
			if err := hook(dname, dir); err != nil {
				return err
			}
		}
		var files, dirs, symlinks []string
		for _, file := range dir.Files {
			files = append(files, file.GetName())
		}
		for _, subdir := range dir.Directories {
			dirs = append(dirs, subdir.GetName())
			subname := subdir.GetName()
			if dname != "" {
				subname = dname + "/" + subname
			}
			refs[subname] = digest.FromProto(subdir.GetDigest())
		}
		for _, symlink := range dir.Symlinks {
			symlinks = append(symlinks, symlink.GetName())
		}
		got[dname] = fmt.Sprintf("files=%q dirs=%q symlinks=%q", files, dirs, symlinks)
		return nil
	})
	return got, err
}

func uploadTree(ctx context.Context, t *testing.T, cl *reapi.Client, files ...string) digest.Digest {
	t.Helper()
	ds := blob.NewStore()
	tree := merkletree.New(digest.SHA256, ds)
	for _, s := range files {
		tree.Set(merkletree.Entry{
			Name: path.Path(s),
			Data: blob.FromBytes(digest.SHA256, "empty", nil),
		})
	}
	d, err := tree.Build(ctx)
	if err != nil {
		t.Fatal(err)
	}
	n, err := cl.UploadAll(ctx, ds)
	if err != nil {
		t.Fatalf("UploadAll()=%d, %v; want _, nil", n, err)
	}
	return d
}

func TestWalkDir(t *testing.T) {
	ctx := t.Context()
	want := map[string]string{
		"":                  `files=["file1"] dirs=["subdir1" "subdir2"] symlinks=[]`,
		"subdir1":           `files=["file1"] dirs=[] symlinks=[]`,
		"subdir2":           `files=["file1"] dirs=["subdir2.1" "subdir2.2"] symlinks=[]`,
		"subdir2/subdir2.1": `files=["file1"] dirs=[] symlinks=[]`,
		"subdir2/subdir2.2": `files=["file1"] dirs=[] symlinks=[]`,
	}
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			var spy casSpy
			cl := reapitest.NewWithOption(ctx, t, &reapitest.Fake{}, reapi.Option{WalkDirStream: stream}, spy.dialOptions()...)
			d := uploadTree(ctx, t, cl,
				"file1",
				"subdir1/file1",
				"subdir2/file1",
				"subdir2/subdir2.1/file1",
				"subdir2/subdir2.2/file1")

			// The first walk reads from CAS and fills the walk cache.
			// The second is served from the cache, by digest.
			for _, pass := range []string{"fetch", "cached"} {
				before := spy.reads.Load()
				got, err := walk(ctx, cl, d)
				if err != nil {
					t.Fatalf("%s: WalkDir=%v; want nil", pass, err)
				}
				if diff := cmp.Diff(want, got); diff != "" {
					t.Errorf("%s: WalkDir diff -want +got:\n%s", pass, diff)
				}
				reads := spy.reads.Load() - before
				if (pass == "cached") != (reads == 0) {
					t.Errorf("%s: %d CAS reads", pass, reads)
				}
			}
		})
	}
}

// treeCAS is a CAS server that serves only GetTree, from trees.
type treeCAS struct {
	rpb.UnimplementedContentAddressableStorageServer

	// trees has the responses to send for each root digest.
	trees map[digest.Digest][]*rpb.GetTreeResponse
	// first, if set, is the only response to the first call.
	first *rpb.GetTreeResponse
	calls atomic.Int32
}

func (s *treeCAS) GetTree(req *rpb.GetTreeRequest, stream grpc.ServerStreamingServer[rpb.GetTreeResponse]) error {
	// RBE rejects other content-types, e.g. "application/grpc+proto",
	// which WalkDir's GetTree codec must not cause.
	md, _ := metadata.FromIncomingContext(stream.Context())
	if ct := md.Get("content-type"); len(ct) != 1 || ct[0] != "application/grpc" {
		return status.Errorf(codes.Unimplemented, "unexpected content-type %q", ct)
	}
	if s.calls.Add(1) == 1 && s.first != nil {
		return stream.Send(s.first)
	}
	resps, ok := s.trees[digest.FromProto(req.RootDigest)]
	if !ok {
		return status.Errorf(codes.NotFound, "no tree %v", req.RootDigest)
	}
	for _, resp := range resps {
		if err := stream.Send(resp); err != nil {
			return err
		}
	}
	return nil
}

// poisonPool is a mem.BufferPool that overwrites buffers returned to it,
// so that reading a buffer after it was freed gives garbage.
type poisonPool struct{}

func (poisonPool) Get(n int) *[]byte {
	b := make([]byte, n)
	return &b
}

func (poisonPool) Put(b *[]byte) {
	for i := range *b {
		(*b)[i] = 0xff
	}
}

// newTreeClient returns a client that walks with GetTree on cas, and
// whose received buffers are overwritten once grpc frees them.
func newTreeClient(t *testing.T, cas *treeCAS) *reapi.Client {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	rpb.RegisterContentAddressableStorageServer(srv, cas)
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		experimental.WithBufferPool(poisonPool{}),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	cl, err := reapi.NewFromConn(t.Context(), reapi.Option{WalkDirStream: true}, cred.Cred{}, conn, conn, conn)
	if err != nil {
		t.Fatalf("NewFromConn: %v", err)
	}
	t.Cleanup(func() { cl.Close() })
	return cl
}

// fileNodes returns empty files with names.
func fileNodes(names ...string) []*rpb.FileNode {
	var files []*rpb.FileNode
	for _, name := range names {
		files = append(files, &rpb.FileNode{Name: name, Digest: digest.SHA256.Empty().Proto()})
	}
	return files
}

// dirDigest returns the digest of dir.
func dirDigest(t *testing.T, dir *rpb.Directory) digest.Digest {
	t.Helper()
	b, err := proto.Marshal(dir)
	if err != nil {
		t.Fatal(err)
	}
	return digest.SHA256.FromBytes(b)
}

// dirNode returns the node for dir named name.
func dirNode(t *testing.T, name string, dir *rpb.Directory) *rpb.DirectoryNode {
	t.Helper()
	return &rpb.DirectoryNode{Name: name, Digest: dirDigest(t, dir).Proto()}
}

// TestWalkDirRepeatedDirectories checks WalkDir on a GetTree stream that
// sends a directory once for every place it occurs, as RBE does, over
// several messages larger than 1 KiB (so grpc hands them over in pooled
// buffers, which are freed after use), with empty directories.
func TestWalkDirRepeatedDirectories(t *testing.T) {
	ctx := t.Context()
	// Each non-empty directory has enough long file names to be over
	// 1 KiB.
	var names []string
	for i := range 10 {
		names = append(names, fmt.Sprintf("file-with-a-long-name-for-a-large-directory-%02d", i))
	}
	x := &rpb.Directory{Files: fileNodes(names...)}
	a := &rpb.Directory{Files: fileNodes(append(slices.Clone(names), "only")...), Directories: []*rpb.DirectoryNode{dirNode(t, "x", x)}}
	b := &rpb.Directory{Files: fileNodes(names...), Directories: []*rpb.DirectoryNode{dirNode(t, "x", x)}}
	e := &rpb.Directory{}
	root := &rpb.Directory{
		Files: fileNodes(names...),
		Directories: []*rpb.DirectoryNode{
			dirNode(t, "a0", a), dirNode(t, "b0", b), dirNode(t, "b1", b), dirNode(t, "e0", e), dirNode(t, "e1", e),
		},
	}
	cas := &treeCAS{trees: map[digest.Digest][]*rpb.GetTreeResponse{
		dirDigest(t, root): {
			{Directories: []*rpb.Directory{root, a, b}},
			{Directories: []*rpb.Directory{b, e, e}},
			{Directories: []*rpb.Directory{x, x, x}},
		},
		dirDigest(t, b): {
			{Directories: []*rpb.Directory{b, x}},
		},
	}}
	cl := newTreeClient(t, cas)
	q := func(fs ...string) string {
		return fmt.Sprintf("%q", fs)
	}
	big := q(names...)
	want := map[string]string{
		"":     "files=" + big + ` dirs=["a0" "b0" "b1" "e0" "e1"] symlinks=[]`,
		"a0":   "files=" + q(append(slices.Clone(names), "only")...) + ` dirs=["x"] symlinks=[]`,
		"a0/x": "files=" + big + " dirs=[] symlinks=[]",
		"b0":   "files=" + big + ` dirs=["x"] symlinks=[]`,
		"b0/x": "files=" + big + " dirs=[] symlinks=[]",
		"b1":   "files=" + big + ` dirs=["x"] symlinks=[]`,
		"b1/x": "files=" + big + " dirs=[] symlinks=[]",
		"e0":   "files=[] dirs=[] symlinks=[]",
		"e1":   "files=[] dirs=[] symlinks=[]",
	}

	// While WalkDir handles a0, after root listed b0 as not cached,
	// another walk caches b. WalkDir then passes that walk's copy for b0
	// rather than decoding its own.
	var cachedB, gotB0 *rpb.Directory
	got, err := walkHook(ctx, cl, dirDigest(t, root), func(dname string, dir *rpb.Directory) error {
		switch dname {
		case "a0":
			return cl.WalkDir(ctx, dirDigest(t, b), func(dname string, _ digest.Digest, dir *rpb.Directory) error {
				if dname == "" {
					cachedB = dir
				}
				return nil
			})
		case "b0":
			gotB0 = dir
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir=%v; want nil", err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("WalkDir diff -want +got:\n%s", diff)
	}
	if cachedB == nil || gotB0 != cachedB {
		t.Errorf("WalkDir passed b0 %p; want the cached copy %p", gotB0, cachedB)
	}
}

// TestWalkDirChildBeforeParent checks that WalkDir handles a GetTree
// stream that sends directories before the parents that list them.
func TestWalkDirChildBeforeParent(t *testing.T) {
	ctx := t.Context()
	sub21 := &rpb.Directory{Files: fileNodes("file1")}
	sub2 := &rpb.Directory{Files: fileNodes("file1"), Directories: []*rpb.DirectoryNode{dirNode(t, "subdir2.1", sub21)}}
	sub1 := &rpb.Directory{Files: fileNodes("file1", "file2")}
	root := &rpb.Directory{Files: fileNodes("file1"), Directories: []*rpb.DirectoryNode{dirNode(t, "subdir1", sub1), dirNode(t, "subdir2", sub2)}}
	cas := &treeCAS{trees: map[digest.Digest][]*rpb.GetTreeResponse{
		dirDigest(t, root): {
			{Directories: []*rpb.Directory{sub21, sub2, sub1, root}},
		},
	}}
	cl := newTreeClient(t, cas)
	want := map[string]string{
		"":                  `files=["file1"] dirs=["subdir1" "subdir2"] symlinks=[]`,
		"subdir1":           `files=["file1" "file2"] dirs=[] symlinks=[]`,
		"subdir2":           `files=["file1"] dirs=["subdir2.1"] symlinks=[]`,
		"subdir2/subdir2.1": `files=["file1"] dirs=[] symlinks=[]`,
	}
	got, err := walk(ctx, cl, dirDigest(t, root))
	if err != nil {
		t.Fatalf("WalkDir=%v; want nil", err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("WalkDir diff -want +got:\n%s", diff)
	}
}

// TestWalkDirRetriesBadResponse checks that WalkDir retries a GetTree
// response it cannot decode, as it did when grpc decoded it.
func TestWalkDirRetriesBadResponse(t *testing.T) {
	// A directories field whose length runs past the end.
	badResp := &rpb.GetTreeResponse{}
	badResp.ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 1, protowire.BytesType), 5))
	// A directory with a field tag but no value.
	badDir := &rpb.Directory{}
	badDir.ProtoReflect().SetUnknown(protowire.AppendTag(nil, 1, protowire.VarintType))
	for _, tc := range []struct {
		name  string
		first *rpb.GetTreeResponse
	}{
		{name: "response", first: badResp},
		{name: "directory", first: &rpb.GetTreeResponse{Directories: []*rpb.Directory{badDir}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			sub := &rpb.Directory{Files: fileNodes("file1")}
			root := &rpb.Directory{Files: fileNodes("file1"), Directories: []*rpb.DirectoryNode{dirNode(t, "subdir1", sub)}}
			cas := &treeCAS{
				first: tc.first,
				trees: map[digest.Digest][]*rpb.GetTreeResponse{
					dirDigest(t, root): {{Directories: []*rpb.Directory{root, sub}}},
				},
			}
			cl := newTreeClient(t, cas)
			if _, err := walk(ctx, cl, dirDigest(t, root)); err != nil {
				t.Fatalf("WalkDir=%v; want nil", err)
			}
			if got := cas.calls.Load(); got != 2 {
				t.Errorf("GetTree calls=%d; want 2", got)
			}
		})
	}
}

// TestWalkDirParallel checks that concurrent walks, which reuse each
// other's maps through a pool, each see only their own tree.
func TestWalkDirParallel(t *testing.T) {
	ctx := t.Context()
	cl := reapitest.NewWithOption(ctx, t, &reapitest.Fake{}, reapi.Option{WalkDirStream: true})
	const trees = 8
	var roots []digest.Digest
	var wants []map[string]string
	for i := range trees {
		top := fmt.Sprintf("t%d", i)
		roots = append(roots, uploadTree(ctx, t, cl, top+"/file", top+"/sub/file", "shared/file"))
		wants = append(wants, map[string]string{
			"":           fmt.Sprintf(`files=[] dirs=["shared" %q] symlinks=[]`, top),
			"shared":     `files=["file"] dirs=[] symlinks=[]`,
			top:          `files=["file"] dirs=["sub"] symlinks=[]`,
			top + "/sub": `files=["file"] dirs=[] symlinks=[]`,
		})
	}
	// The first round streams the trees, later rounds are served from
	// the walk cache.
	for round := range 3 {
		var wg sync.WaitGroup
		for i, d := range roots {
			wg.Go(func() {
				got, err := walk(ctx, cl, d)
				if err != nil {
					t.Errorf("round %d tree %d: WalkDir=%v; want nil", round, i, err)
					return
				}
				if diff := cmp.Diff(wants[i], got); diff != "" {
					t.Errorf("round %d tree %d: WalkDir diff -want +got:\n%s", round, i, diff)
				}
			})
		}
		wg.Wait()
	}
}

// TestWalkDirAfterAbortedWalk checks that a walk does not see the names
// an aborted walk left for subdirectories it never visited. Two phase
// caching aborts most walks, at the first mismatch.
func TestWalkDirAfterAbortedWalk(t *testing.T) {
	ctx := t.Context()
	cl := reapitest.NewWithOption(ctx, t, &reapitest.Fake{}, reapi.Option{WalkDirStream: true})
	errAbort := errors.New("abort")
	// walkStatePool may drop a state, so repeat until a walk has
	// likely reused the aborted walk's state.
	for i := range 10 {
		y := fmt.Sprintf("y%d", i)
		// c in the second tree has the same digest as b in the first.
		d1 := uploadTree(ctx, t, cl, fmt.Sprintf("a/x%d", i), "b/"+y)
		d2 := uploadTree(ctx, t, cl, "c/"+y)
		err := cl.WalkDir(ctx, d1, func(dname string, _ digest.Digest, _ *rpb.Directory) error {
			if dname == "a" {
				return errAbort
			}
			return nil
		})
		if !errors.Is(err, errAbort) {
			t.Fatalf("WalkDir=%v; want %v", err, errAbort)
		}
		got, err := walk(ctx, cl, d2)
		if err != nil {
			t.Fatalf("WalkDir=%v; want nil", err)
		}
		want := map[string]string{
			"":  `files=[] dirs=["c"] symlinks=[]`,
			"c": fmt.Sprintf(`files=[%q] dirs=[] symlinks=[]`, y),
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("WalkDir diff -want +got:\n%s", diff)
		}
	}
}
