// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build unix

package localexec

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"go.chromium.org/build/siso/o11y/clog"
)

// Process-tree management for non-console local actions, modeled on Bazel's
// process-wrapper: each runs as its own process group so cancel and post-exit
// reap reach every descendant, not just the direct child. A setsid() escapee
// still leaves the group (PID namespaces would be needed; possible follow-up).
//
// The spawn helper marks itself as a child subreaper (becomeSubreaper), so a
// descendant orphaned mid-run reparents to us instead of to init and we can
// reap it with wait4(-pgid). Otherwise a SIGKILLed member that nobody reaps
// would linger as a zombie and keep kill(-pgid) succeeding forever (e.g. siso
// as PID 1 in a container with no init reaper).
const (
	// How long to wait after initial SIGTERM before SIGKILL'ing the group
	killDelay = 1 * time.Second

	// Poll interval while waiting for process group to become empty.
	killInterval = 100 * time.Microsecond

	// Backstop for drainGroup: a legitimate drain completes in microseconds, so
	// exceeding this means a member can't be cleared; give up, don't hang.
	drainTimeout = 1 * time.Second
)

// setProcGroup makes the action its own process group leader (pgid == pid),
// applied by the runtime between fork and exec.
func setProcGroup(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// cancelGroup is exec.Cmd.Cancel for setProcGroup actions: SIGTERM the group for
// cleanup, then SIGKILL it if it outlives killDelay. Wait can't return until
// Cancel does, so we poll the group itself (kill 0 probes, failing with ESRCH
// once empty), not the Cmd.
func cancelGroup(ctx context.Context, c *exec.Cmd) error {
	// Gracefully ask the processes to terminate.
	pgid := c.Process.Pid
	err := syscall.Kill(-pgid, syscall.SIGTERM)
	clog.Warningf(ctx, "send SIGTERM to pgid=%d: %v", pgid, err)
	if err == syscall.ESRCH {
		// Whole group already gone: the action finished on its own and
		// cancellation lost the race. Report os.ErrProcessDone so os/exec keeps
		// the real exit status instead of injecting context.Canceled over a
		// successful exit (see watchCtx/Wait).
		return os.ErrProcessDone
	}
	if err != nil {
		// Unexpected (e.g. EPERM): surface it rather than masking it.
		return err
	}

	// Give the group until killDelay to exit on its own after the SIGTERM...
	deadline := time.Now().Add(killDelay)
	for {
		// Reap reparented descendants so their zombies don't keep kill(-pgid, 0)
		// succeeding for the full killDelay - but only once os/exec has reaped
		// the leader (pid == pgid), so wait4(-pgid) can't steal the main child's
		// exit status out from under Cmd.Wait.
		if syscall.Kill(pgid, 0) == syscall.ESRCH {
			reapReparented(pgid)
		}
		if syscall.Kill(-pgid, 0) != nil {
			// Any error ends the poll (normally ESRCH: graceful exit complete);
			// the SIGKILL below and the post-Wait drainGroup handle survivors.
			break
		}
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(killInterval)
	}

	// ...then SIGKILL whatever's left; drainGroup after Cmd.Wait does the
	// authoritative reap.
	syscall.Kill(-pgid, syscall.SIGKILL)

	return nil
}

// drainGroup SIGKILLs the process group and reaps every member until none remain
// (kill(-pgid) reaches ESRCH) or it gives up at drainTimeout with a member it
// couldn't clear. It is called synchronously after Cmd.Wait has reaped the group
// leader (the action's direct child), so wait4(-pgid) here only ever reaps the
// descendants the subreaper reparented to us, never the leader.
func drainGroup(ctx context.Context, pgid int) {
	deadline := time.Now().Add(drainTimeout)
	for {
		reapReparented(pgid)
		err := syscall.Kill(-pgid, syscall.SIGKILL)
		if groupGone(err) {
			return // ESRCH: group empty.
		}
		// err == nil: members remain. Any other error (e.g. EPERM after a uid
		// change) means a member we can't clear is still there; keep trying.
		if !time.Now().Before(deadline) {
			// Stuck member we can't clear; containing it would need PID
			// namespaces. Give up rather than hang the build.
			clog.Warningf(ctx, "drainGroup pgid=%d: gave up after %s (last kill: %v); likely a setsid escapee, a uid-changed, or a D-state descendant left an uncollectable group member", pgid, drainTimeout, err)
			return
		}
		time.Sleep(killInterval)
	}
}

// groupGone reports whether a kill(-pgid) error means the group is empty: only
// ESRCH does (EPERM etc. means an unsignalable member is still there).
func groupGone(killErr error) bool {
	return errors.Is(killErr, syscall.ESRCH)
}

// reapReparented reaps already-exited members of group pgid that the subreaper
// reparented to us, so they don't linger as zombies (without a subreaper, on
// non-Linux, wait4 just returns ECHILD and init/launchd reaps them). Callers
// must ensure the leader (pid == pgid) was already reaped by os/exec, so this
// never steals the action's main child.
func reapReparented(pgid int) {
	for {
		wpid, err := unix.Wait4(-pgid, nil, unix.WNOHANG, nil)
		if wpid <= 0 || err != nil {
			return
		}
	}
}
