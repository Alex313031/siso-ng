// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package hashfs

import (
	"context"
	"io/fs"
	"os"

	pb "go.chromium.org/build/siso/hashfs/proto"
)

// FSMonitor provides a way to access file info on the disk for the entry.
type FSMonitor interface {
	// ClockToken returns a token that represents the check time.
	ClockToken(context.Context) (string, error)

	// Scan returns FileInfoer from last checked token.
	Scan(context.Context, string) (FileInfoer, error)
}

// FileInfoer get fs.FileInfo for *pb.Entry from the disk.
type FileInfoer interface {
	FileInfo(context.Context, *pb.Entry) (fs.FileInfo, error)
}

type osfsInfoer struct{}

// FileInfo returns file info of the entry.
func (osfsInfoer) FileInfo(ctx context.Context, ent *pb.Entry) (fs.FileInfo, error) {
	return os.Lstat(ent.Name)
}
