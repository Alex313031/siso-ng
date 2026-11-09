// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build windows

package host

import (
	"fmt"
	"math/bits"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	relationProcessorCore = 0
)

type processorRelationship struct {
	flags           byte
	efficiencyClass byte
	reserved        [20]byte
	groupCount      uint16
	groupMask       [1]groupAffinity
}

type groupAffinity struct {
	mask     uintptr
	group    uint16
	reserved [3]uint16
}

type systemLogicalProcessorInformationEx struct {
	relationship uint32
	size         uint32
	payload      [1]byte
}

func cpuCores() (int, int, error) {
	kernel32 := windows.NewLazySystemDLL("kernel32.dll")
	procGetLogicalProcessorInformationEx := kernel32.NewProc("GetLogicalProcessorInformationEx")

	err := procGetLogicalProcessorInformationEx.Find()
	if err != nil {
		return 0, 0, fmt.Errorf("failed to find GetLogicalProcessorInformationEx: %w", err)
	}

	// Query buffer size
	var returnedLength uint32
	r1, _, err := procGetLogicalProcessorInformationEx.Call(
		uintptr(relationProcessorCore),
		0,
		uintptr(unsafe.Pointer(&returnedLength)),
	)
	if r1 != 0 || returnedLength == 0 {
		if err != nil && err != syscall.Errno(0) {
			return 0, 0, fmt.Errorf("GetLogicalProcessorInformationEx failed: %w", err)
		}
		return 0, 0, fmt.Errorf("GetLogicalProcessorInformationEx returned 0 length")
	}

	// Retrieve data
	buf := make([]byte, returnedLength)
	r1, _, err = procGetLogicalProcessorInformationEx.Call(
		uintptr(relationProcessorCore),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&returnedLength)),
	)
	if r1 == 0 {
		if err != nil && err != syscall.Errno(0) {
			return 0, 0, fmt.Errorf("GetLogicalProcessorInformationEx failed: %w", err)
		}
		return 0, 0, fmt.Errorf("GetLogicalProcessorInformationEx failed")
	}

	// Extract what we want
	physicalCores := 0
	logicalCores := 0
	offset := uint32(0)
	for offset < returnedLength {
		info := (*systemLogicalProcessorInformationEx)(unsafe.Pointer(&buf[offset]))
		if info.size == 0 {
			break
		}

		if info.relationship == relationProcessorCore {
			physicalCores++
			rel := (*processorRelationship)(unsafe.Pointer(&info.payload[0]))

			groupMasks := unsafe.Slice(&rel.groupMask[0], rel.groupCount)
			for _, gm := range groupMasks {
				logicalCores += bits.OnesCount64(uint64(gm.mask))
			}
		}

		offset += info.size
	}
	if physicalCores == 0 || logicalCores == 0 {
		return 0, 0, fmt.Errorf("GetLogicalProcessorInformationEx didn't return expected info")
	}

	return physicalCores, logicalCores, nil
}
