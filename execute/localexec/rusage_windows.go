// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build windows

package localexec

import (
	"os/exec"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	durationpb "google.golang.org/protobuf/types/known/durationpb"

	epb "go.chromium.org/build/siso/execute/proto"
)

var (
	psapi                = windows.NewLazySystemDLL("psapi.dll")
	getProcessMemoryInfo = psapi.NewProc("GetProcessMemoryInfo")
)

type processMemoryCounters struct {
	CB                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

type rusageTracker struct {
	hDup windows.Handle
}

func newRusageTracker(cmd *exec.Cmd) *rusageTracker {
	if cmd.Process == nil {
		return nil
	}
	var hDup windows.Handle
	_ = cmd.Process.WithHandle(func(h uintptr) {
		curProc := windows.CurrentProcess()
		_ = windows.DuplicateHandle(
			curProc,
			windows.Handle(h),
			curProc,
			&hDup,
			0,
			false,
			windows.DUPLICATE_SAME_ACCESS,
		)
	})
	if hDup == 0 {
		return nil
	}
	return &rusageTracker{hDup: hDup}
}

func (t *rusageTracker) Close() {
	if t.hDup != 0 {
		windows.CloseHandle(t.hDup)
		t.hDup = 0
	}
}

func (t *rusageTracker) Rusage() *epb.Rusage {
	if t.hDup == 0 {
		return nil
	}
	var mem processMemoryCounters
	mem.CB = uint32(unsafe.Sizeof(mem))
	r1, _, _ := getProcessMemoryInfo.Call(uintptr(t.hDup), uintptr(unsafe.Pointer(&mem)), uintptr(mem.CB))
	if r1 == 0 {
		return nil
	}

	var creationTime, exitTime, kernelTime, userTime windows.Filetime
	err := windows.GetProcessTimes(t.hDup, &creationTime, &exitTime, &kernelTime, &userTime)
	if err != nil {
		return nil
	}

	return &epb.Rusage{
		MaxRss: int64(mem.PeakWorkingSetSize),
		Majflt: int64(mem.PageFaultCount),
		Utime:  durationpb.New(filetimeToDuration(userTime)),
		Stime:  durationpb.New(filetimeToDuration(kernelTime)),
	}
}

func filetimeToDuration(ft windows.Filetime) time.Duration {
	ns := (int64(ft.HighDateTime)<<32 + int64(ft.LowDateTime)) * 100
	return time.Duration(ns)
}
