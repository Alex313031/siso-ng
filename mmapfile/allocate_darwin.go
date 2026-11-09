// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build darwin

package mmapfile

import (
	"os"

	"golang.org/x/sys/unix"
)

func allocate(f *os.File, size int64) error {
	fstore := unix.Fstore_t{
		Flags:   unix.F_ALLOCATEALL, // Allocate all requested space or fail
		Posmode: unix.F_PEOFPOSMODE, // Allocate from physical EOF
		Offset:  0,
		Length:  size,
	}
	err := unix.FcntlFstore(f.Fd(), unix.F_PREALLOCATE, &fstore)
	if err != nil {
		return err
	}
	// Important macOS quirk: F_PREALLOCATE reserves space but
	// does not update the file size. You must call Ftruncate to apply it.
	return f.Truncate(size)
}
