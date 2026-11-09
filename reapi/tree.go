// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package reapi

import (
	"context"
	"errors"

	"google.golang.org/protobuf/proto"

	"go.chromium.org/build/hashigo/digest"
	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/blob"
	"go.chromium.org/build/siso/o11y/clog"
)

// FetchTree fetches trees at dirname identified by digest into digest store and returns its root directory.
func (c *Client) FetchTree(ctx context.Context, dirname string, d digest.Digest, ds *blob.Store) (*rpb.Directory, error) {
	b, err := c.Get(ctx, d, dirname)
	if err != nil {
		return nil, err
	}
	return ParseTree(ctx, c.digestFn, b, ds)
}

// ParseTree unmarshals a serialized rpb.Tree, registers its child directories in ds, and returns the root directory.
func ParseTree(ctx context.Context, fn digest.Function, b []byte, ds *blob.Store) (*rpb.Directory, error) {
	tree := &rpb.Tree{}
	if err := proto.Unmarshal(b, tree); err != nil {
		return nil, err
	}
	for _, c := range tree.Children {
		d, err := blob.FromProtoMessage(fn, c)
		if err != nil {
			clog.Errorf(ctx, "digest for children %s: %v", c, err)
			continue
		}
		ds.Set(d)
	}
	if tree.Root == nil {
		return nil, errors.New("tree root is nil")
	}
	return tree.Root, nil
}
