// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package artfsutil

import (
	"context"
	"errors"
	"path/filepath"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/reapi/merkletree"
	artfspb "go.chromium.org/build/siso/toolsupport/artfsutil/proto/artfs"
	manifestpb "go.chromium.org/build/siso/toolsupport/artfsutil/proto/manifest"
)

// Client is artfs client.
type Client struct {
	dir    string
	conn   *grpc.ClientConn
	client artfspb.ArtfsClient
}

// New creates new artfs client mounted at dir.
func New(ctx context.Context, dir, endpoint string) (*Client, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, err
	}
	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		clog.Warningf(ctx, "artfs: failed to dial to artfs server %s: %v", endpoint, err)
		return nil, err
	}
	clog.Infof(ctx, "artfs on %s connected to %s", dir, endpoint)
	c := &Client{
		dir:    dir,
		conn:   conn,
		client: artfspb.NewArtfsClient(conn),
	}
	return c, nil
}

// Close closes connection to artfs server.
func (c *Client) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	conn := c.conn
	c.conn = nil
	return conn.Close()
}

// ArtfsInsert inserts entries at dir.
func (c *Client) ArtfsInsert(ctx context.Context, dir string, entries []merkletree.Entry) error {
	if c == nil || c.conn == nil || c.client == nil {
		return errors.ErrUnsupported
	}
	s, err := c.client.AddCasFiles(ctx)
	if err != nil {
		return err
	}
	for _, ent := range entries {
		fullpath := filepath.Join(dir, ent.Name)
		relpath, err := filepath.Rel(c.dir, fullpath)
		if err != nil {
			clog.Warningf(ctx, "artfs: out of dir: %s", ent.Name)
			continue
		}
		if !filepath.IsLocal(relpath) {
			clog.Warningf(ctx, "artfs: out of dir: %s", ent.Name)
			continue
		}
		d := ent.Data.Digest()
		err = s.Send(&manifestpb.FileManifest{
			Digest: &manifestpb.Digest{
				Hash:      d.Hash,
				SizeBytes: d.SizeBytes,
			},
			Path:         relpath,
			IsExecutable: ent.IsExecutable,
		})
		if err != nil {
			clog.Warningf(ctx, "artfs: failed to add %s: %v", ent.Name, err)
		}
	}
	res, err := s.CloseAndRecv()
	clog.Infof(ctx, "artfs: insert %s: %v", res, err)
	return err
}
