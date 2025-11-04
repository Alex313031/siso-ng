// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package reapi

import (
	"context"
	"fmt"
	"io"

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	bpb "google.golang.org/genproto/googleapis/bytestream"

	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/reapi/bytestreamio"
	"go.chromium.org/build/siso/reapi/digest"
)

// CacheStore provides a thin wrapper around REAPI client that gets and uploads blobs and action results.
type CacheStore struct {
	client *Client
}

// CacheStore returns cache store of the client.
func (c *Client) CacheStore() CacheStore {
	return CacheStore{
		client: c,
	}
}

func (c CacheStore) String() string {
	return fmt.Sprintf("cachestore:reapi addr:%s instance:%s", c.client.opt.Address, c.client.opt.Instance)
}

// GetActionResult gets action result for the action identified by the digest.
func (c CacheStore) GetActionResult(ctx context.Context, d digest.Digest) (*rpb.ActionResult, error) {
	return c.client.GetActionResult(ctx, d)
}

// SetActionResult sets action result for the action identified by the digest.
// If a failing action is provided, caching will be skipped.
func (c CacheStore) SetActionResult(ctx context.Context, d digest.Digest, ar *rpb.ActionResult) error {
	// Intentionally no-op. Setting the action result could allow a malicious
	// actor to perform arbitrary code execution.
	// May reconsider in the future for trusted workers, but for now, do nothing.

	// Intentionally return no error, as it is intended for this function to be
	// called.
	return nil
}

// GetContent gets contents for the fname identified by the digest.
func (c CacheStore) GetContent(ctx context.Context, d digest.Digest, fname string) ([]byte, error) {
	return c.client.Get(ctx, d, fname)
}

// SetContent sets contents for the fname identified by the digest.
func (c CacheStore) SetContent(ctx context.Context, d digest.Digest, fname string, content []byte) error {
	data := digest.FromBytes(fname, content)
	if d != data.Digest() {
		return fmt.Errorf("digest mismatch: d=%s content=%s", d, data.Digest())
	}
	ds := digest.NewStore()
	ds.Set(data)
	_, err := c.client.UploadAll(ctx, ds)
	return err
}

// HasContent returns whether digest is in cache store.
func (c CacheStore) HasContent(ctx context.Context, d digest.Digest) bool {
	missing, err := c.client.Missing(ctx, []digest.Digest{d})
	if err != nil {
		clog.Warningf(ctx, "failed to call missing [%s]: %v", d, err)
		return false
	}
	return len(missing) != 1
}

// Source returns digest source for fname identified by the digest.
func (c CacheStore) Source(_ context.Context, d digest.Digest, fname string) digest.Source {
	return digestSource{
		c:     c.client,
		d:     d,
		fname: fname,
	}
}

type digestSourceReader struct {
	r io.ReadCloser
	n int
	c *Client
}

func (r *digestSourceReader) Read(buf []byte) (int, error) {
	n, err := r.r.Read(buf)
	r.n += n
	return n, err
}

func (r *digestSourceReader) Close() error {
	err := r.r.Close()
	r.c.m.ReadDone(r.n, err)
	return err
}

type digestSource struct {
	c     *Client
	d     digest.Digest
	fname string
}

func (s digestSource) Open(ctx context.Context) (io.ReadCloser, error) {
	r, err := bytestreamio.Open(ctx, bpb.NewByteStreamClient(s.c.casConn), s.c.resourceName(s.d))
	if err != nil {
		s.c.m.ReadDone(0, err)
		return nil, err
	}
	rd, err := s.c.newDecoder(r, s.d)
	if err != nil {
		s.c.m.ReadDone(0, err)
		return nil, err
	}
	return &digestSourceReader{r: rd, c: s.c}, err
}

func (s digestSource) String() string {
	return fmt.Sprintf("digest-data-in-cas:%s for %s", s.d, s.fname)
}
