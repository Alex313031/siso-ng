// Copyright 2025 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"errors"
	"fmt"
	"io"

	"go.chromium.org/build/hashigo/digest"

	"go.chromium.org/build/siso/blob"
	"go.chromium.org/build/siso/build/cachestore"
	"go.chromium.org/build/siso/reapi"
)

func NewDataSource(ctx context.Context, localCache *reapi.LocalCache, reapiClient *reapi.Client) DataSource {
	var ds DataSource
	ds.Client = reapiClient
	if localCache != nil {
		localCache.GarbageCollectIfRequired(ctx)
		ds.Cache = localCache
	}
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

// CacheStore returns cachestore for this data store.
func (ds DataSource) CacheStore() cachestore.CacheStore {
	if ds.Client != nil {
		return ds.Client.CacheStore()
	}
	return ds.Cache
}

// DigestData creates a new blob.Data from the given digest and filename,
// using the DataSource to retrieve the actual data.
func (ds DataSource) DigestData(ctx context.Context, d digest.Digest, fname string) blob.Data {
	return blob.NewData(ds.Source(ctx, d, fname), d)
}

// Source returns a blob.Source for the given digest and filename.
func (ds DataSource) Source(_ context.Context, d digest.Digest, fname string) blob.Source {
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
		r, err = s.dataSource.Client.GetReader(ctx, s.d, s.fname)
		if err == nil {
			return r, nil
		}
	}
	// ctx may be deadline exceeded or canceled.
	// if so, return such error.
	// DeadlineExceeded would trigger retry in hashfs flush.
	if ctx.Err() != nil {
		return nil, context.Cause(ctx)
	}
	if err != nil {
		return nil, err
	}
	return nil, errors.New("no client or cache in datasource")
}

func (s source) String() string {
	return fmt.Sprintf("dataSource:%s", s.fname)
}
