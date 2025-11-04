// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package reapi

import (
	"context"

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/protobuf/proto"

	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/reapi/digest"
)

// FetchTree fetches trees at dirname identified by digest into digest store and returns its root directory.
func (c *Client) FetchTree(ctx context.Context, dirname string, d digest.Digest, ds *digest.Store) (*rpb.Directory, error) {
	b, err := c.Get(ctx, d, dirname)
	if err != nil {
		return nil, err
	}
	tree := &rpb.Tree{}
	err = proto.Unmarshal(b, tree)
	if err != nil {
		return nil, err
	}
	for _, c := range tree.Children {
		d, err := digest.FromProtoMessage(c)
		if err != nil {
			clog.Errorf(ctx, "digest for children %s: %v", c, err)
			continue
		}
		ds.Set(d)
	}
	return tree.Root, nil
}
