// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build unix

package ninja

import (
	"context"
	"fmt"

	"golang.org/x/sys/unix"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/ui"
)

func (c *Command) checkResourceLimits(ctx context.Context, limits build.Limits) {
	var lim unix.Rlimit
	err := unix.Getrlimit(unix.RLIMIT_NOFILE, &lim)
	if err != nil {
		clog.Warningf(ctx, "failed to get rlimit: %v", err)
		return
	}
	nfile := uint64(limits.Local) * 8 // 8 fds per proc?
	switch {
	case c.offline:
	case c.remoteJobs > 0:
		// reproxy grpc client+server, scandeps server client+server
		nfile += uint64(c.remoteJobs) * 4
	default:
		nfile += uint64(limits.Remote) * 4
	}
	clog.Infof(ctx, "rlimit.nofile=%d,%d required=%d?", lim.Cur, lim.Max, nfile)
	if lim.Cur < nfile {
		ui.Default.PrintLines(ui.SGR(ui.Yellow, fmt.Sprintf("WARNING: too low file limit=%d. would fail with too many open files\n", lim.Cur)))
	}
}
