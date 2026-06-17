// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build windows

package localexec

import (
	"context"
	"os/exec"

	"go.chromium.org/build/siso/o11y/clog"
)

// Windows has no unix process groups, so these are no-op/direct-child stubs;
// descendants of a cancelled action aren't tracked. Follow-up: a Job Object
// (kill-on-close + wait for ACTIVE_PROCESS_ZERO, as Bazel does) would give the
// cancellation path cancelGroup/drainGroup parity; completion needs nothing -
// it waits for stdout/stderr EOF like Ninja, see runOnce.

func setProcGroup(c *exec.Cmd) {}

// cancelGroup kills the direct child only (Go can't signal a group on Windows).
func cancelGroup(ctx context.Context, c *exec.Cmd) error {
	err := c.Process.Kill()
	clog.Warningf(ctx, "send kill to pid=%d: %v", c.Process.Pid, err)
	// Return err verbatim: nil when we killed a live child (os/exec reports the
	// cancellation), os.ErrProcessDone when it had already exited (os/exec keeps
	// the real exit status). Matches the unix cancelGroup's ESRCH handling.
	return err
}

// drainGroup is a no-op stub: without Job Objects there is nothing to sweep with.
func drainGroup(ctx context.Context, pgid int) {}
