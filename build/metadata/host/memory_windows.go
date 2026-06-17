// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build windows

package host

import (
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

type memoryStatusEx struct {
	dwLength                uint32
	dwMemoryLoad            uint32
	ullTotalPhys            uint64
	ullAvailPhys            uint64
	ullTotalPageFile        uint64
	ullAvailPageFile        uint64
	ullTotalVirtual         uint64
	ullAvailVirtual         uint64
	ullAvailExtendedVirtual uint64
}

func memoryTotal() (uint64, error) {
	kernel32 := windows.NewLazySystemDLL("kernel32.dll")
	procGlobalMemoryStatusEx := kernel32.NewProc("GlobalMemoryStatusEx")

	err := procGlobalMemoryStatusEx.Find()
	if err != nil {
		return 0, fmt.Errorf("failed to find GlobalMemoryStatusEx: %w", err)
	}

	var ms memoryStatusEx
	ms.dwLength = uint32(unsafe.Sizeof(ms))
	r1, _, err := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&ms)))
	if r1 == 0 {
		if err != nil && err != syscall.Errno(0) {
			return 0, fmt.Errorf("GlobalMemoryStatusEx failed: %w", err)
		}
		return 0, fmt.Errorf("GlobalMemoryStatusEx returned 0 despite no error")
	}
	return ms.ullTotalPhys, nil
}
