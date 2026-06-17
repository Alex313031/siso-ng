// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build unix

package trace

import (
	"runtime"
	"syscall"
	"time"
)

type usageRecord struct {
	rusage syscall.Rusage
	start  time.Time
}

func (u *usageRecord) get() {
	syscall.Getrusage(syscall.RUSAGE_SELF, &u.rusage)
}

func (u *usageRecord) sample(pid int64, t time.Time) []Event {
	var rusage syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &rusage)
	ret := make([]Event, 0, 3)
	o := Event{
		Ph:  "C",
		T:   t.Sub(u.start).Microseconds(),
		Pid: pid,
		Tid: sisoTid,
	}
	o.Name = "cpu"
	utime := float64(rusage.Utime.Nano()-u.rusage.Utime.Nano()) / float64(time.Second)
	stime := float64(rusage.Stime.Nano()-u.rusage.Stime.Nano()) / float64(time.Second)
	o.Args = map[string]any{
		"user": utime,
		"sys":  stime,
	}
	ret = append(ret, o)
	o.Name = "mem"
	maxrss := rusage.Maxrss
	if runtime.GOOS == "linux" {
		maxrss *= 1024
	}
	o.Args = map[string]any{
		"maxrss": maxrss,
	}
	ret = append(ret, o)
	o.Name = "io"
	o.Args = map[string]any{
		// TODO: use /proc/self/io syscr/syscw for linux?
		"rop": rusage.Inblock - u.rusage.Inblock,
		"wop": rusage.Oublock - u.rusage.Oublock,
	}
	ret = append(ret, o)
	u.rusage = rusage
	return ret
}
