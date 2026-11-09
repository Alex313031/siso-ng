// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package reapi

import (
	"context"
	"iter"

	"go.chromium.org/build/hashigo/digest"
	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/blob"
	"go.chromium.org/build/siso/o11y/clog"
)

// ActionCacheMap is action cache map used in two phase caching.
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
	if m.c.opt.LocalCache != nil {
		err := m.c.opt.LocalCache.AddActionLookup(ctx, lookupKey, action)
		if err != nil {
			clog.Warningf(ctx, "local addActionLookup %q %s: %v", lookupKey, action, err)
		}
	}
	if m.c.opt.DisableTwoPhaseCachingMethods {
		return nil
	}
	client := rpb.NewActionCacheClient(m.c.casConn)
	_, err := client.AddActionLookup(ctx, &rpb.AddActionLookupRequest{
		InstanceName:   m.c.opt.Instance,
		LookupKey:      lookupKey,
		ActionDigest:   action.Proto(),
		DigestFunction: m.c.digestFn.Value(),
	})
	return err
}

// List lists up actions associated to lookup key.
func (m ActionCacheMap) List(ctx context.Context, lookupKey string) iter.Seq2[*rpb.Action, error] {
	return func(yield func(*rpb.Action, error) bool) {
		seen := make(map[digest.Digest]struct{})
		if m.c.opt.LocalCache != nil {
			for d, err := range m.c.opt.LocalCache.ListActionDigests(ctx, lookupKey) {
				if err != nil {
					yield(nil, err)
					return
				}
				if _, ok := seen[d]; ok {
					continue
				}
				action := &rpb.Action{}
				err = m.c.Proto(ctx, d, action)
				if err == nil {
					seen[d] = struct{}{}
					if !yield(action, nil) {
						return
					}
					continue
				}
				clog.Warningf(ctx, "invalid action %s: %v", d, err)
			}
		}
		if m.c.opt.DisableTwoPhaseCachingMethods {
			return
		}
		client := rpb.NewActionCacheClient(m.c.casConn)
		// ListActions supports paging, but it might not worth to
		// check many actions in two phase caching since it would take
		// long time to check many actions.
		// so no need to support paging here?
		resp, err := client.ListActions(ctx, &rpb.ListActionsRequest{
			InstanceName:   m.c.opt.Instance,
			LookupKey:      lookupKey,
			DigestFunction: m.c.digestFn.Value(),
		})
		if err != nil {
			yield(nil, err)
			return
		}
		for _, action := range resp.Actions {
			actionData, err := blob.FromProtoMessage(m.c.digestFn, action)
			if err != nil {
				clog.Warningf(ctx, "invalid action %s: %v", action, err)
				continue
			}
			d := actionData.Digest()
			if _, ok := seen[d]; ok {
				continue
			}
			seen[d] = struct{}{}
			err = m.c.opt.LocalCache.SetProto(ctx, d, action)
			if err != nil {
				clog.Warningf(ctx, "store action %s: %v", d, err)
			}
			err = m.c.opt.LocalCache.AddActionLookup(ctx, lookupKey, d)
			if err != nil {
				clog.Warningf(ctx, "add action map %q -> %s: %v", lookupKey, d, err)
			}
			if !yield(action, nil) {
				return
			}
		}
	}
}
