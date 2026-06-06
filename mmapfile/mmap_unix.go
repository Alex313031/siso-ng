// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build unix

package mmapfile

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// Read maps path read-only into memory. The returned slice is page-cache
// backed and stays valid until Unmap is called. An empty file returns
// (nil, nil).
func Read(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := fi.Size()
	if size == 0 {
		return nil, nil
	}
	data, err := unix.Mmap(int(f.Fd()), 0, int(size), unix.PROT_READ, unix.MAP_PRIVATE)
	if err != nil {
		return nil, fmt.Errorf("mmap %s: %w", path, err)
	}
	return data, nil
}

// Unmap releases a mapping returned by Read. A nil/empty slice is a no-op,
// matching Read's empty-file return.
func Unmap(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	return unix.Munmap(data)
}

// Write truncates f to size bytes and maps it read-write. The caller
// writes into the returned slice, then calls closer to unmap and close f.
// On any error from the mmap path, f is closed.
func Write(f *os.File, size int) (data []byte, closer func() error, retErr error) {
	defer func() {
		if retErr != nil {
			f.Close()
		}
	}()

	if err := f.Truncate(int64(size)); err != nil {
		return nil, nil, fmt.Errorf("truncate %s: %w", f.Name(), err)
	}

	data, err := unix.Mmap(int(f.Fd()), 0, size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		return nil, nil, fmt.Errorf("mmap %s: %w", f.Name(), err)
	}

	closer = func() error {
		if err := unix.Munmap(data); err != nil {
			f.Close()
			return fmt.Errorf("munmap %s: %w", f.Name(), err)
		}
		return f.Close()
	}
	return data, closer, nil
}
