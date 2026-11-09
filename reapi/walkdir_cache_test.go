// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package reapi_test

import (
	"context"
	"fmt"
	"testing"

	"google.golang.org/protobuf/proto"

	"go.chromium.org/build/hashigo/digest"
	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/reapi"
	"go.chromium.org/build/siso/reapi/reapitest"
)

// newWalkDirClient returns a client whose CAS holds a small tree
// with 5 directories, and the tree's root digest.
func newWalkDirClient(ctx context.Context, t *testing.T, opt reapi.Option) (*reapi.Client, digest.Digest) {
	t.Helper()
	cl := reapitest.NewWithOption(ctx, t, &reapitest.Fake{}, opt)
	return cl, uploadTree(ctx, t, cl, "file1", "a/file1", "b/file1", "b/c/file1", "b/d/file2")
}

// TestWalkDirSharesCachedDirectories checks that walks served from the
// cache hand out the cached messages themselves, and that walking does
// not modify them.
func TestWalkDirSharesCachedDirectories(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			ctx := t.Context()
			cl, d := newWalkDirClient(ctx, t, reapi.Option{WalkDirStream: stream})
			walk := func() map[string]*rpb.Directory {
				t.Helper()
				dirs := make(map[string]*rpb.Directory)
				err := cl.WalkDir(ctx, d, func(dname string, _ digest.Digest, dir *rpb.Directory) error {
					dirs[dname] = dir
					return nil
				})
				if err != nil {
					t.Fatalf("WalkDir=%v; want nil", err)
				}
				return dirs
			}
			first := walk()
			orig := make(map[string]*rpb.Directory)
			for dname, dir := range first {
				orig[dname] = proto.Clone(dir).(*rpb.Directory)
			}
			for range 2 {
				got := walk()
				if len(got) != len(first) {
					t.Errorf("walked %d dirs; want %d", len(got), len(first))
				}
				for dname, dir := range got {
					if dir != first[dname] {
						t.Errorf("%q: cached walk returned a different message", dname)
					}
					if !proto.Equal(dir, orig[dname]) {
						t.Errorf("%q: directory modified by walk: %v; want %v", dname, dir, orig[dname])
					}
				}
			}
		})
	}
}
