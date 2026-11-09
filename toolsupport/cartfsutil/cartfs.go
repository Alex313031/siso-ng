// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package cartfsutil

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	log "github.com/golang/glog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"go.chromium.org/build/hashigo/digest"

	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/reapi/merkletree"
	cartfspb "go.chromium.org/build/siso/toolsupport/cartfsutil/proto/server"
)

// Client is cartfs client.
type Client struct {
	dir    string
	conn   *grpc.ClientConn
	client cartfspb.CartfsClient
}

// New creates new cartfs client mounted at dir.
func New(ctx context.Context, endpoint string, opts ...grpc.DialOption) (*Client, error) {
	dopts := append([]grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}, opts...)
	conn, err := grpc.NewClient(endpoint, dopts...)
	if err != nil {
		clog.Warningf(ctx, "cartfs: failed to dial to cartfs server %s: %v", endpoint, err)
		return nil, err
	}
	clog.Infof(ctx, "cartfs connected to %s", endpoint)
	client := cartfspb.NewCartfsClient(conn)
	resp, err := client.GetState(ctx, &cartfspb.GetStateRequest{})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to get cartfs state: %w", err)
	}
	clog.Infof(ctx, "cartfs state: %v", resp)
	c := &Client{
		dir:    resp.MountPoint,
		conn:   conn,
		client: client,
	}
	return c, nil
}

// Close closes connection to cartfs server.
func (c *Client) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	conn := c.conn
	c.conn = nil
	return conn.Close()
}

// Urgency specifies urgency to register a file.
type Urgency int

const (
	urgencyUnspecified Urgency = iota
	UrgencyImmediate
	UrgencyOnAccess
)

// Registration is a registration for a file.
type Registration struct {
	Entry   merkletree.Entry
	Urgency Urgency

	// result of RegisterFiles
	Err error
}

// Digest returns digest of the file.
func (c *Client) Digest(ctx context.Context, fname string) (digest.Digest, error) {
	if c == nil || c.conn == nil || c.client == nil {
		return digest.Digest{}, errors.ErrUnsupported
	}
	relpath, err := filepath.Rel(c.dir, fname)
	if err != nil {
		return digest.Digest{}, fmt.Errorf("cartfs: get digest: out of dir: %q", fname)
	}
	if !filepath.IsLocal(relpath) {
		return digest.Digest{}, fmt.Errorf("cartfs: get digest: out of dir: %q", fname)
	}
	relpath = filepath.ToSlash(relpath)
	resp, err := c.client.GetDigest(ctx, &cartfspb.GetDigestRequest{
		Identifier: &cartfspb.GetDigestRequest_Path{
			Path: relpath,
		},
	})
	if err != nil {
		return digest.Digest{}, fmt.Errorf("cartfs: get digest %q: %w", fname, err)
	}
	return digest.Digest{
		Hash:      resp.Hash,
		SizeBytes: int64(resp.Size),
	}, nil
}

// RegisterFiles registers entries at dir.
func (c *Client) RegisterFiles(ctx context.Context, dir string, entries []*Registration) error {
	if c == nil || c.conn == nil || c.client == nil {
		return errors.ErrUnsupported
	}
	m := make(map[string]*Registration)
	req := &cartfspb.RegisterFilesRequest{}
	// limit the number of files in a request?
	for _, ent := range entries {
		if ent == nil {
			continue
		}
		fullpath := filepath.Join(dir, string(ent.Entry.Name))
		relpath, err := filepath.Rel(c.dir, fullpath)
		if log.V(1) {
			clog.Infof(ctx, "cartfs entry %q %q -> %q: %v", dir, ent.Entry.Name, relpath, err)
		}
		if err != nil {
			ent.Err = fmt.Errorf("cartfs: out of dir: %q", ent.Entry.Name)
			clog.Warningf(ctx, "%v", ent.Err)
			continue
		}
		if !filepath.IsLocal(relpath) {
			ent.Err = fmt.Errorf("cartfs: out of dir: %q", ent.Entry.Name)
			clog.Warningf(ctx, "%v", ent.Err)
			continue
		}
		relpath = filepath.ToSlash(relpath)
		m[relpath] = ent
		d := ent.Entry.Data.Digest()
		if d.IsZero() {
			ent.Err = fmt.Errorf("cartfs: empty digest: %q", ent.Entry.Name)
			clog.Warningf(ctx, "%v", ent.Err)
			continue
		}
		urgency := cartfspb.ContentPullUrgency_CONTENT_PULL_URGENCY_ON_ACCESS
		switch ent.Urgency {
		case UrgencyImmediate:
			urgency = cartfspb.ContentPullUrgency_CONTENT_PULL_URGENCY_IMMEDIATE
		case UrgencyOnAccess:
			urgency = cartfspb.ContentPullUrgency_CONTENT_PULL_URGENCY_ON_ACCESS
		}
		req.Registrations = append(req.Registrations, &cartfspb.FileRegistrationInfo{
			Path:         relpath,
			Hash:         d.Hash,
			Size:         uint64(d.SizeBytes),
			Urgency:      urgency,
			IsExecutable: ent.Entry.IsExecutable,
		})
	}
	resp, err := c.client.RegisterFiles(ctx, req)
	if err != nil {
		for _, ent := range m {
			ent.Err = err
		}
		return fmt.Errorf("cartfs register: %w", err)
	}
	for _, rerr := range resp.Errors {
		ent := m[rerr.Path]
		if ent != nil {
			ent.Err = fmt.Errorf("cartfs register failed: %s", rerr.ErrorMessage)
		}
	}
	return nil
}
