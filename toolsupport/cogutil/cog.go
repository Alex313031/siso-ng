// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package cogutil

import (
	"context"
	"errors"
	"os"
	"strings"

	"go.chromium.org/build/siso/o11y/clog"
)

// Client is cogfs client.
type Client struct {
}

// New creates new cog fs client at dir.
func New(ctx context.Context, dir string) (*Client, error) {
	if !strings.HasPrefix(dir, "/google/cog/") {
		return nil, errors.ErrUnsupported
	}

	buf, err := os.ReadFile("/google/cog/status/version")
	if err != nil {
		return nil, err
	}
	clog.Infof(ctx, "cog version:\n%s", string(buf))
	// cog api endpoint is fmt.Sprintf("unix:///google/cog/status/uds/%d", os.Getuid())
	// TODO? use cog API?
	return &Client{}, nil
}

// Info returns cog supported status.
func (c *Client) Info() string {
	if c == nil {
		return "cog disabled"
	}
	return "cog enabled"
}

// Close closes connection to cogfs server.
func (c *Client) Close() error {
	return nil
}
