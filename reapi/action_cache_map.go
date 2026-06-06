// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package reapi

import (
	"context"
	"iter"

	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/reapi/digest"
)

// ActionCacheMap is a mapping service from lookup key to actions,
// for two phase caching.
type ActionCacheMap struct {
	c *Client
}

// ActionCacheMap returns action cache map of the client.
// client should not be nil.
func (c *Client) ActionCacheMap() ActionCacheMap {
	return ActionCacheMap{
		c: c,
	}
}

// Add adds action to lookup key.
func (m ActionCacheMap) Add(ctx context.Context, lookupKey string, action digest.Digest) error {
	client := rpb.NewActionCacheClient(m.c.casConn)
	_, err := client.AddActionLookup(ctx, &rpb.AddActionLookupRequest{
		InstanceName: m.c.opt.Instance,
		LookupKey:    lookupKey,
		ActionDigest: action.Proto(),
	})
	return err
}

// List lists up actions associated to lookup key.
func (m ActionCacheMap) List(ctx context.Context, lookupKey string) iter.Seq2[*rpb.Action, error] {
	client := rpb.NewActionCacheClient(m.c.casConn)
	return func(yield func(*rpb.Action, error) bool) {
		// ListActions supports paging, but it might not worth to
		// check many actions in two phase caching since it would take
		// long time to check many actions.
		// so no need to support paging here?
		resp, err := client.ListActions(ctx, &rpb.ListActionsRequest{
			InstanceName: m.c.opt.Instance,
			LookupKey:    lookupKey,
		})
		if err != nil {
			yield(nil, err)
			return
		}
		for _, action := range resp.Actions {
			if !yield(action, nil) {
				return
			}
		}
	}
}
