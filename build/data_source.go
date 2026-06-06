// Copyright 2025 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"go.chromium.org/build/siso/auth/cred"
	"go.chromium.org/build/siso/build/cachestore"
	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/reapi"
	"go.chromium.org/build/siso/reapi/digest"
)

func NewDataSource(ctx context.Context, credential cred.Cred, localCacheEnable bool, cacheDir string, reapiClient *reapi.Client) DataSource {
	layeredCache := NewLayeredCache()
	if localCacheEnable {
		cache, err := NewLocalCache(cacheDir)
		if err != nil {
			clog.Warningf(ctx, "failed to create local cache - no local cache enabled: %v", err)
		} else {
			layeredCache.AddLayer(cache)
			cache.GarbageCollectIfRequired(ctx)
		}
	}
	if reapiClient != nil {
		layeredCache.AddLayer(reapiClient.CacheStore())
	}
	var ds DataSource
	ds.Client = reapiClient
	ds.Cache = layeredCache
	return ds
}

// DataSource provides access to build data, potentially from a local cache or a remote reapi client.
type DataSource struct {
	Cache  cachestore.CacheStore
	Client *reapi.Client
}

// Close closes the underlying reapi client, if it exists.
func (ds DataSource) Close(ctx context.Context) error {
	if ds.Client == nil {
		return nil
	}
	return ds.Client.Close()
}

// DigestData creates a new digest.Data from the given digest and filename,
// using the DataSource to retrieve the actual data.
func (ds DataSource) DigestData(ctx context.Context, d digest.Digest, fname string) digest.Data {
	return digest.NewData(ds.Source(ctx, d, fname), d)
}

// Source returns a digest.Source for the given digest and filename.
func (ds DataSource) Source(_ context.Context, d digest.Digest, fname string) digest.Source {
	return source{
		dataSource: ds,
		d:          d,
		fname:      fname,
	}
}

type source struct {
	dataSource DataSource
	d          digest.Digest
	fname      string
}

func (s source) Open(ctx context.Context) (io.ReadCloser, error) {
	var r io.ReadCloser
	var err error
	if s.dataSource.Cache != nil {
		src := s.dataSource.Cache.Source(ctx, s.d, s.fname)
		if src != nil {
			r, err = src.Open(ctx)
			if err == nil {
				return r, nil
			}
		}
		// fallback
	}
	if s.dataSource.Client != nil {
		r, err := s.dataSource.Client.GetReader(ctx, s.d, s.fname)
		if err == nil {
			return r, nil
		}
		// fallback
	}
	// ctx may be deadline exceeded or canceled.
	// if so, return such error.
	// DeadlineExceeded would trigger retry in hashfs flush.
	if ctx.Err() != nil {
		return nil, context.Cause(ctx)
	}
	// siso process runs at some directory, but
	// s.fname may not be relative to the working directory.
	// Actually, it is workspace relative if it is created by
	// *Cmd.entriesFromResult, and failed to open as such path
	// doesn't exist. return with better error message.
	if !filepath.IsAbs(s.fname) {
		return nil, fmt.Errorf("failed to fetch source %v for %q: %w", s.d, s.fname, err)
	}
	// no reapi configured. use local file?
	f, err := os.Open(s.fname)
	return f, err
}

func (s source) String() string {
	return fmt.Sprintf("dataSource:%s", s.fname)
}
