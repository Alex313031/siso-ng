// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build unix

package localexec

import (
	"os/exec"
	"runtime"
	"syscall"
	"time"

	durationpb "google.golang.org/protobuf/types/known/durationpb"

	epb "go.chromium.org/build/siso/execute/proto"
)

type rusageTracker struct {
	cmd *exec.Cmd
}

func newRusageTracker(cmd *exec.Cmd) *rusageTracker {
	return &rusageTracker{cmd: cmd}
}

func (t *rusageTracker) Close() {}

func (t *rusageTracker) Rusage() *epb.Rusage {
	if t.cmd.ProcessState == nil {
		return nil
	}
	if u, ok := t.cmd.ProcessState.SysUsage().(*syscall.Rusage); ok {
		maxRss := u.Maxrss
		if runtime.GOOS == "linux" {
			maxRss *= 1024
		}
		return &epb.Rusage{
			MaxRss:  maxRss,
			Majflt:  u.Majflt,
			Inblock: u.Inblock,
			Oublock: u.Oublock,
			Utime:   durationpb.New(time.Duration(u.Utime.Nano()) * time.Nanosecond),
			Stime:   durationpb.New(time.Duration(u.Stime.Nano()) * time.Nanosecond),
		}
	}
	return nil
}
