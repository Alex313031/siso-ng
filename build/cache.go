// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"errors"
	"time"

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	log "github.com/golang/glog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"go.chromium.org/build/siso/build/cachestore"
	"go.chromium.org/build/siso/execute"
	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/o11y/iometrics"
	"go.chromium.org/build/siso/o11y/trace"
	"go.chromium.org/build/siso/reapi/digest"
	"go.chromium.org/build/siso/runtimex"
	"go.chromium.org/build/siso/sync/semaphore"
)

// CacheOptions is cache options.
type CacheOptions struct {
	Store      cachestore.CacheStore
	EnableRead bool
}

// Cache is a cache used in the builder.
type Cache struct {
	store      cachestore.CacheStore
	enableRead bool

	sema *semaphore.Semaphore

	m *iometrics.IOMetrics
}

// NewCache creates new cache.
func NewCache(ctx context.Context, opts CacheOptions) (*Cache, error) {
	clog.Infof(ctx, "cache store=%v read=%t",
		opts.Store,
		opts.EnableRead)
	if opts.Store == nil {
		return nil, errors.New("cache: store is not set")
	}
	return &Cache{
		store:      opts.Store,
		enableRead: opts.EnableRead,

		// TODO(b/274038010): cache-digest semaphore should share with execute/remotecache?
		sema: semaphore.New("cache-digest", runtimex.NumCPU()*10),

		m: iometrics.New("cache-content"),
	}, nil
}

// GetActionResult gets action result for the cmd from cache.
func (c *Cache) GetActionResult(ctx context.Context, cmd *execute.Cmd) error {
	now := time.Now()
	if c == nil || c.store == nil {
		return status.Error(codes.NotFound, "cache is not configured")
	}
	if !c.enableRead {
		return status.Error(codes.NotFound, "cache disable raed")
	}
	ctx, span := trace.NewSpan(ctx, "cache-get")
	defer span.Close(nil)

	var d digest.Digest
	err := c.sema.Do(ctx, func(ctx context.Context) error {
		var err error
		d, err = cmd.Digest(ctx, nil)
		return err
	})
	if err != nil {
		return err
	}
	rctx, getSpan := trace.NewSpan(ctx, "get-action-result")
	result, err := c.store.GetActionResult(rctx, d)
	getSpan.Close(nil)
	if err != nil {
		return err
	}
	if log.V(1) {
		clog.Infof(ctx, "cached result: %s", result)
	}

	// copy the action result into cmd.
	cmd.SetActionDigest(d)
	cmd.SetActionResult(result, true)
	err = c.setActionResultStdout(ctx, cmd, result)
	if err != nil {
		clog.Warningf(ctx, "cache-get (elapsed %s): failed to set stdout to action result: %v", time.Since(now), err)
		return err
	}
	err = c.setActionResultStderr(ctx, cmd, result)
	if err != nil {
		clog.Warningf(ctx, "cache-get (elapsed %s): failed to set stderr to action result: %v", time.Since(now), err)
		return err
	}
	err = cmd.RecordOutputs(ctx, c.store, now)
	if err != nil {
		clog.Warningf(ctx, "cache-get (elapsed %s): failed to record outputs from cache: %v", time.Since(now), err)
		return err
	}
	return nil
}

func (c *Cache) setActionResultStdout(ctx context.Context, cmd *execute.Cmd, result *rpb.ActionResult) error {
	w := cmd.StdoutWriter()
	if len(result.StdoutRaw) > 0 {
		w.Write(result.StdoutRaw)
		return nil
	}
	d := digest.FromProto(result.GetStdoutDigest())
	if d.SizeBytes == 0 {
		return nil
	}
	buf, err := c.store.GetContent(ctx, d, "stdout")
	if err != nil {
		return err
	}
	w.Write(buf)
	return nil
}

func (c *Cache) setActionResultStderr(ctx context.Context, cmd *execute.Cmd, result *rpb.ActionResult) error {
	w := cmd.StderrWriter()
	if len(result.StderrRaw) > 0 {
		w.Write(result.StderrRaw)
		return nil
	}
	d := digest.FromProto(result.GetStderrDigest())
	if d.SizeBytes == 0 {
		return nil
	}
	buf, err := c.store.GetContent(ctx, d, "stderr")
	if err != nil {
		return err
	}
	w.Write(buf)
	return nil
}
