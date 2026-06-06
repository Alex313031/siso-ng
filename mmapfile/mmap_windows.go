// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build windows

package mmapfile

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
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
	h, err := windows.CreateFileMapping(windows.Handle(f.Fd()), nil, windows.PAGE_READONLY, 0, 0, nil)
	if err != nil {
		return nil, fmt.Errorf("CreateFileMapping %s: %w", path, err)
	}
	addr, err := windows.MapViewOfFile(h, windows.FILE_MAP_READ, 0, 0, 0)
	if err != nil {
		windows.CloseHandle(h)
		return nil, fmt.Errorf("MapViewOfFile %s: %w", path, err)
	}
	// The mapping stays alive until all views are unmapped, so we can
	// close the mapping handle now.
	if err := windows.CloseHandle(h); err != nil {
		windows.UnmapViewOfFile(addr)
		return nil, fmt.Errorf("CloseHandle %s: %w", path, err)
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(addr)), int(size)), nil
}

// Unmap releases a mapping returned by Read. A nil/empty slice is a no-op,
// matching Read's empty-file return.
func Unmap(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	return windows.UnmapViewOfFile(uintptr(unsafe.Pointer(&data[0])))
}

// Write truncates f to size bytes and maps it read-write. The caller
// writes into the returned slice, then calls closer to flush, unmap, and
// close f. On any error from the mmap path, f is closed.
func Write(f *os.File, size int) (data []byte, closer func() error, retErr error) {
	defer func() {
		if retErr != nil {
			f.Close()
		}
	}()

	if err := f.Truncate(int64(size)); err != nil {
		return nil, nil, fmt.Errorf("truncate %s: %w", f.Name(), err)
	}

	high := uint32(uint64(size) >> 32)
	low := uint32(uint64(size) & 0xFFFFFFFF)
	h, err := windows.CreateFileMapping(windows.Handle(f.Fd()), nil, windows.PAGE_READWRITE, high, low, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("CreateFileMapping %s: %w", f.Name(), err)
	}

	addr, err := windows.MapViewOfFile(h, windows.FILE_MAP_READ|windows.FILE_MAP_WRITE, 0, 0, 0)
	if err != nil {
		windows.CloseHandle(h)
		return nil, nil, fmt.Errorf("MapViewOfFile %s: %w", f.Name(), err)
	}

	data = unsafe.Slice((*byte)(unsafe.Pointer(addr)), size)

	closer = func() error {
		if err := windows.FlushViewOfFile(addr, 0); err != nil {
			windows.UnmapViewOfFile(addr)
			windows.CloseHandle(h)
			f.Close()
			return fmt.Errorf("FlushViewOfFile %s: %w", f.Name(), err)
		}
		if err := windows.UnmapViewOfFile(addr); err != nil {
			windows.CloseHandle(h)
			f.Close()
			return fmt.Errorf("UnmapViewOfFile %s: %w", f.Name(), err)
		}
		if err := windows.CloseHandle(h); err != nil {
			f.Close()
			return fmt.Errorf("CloseHandle %s: %w", f.Name(), err)
		}
		return f.Close()
	}
	return data, closer, nil
}
