// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build unix

package build

import (
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

func (u *usageRecord) sample(t time.Time) []traceEventObject {
	var rusage syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &rusage)
	ret := make([]traceEventObject, 0, 2)
	o := traceEventObject{
		Ph:  "C",
		T:   t.Sub(u.start).Microseconds(),
		Pid: sisoPid,
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
	o.Args = map[string]any{
		"maxrss": rusage.Maxrss,
	}
	ret = append(ret, o)
	u.rusage = rusage
	return ret
}
