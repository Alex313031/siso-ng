// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build darwin

package osfs

import (
	"context"
	"time"

	"golang.org/x/sys/unix"
)

// Clonefile copies src to dst by using cloneline.
func (fs *OSFS) Clonefile(ctx context.Context, src, dst string) error {
	started := time.Now()
	err := unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW)
	fs.OpsDone(err)
	if dur := time.Since(started); dur > 1*time.Minute {
		logSlow(ctx, dst, dur, err)
	}
	return err
}
