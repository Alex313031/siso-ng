// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build linux

package mmapfile

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func allocate(f *os.File, size int64) error {
	err := unix.Fallocate(int(f.Fd()), 0, 0, size)
	if err != nil && (errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EOPNOTSUPP)) {
		// truncate doesn't allocate backing store, to risk to
		// panic when touching the file when disk full.
		return f.Truncate(size)
	}
	return err
}
