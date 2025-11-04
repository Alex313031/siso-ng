// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build unix

package localexec

import (
	"os/exec"
	"syscall"
	"time"

	durationpb "google.golang.org/protobuf/types/known/durationpb"

	epb "go.chromium.org/build/siso/execute/proto"
)

func rusage(cmd *exec.Cmd) *epb.Rusage {
	if u, ok := cmd.ProcessState.SysUsage().(*syscall.Rusage); ok {
		return &epb.Rusage{
			MaxRss:  u.Maxrss,
			Majflt:  u.Majflt,
			Inblock: u.Inblock,
			Oublock: u.Oublock,
			Utime:   durationpb.New(time.Duration(u.Utime.Nano()) * time.Nanosecond),
			Stime:   durationpb.New(time.Duration(u.Stime.Nano()) * time.Nanosecond),
		}
	}
	return nil
}
