// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build windows

package trace

import (
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

type usageRecord struct {
	kernelTime windows.Filetime
	userTime   windows.Filetime
	ioCounter  windows.IO_COUNTERS
	start      time.Time
}

var getProcessIoCounters = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetProcessIoCounters")

func (u *usageRecord) get() {
	p := windows.CurrentProcess()
	var creationTime, exitTime windows.Filetime
	windows.GetProcessTimes(p, &creationTime, &exitTime, &u.kernelTime, &u.userTime)
	getProcessIoCounters.Call(uintptr(p), uintptr(unsafe.Pointer(&u.ioCounter)))
}

func (u *usageRecord) sample(pid int64, t time.Time) []Event {
	p := windows.CurrentProcess()
	var creationTime, exitTime windows.Filetime
	var kernelTime, userTime windows.Filetime
	windows.GetProcessTimes(p, &creationTime, &exitTime, &kernelTime, &userTime)
	var ioCounter windows.IO_COUNTERS
	getProcessIoCounters.Call(uintptr(windows.CurrentProcess()), uintptr(unsafe.Pointer(&ioCounter)))
	ret := make([]Event, 0, 2)
	o := Event{
		Ph:  "C",
		T:   t.Sub(u.start).Microseconds(),
		Pid: pid,
		Tid: sisoTid,
	}
	o.Name = "cpu"
	utime := float64(userTime.Nanoseconds()-u.userTime.Nanoseconds()) / 1e9
	stime := float64(kernelTime.Nanoseconds()-u.kernelTime.Nanoseconds()) / 1e9
	o.Args = map[string]any{
		"user": utime,
		"sys":  stime,
	}
	ret = append(ret, o)
	// TODO: mem
	o.Name = "io"
	o.Args = map[string]any{
		"rop": ioCounter.ReadOperationCount - u.ioCounter.ReadOperationCount,
		"wop": ioCounter.WriteOperationCount - u.ioCounter.WriteOperationCount,
	}
	ret = append(ret, o)
	u.kernelTime = kernelTime
	u.userTime = userTime
	u.ioCounter = ioCounter
	return ret
}
