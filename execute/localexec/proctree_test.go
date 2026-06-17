// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build unix

package localexec

import (
	"context"
	"errors"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/execute"
)

// The scripts below record the action's own pid to "pgid" and every spawned
// descendant's pid to "pids/<pid>" (one file per process, so concurrent
// writers can't interleave), all relative to the action's workspace. With
// Setpgid the action's pid doubles as its process group id.
const (
	// scriptTree spawns a backgrounded child and an orphaned grandchild (its
	// intermediate parent sh exits immediately, reparenting it to init without
	// changing its process group), then replaces itself with a long sleep.
	scriptTree = `echo $$ > pgid
sh -c 'echo $$ > pids/$$; exec sleep 300' &
sh -c 'sh -c "echo \$\$ > pids/\$\$; exec sleep 300" &' &
exec sleep 300`

	// scriptLeak backgrounds a child that inherits the action's stdout pipe and
	// outlives it briefly (the EOF barrier must wait for it), plus a child that
	// detaches from the pipes and would outlive it by minutes (the post-EOF
	// sweep must kill it), then exits successfully right away.
	scriptLeak = `echo $$ > pgid
sh -c 'echo $$ > pids/$$; exec sleep 0.5' &
sh -c 'echo $$ > pids/$$; exec sleep 300' >/dev/null 2>&1 &
exit 0`

	// scriptFast finishes almost immediately and spawns nothing.
	scriptFast = `echo $$ > pgid`
)

// newTreeAction returns an execute.Cmd running script in a fresh workspace
// prepared with an empty pids/ directory.
func newTreeAction(t *testing.T, script string) (*execute.Cmd, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "pids"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &execute.Cmd{
		Args:          []string{"sh", "-c", script},
		Env:           os.Environ(),
		WorkspaceRoot: dir,
	}, dir
}

// requirePS skips the test when ps isn't on PATH: processGone relies on it to
// tell a live process from a zombie, and there's no point running without it.
func requirePS(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ps"); err != nil {
		t.Skipf("ps not available: %v", err)
	}
}

// processGone reports whether pid is gone. A zombie counts as gone: it is
// dead (it can't touch outputs anymore) but stays in the process table until
// init/launchd reaps it, which is outside localexec's control - its parent
// died with it. Pid reuse within a test's seconds-long window would make a
// dead pid look alive, but Linux allocates pids sequentially from a 4M space
// (macOS ~100k), so a wrap during the test is not realistic.
func processGone(pid int) bool {
	if syscall.Kill(pid, 0) == syscall.ESRCH {
		return true
	}
	// ps is guaranteed present by requirePS at the top of each caller, so an error
	// here means it ran and found no such pid: gone, vanished between the two checks.
	out, err := exec.Command("ps", "-o", "state=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return true
	}
	return strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}

// readPidFile returns the pid recorded in path, or 0 if it isn't there yet.
func readPidFile(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	p, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || p <= 0 {
		return 0
	}
	return p
}

// readPidDir returns the pids recorded in dir, skipping incomplete files.
func readPidDir(dir string) []int {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var pids []int
	for _, e := range ents {
		if p := readPidFile(filepath.Join(dir, e.Name())); p > 0 {
			pids = append(pids, p)
		}
	}
	return pids
}

// waitForTree polls until the action in dir has recorded its own pid and both
// descendant pids (scriptTree and scriptLeak each spawn exactly two), so a test
// can act once the whole tree is provably running. Returns 0, nil on timeout.
// Must not t.Fatal: it runs in stress goroutines.
func waitForTree(dir string) (pid int, pids []int) {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		pid = readPidFile(filepath.Join(dir, "pgid"))
		pids = readPidDir(filepath.Join(dir, "pids"))
		if pid > 0 && len(pids) >= 2 {
			return pid, pids
		}
		time.Sleep(5 * time.Millisecond)
	}
	return 0, nil
}

// killTree SIGKILLs the action's process group (pgid == action pid, and these
// scripts never setsid) so a failing run doesn't litter the machine with 300s
// sleeps.
func killTree(pgid int) {
	if pgid > 0 {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}
}

// TestActionKillsOrphanedGrandchild verifies that cancelling an action kills
// every process it spawned - including a backgrounded child and an orphaned
// grandchild whose parent already exited - not just the direct child, and
// that the run only returns once they are all gone.
func TestActionKillsOrphanedGrandchild(t *testing.T) {
	requirePS(t)
	cmd, dir := newTreeAction(t, scriptTree)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := runViaHelper(ctx, cmd)
		done <- err
	}()

	pid, pids := waitForTree(dir)
	if pid == 0 {
		t.Fatal("action did not record its pid and 2 descendant pids in time")
	}
	t.Cleanup(func() { killTree(pid) })
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Errorf("runViaHelper err = nil, want cancellation error")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("runViaHelper did not return after cancellation")
	}

	for _, p := range append([]int{pid}, pids...) {
		if !processGone(p) {
			t.Errorf("descendant pid %d of action pid %d still alive after cancel, want gone", p, pid)
		}
	}
}

// TestCancelGroupReportsProcessDoneAfterExit verifies that cancelling an action
// whose process group has already exited reports os.ErrProcessDone, not nil:
// os/exec turns a nil Cancel return into context.Canceled even over a clean
// exit, and callers would then skip output recording.
func TestCancelGroupReportsProcessDoneAfterExit(t *testing.T) {
	c := exec.Command("sh", "-c", "exit 0")
	setProcGroup(c)
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	// Reap the leader as os/exec's Process.Wait would; with no descendants the
	// process group is now empty, so cancelGroup's SIGTERM gets ESRCH.
	if _, err := c.Process.Wait(); err != nil {
		t.Fatalf("reap leader: %v", err)
	}

	if err := cancelGroup(t.Context(), c); !errors.Is(err, os.ErrProcessDone) {
		t.Errorf("cancelGroup after the group exited = %v; want os.ErrProcessDone", err)
	}
}

// TestActionWaitsForLeakedStdoutHolder verifies the completion contract on an
// action that exits successfully but leaks background children: the run (a)
// returns exit 0 only after the child holding the stdout pipe has exited on
// its own (the EOF barrier; it must not be killed early), (b) does not wait
// for the fully detached child, which the post-EOF drainGroup sweep kills
// instead, and (c) leaves no descendant behind either way.
func TestActionWaitsForLeakedStdoutHolder(t *testing.T) {
	requirePS(t)
	cmd, dir := newTreeAction(t, scriptLeak)

	start := time.Now()
	type result struct {
		res *rpb.ActionResult
		err error
	}
	done := make(chan result, 1)
	go func() {
		res, err := runViaHelper(t.Context(), cmd)
		done <- result{res: res, err: err}
	}()

	pid, pids := waitForTree(dir)
	if pid == 0 {
		t.Fatal("action did not record its pid and 2 descendant pids in time")
	}
	t.Cleanup(func() { killTree(pid) })

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("runViaHelper: %v", r.err)
		}
		if got, want := r.res.ExitCode, int32(0); got != want {
			t.Errorf("exit code = %d, want %d", got, want)
		}
		// The holder keeps the action's stdout open while it sleeps 0.5s;
		// returning earlier would mean the EOF barrier was cut short and the
		// holder was killed while it could still have been writing outputs.
		if elapsed := time.Since(start); elapsed < 450*time.Millisecond {
			t.Errorf("run returned after %v, want >= ~500ms: it must wait for the stdout-holding child to exit", elapsed)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("runViaHelper hung; want it to return once the stdout-holding child exits, without waiting for the detached child")
	}

	for _, p := range pids {
		if !processGone(p) {
			t.Errorf("leaked child pid %d still alive after the action completed, want gone", p)
		}
	}
}

// TestSubreaperReapsOrphan verifies the PR_SET_CHILD_SUBREAPER mechanism that
// replaces the old /proc-scanning zombie detection: with us installed as a
// subreaper, a descendant orphaned when its parent exits reparents to us (not
// init), so we - and only we - can reap it via wait4(-pgid). That is exactly
// what lets drainGroup clear a SIGKILLed group's zombies and terminate even
// when no init would reap them.
func TestSubreaperReapsOrphan(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("subreaper reparenting is Linux-only")
	}
	if err := becomeSubreaper(); err != nil {
		t.Fatalf("becomeSubreaper: %v", err)
	}

	dir := t.TempDir()
	gcFile := filepath.Join(dir, "gc")
	// The leader (its own process group) backgrounds a grandchild and exits the
	// intermediate sh immediately, orphaning the grandchild while it keeps
	// running in the leader's group.
	leader := exec.Command("sh", "-c", "sh -c 'echo $$ > "+gcFile+"; exec sleep 300' &")
	leader.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := leader.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := leader.Process.Pid
	// Reap the leader as os/exec would for an action's direct child.
	if _, err := leader.Process.Wait(); err != nil {
		t.Fatalf("reap leader: %v", err)
	}

	gcPid := readPidFileWait(t, gcFile)
	t.Cleanup(func() { _ = syscall.Kill(gcPid, syscall.SIGKILL) })

	// Kill the orphan and confirm we can reap it via the group: a successful
	// wait4(-pgid) proves it reparented to us (a non-child would give ECHILD),
	// which is what drainGroup relies on to drain the group without init.
	if err := syscall.Kill(gcPid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill orphan %d: %v", gcPid, err)
	}
	wpid, err := unix.Wait4(-pgid, nil, 0, nil)
	if err != nil {
		t.Fatalf("wait4(-%d): %v; orphan did not reparent to us", pgid, err)
	}
	if wpid != gcPid {
		t.Errorf("reaped pid %d, want orphaned grandchild %d", wpid, gcPid)
	}
}

// TestDrainGroupGivesUpOnUnreapableZombie verifies that drainGroup does not hang
// the build when a descendant escapes the process group via setsid() while still
// parenting a member left in the group. drainGroup SIGKILLs that member into a
// zombie owned by the (alive) escapee, which we cannot reap (it is not our child,
// so wait4(-pgid) gives ECHILD) while kill(-pgid) keeps succeeding because the
// zombie is still a group member. The documented setsid-escape gap must bound out,
// not spin forever. (Killing the escapee itself needs PID namespaces; out of scope.)
func TestDrainGroupGivesUpOnUnreapableZombie(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("subreaper reparenting is Linux-only")
	}
	if err := becomeSubreaper(); err != nil {
		t.Fatalf("becomeSubreaper: %v", err)
	}

	dir := t.TempDir()
	dFile := filepath.Join(dir, "d")
	cFile := filepath.Join(dir, "c")
	// Leader (its own process group) forks D in the group and exits. D records its
	// pid, forks child C into the group, then `exec setsid sleep` to leave the
	// group while staying alive - leaving C in the group, parented by the escapee.
	script := "sh -c 'echo $$ > " + dFile +
		"; sh -c \"echo \\$\\$ > " + cFile + "; exec sleep 300\" &" +
		" exec setsid sleep 300' &"
	leader := exec.Command("sh", "-c", script)
	leader.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := leader.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := leader.Process.Pid
	// Reap the leader as os/exec would for the action's direct child.
	if _, err := leader.Process.Wait(); err != nil {
		t.Fatalf("reap leader: %v", err)
	}

	dPid := readPidFileWait(t, dFile)
	cPid := readPidFileWait(t, cFile)
	t.Cleanup(func() {
		_ = syscall.Kill(dPid, syscall.SIGKILL)
		_ = syscall.Kill(cPid, syscall.SIGKILL)
	})

	// The pid files are written at the start of D's and C's scripts, before D
	// reaches `exec setsid`, so seeing both pids does NOT mean D has left the
	// group yet. Wait until it actually has: until then D is an alive, reapable,
	// reparented-to-us in-group member, and drainGroup would legitimately drain
	// the whole group (returning true) - a real outcome, but not the unreapable
	// zombie this test is about. Under scheduler load that race made this test
	// flake. Synchronizing on getpgid makes the setup deterministic.
	waitLeftGroup(t, dPid, pgid)

	// Kill C so it is a zombie owned by the escaped D: still in the group
	// (kill(-pgid) keeps succeeding) but unreapable by us (wait4(-pgid) gives
	// ECHILD), which is exactly the condition that made drainGroup spin forever.
	if err := syscall.Kill(cPid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill C %d: %v", cPid, err)
	}

	done := make(chan struct{})
	go func() {
		drainGroup(t.Context(), pgid)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("drainGroup did not return; it hung on an unreapable zombie left by a setsid escapee")
	}
}

// TestDrainGroupReapsLeakedChild verifies drainGroup's success path: an
// ordinary in-group leaked child is killed and reaped, so the sweep after a
// completed action leaves nothing of its process group behind.
func TestDrainGroupReapsLeakedChild(t *testing.T) {
	requirePS(t)
	if runtime.GOOS != "linux" {
		t.Skip("subreaper reparenting is Linux-only")
	}
	if err := becomeSubreaper(); err != nil {
		t.Fatalf("becomeSubreaper: %v", err)
	}
	dir := t.TempDir()
	cFile := filepath.Join(dir, "c")
	// Leader (its own group) backgrounds an in-group child and exits, orphaning the
	// child to us (the subreaper) - as a leaked-stdout-holder would be after the
	// action's main process exits.
	leader := exec.Command("sh", "-c", "sh -c 'echo $$ > "+cFile+"; exec sleep 300' &")
	leader.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := leader.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := leader.Process.Pid
	if _, err := leader.Process.Wait(); err != nil {
		t.Fatalf("reap leader: %v", err)
	}
	cPid := readPidFileWait(t, cFile)
	t.Cleanup(func() { _ = syscall.Kill(cPid, syscall.SIGKILL) })

	drainGroup(t.Context(), pgid)
	if !processGone(cPid) {
		t.Errorf("in-group child %d still alive after drainGroup; want gone", cPid)
	}
}

// TestGroupGone pins drainGroup's drained-vs-not classification: only ESRCH means
// the process group is empty. Treating any other kill(-pgid) error (e.g. EPERM for
// a descendant that changed real UID) as drained would let runOnce record outputs
// while an un-killed descendant is still running.
func TestGroupGone(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"esrch", syscall.ESRCH, true},
		{"nil", nil, false},
		{"eperm", syscall.EPERM, false},
		{"einval", syscall.EINVAL, false},
	} {
		if got := groupGone(tc.err); got != tc.want {
			t.Errorf("groupGone(%v) = %v; want %v", tc.err, got, tc.want)
		}
	}
}

// readPidFileWait polls until path holds a pid (written via `echo $$ > path`)
// and returns it, failing if none appears in time.
func readPidFileWait(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if p := readPidFile(path); p > 0 {
			return p
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("pid file %s never got a pid", path)
	return 0
}

// waitLeftGroup blocks until pid is no longer a member of process group pgid
// (e.g. after it calls setsid), so tests can synchronize on a descendant having
// actually escaped the group rather than racing its exec. Fails if pid dies or
// never leaves within the deadline.
func waitLeftGroup(t *testing.T, pid, pgid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		pg, err := syscall.Getpgid(pid)
		if err != nil {
			t.Fatalf("getpgid(%d): %v (process died before leaving group %d)", pid, err, pgid)
		}
		if pg != pgid {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("pid %d never left process group %d", pid, pgid)
}

// TestProcessTreeStress runs many concurrent actions that fork descendants
// and cancels them at random points, verifying that no descendant survives a
// finished action, nothing hangs, and no unexpected error surfaces.
func TestProcessTreeStress(t *testing.T) {
	requirePS(t)
	const n = 96
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			shape := i % 3
			var script string
			switch shape {
			case 0:
				script = scriptTree
			case 1:
				script = scriptLeak
			case 2:
				script = scriptFast
			}
			cmd, dir := newTreeAction(t, script)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			done := make(chan error, 1)
			go func() {
				_, err := runViaHelper(ctx, cmd)
				done <- err
			}()

			var pid int
			var pids []int
			switch shape {
			case 0:
				// Cancel mid-run, once the whole tree is provably running.
				pid, pids = waitForTree(dir)
				if pid == 0 {
					t.Errorf("action %d (shape %d): tree did not come up in time", i, shape)
					return
				}
				time.Sleep(time.Duration(5+rand.IntN(295)) * time.Millisecond)
				cancel()
			case 1:
				// Never cancelled; the action exits 0 on its own.
				pid, pids = waitForTree(dir)
				if pid == 0 {
					t.Errorf("action %d (shape %d): tree did not come up in time", i, shape)
					return
				}
			case 2:
				// Cancel racing with completion of a near-instant action.
				time.Sleep(time.Duration(rand.IntN(5)) * time.Millisecond)
				cancel()
			}
			t.Cleanup(func() { killTree(pid) })

			var err error
			select {
			case err = <-done:
			case <-time.After(30 * time.Second):
				t.Errorf("action %d (shape %d): runViaHelper did not return", i, shape)
				return
			}
			switch shape {
			case 0:
				if err == nil {
					t.Errorf("action %d (shape %d): err = nil, want cancellation error", i, shape)
				}
			case 1:
				if err != nil {
					t.Errorf("action %d (shape %d): err = %v, want nil", i, shape, err)
				}
			case 2:
				// Cancellation raced with completion; either outcome is fine.
			}

			// Shape 2 may finish (or be cancelled) before recording its pid;
			// pick up whatever it managed to write.
			if pid == 0 {
				pid = readPidFile(filepath.Join(dir, "pgid"))
				if pid == 0 {
					return
				}
			}
			for _, p := range append([]int{pid}, pids...) {
				if !processGone(p) {
					t.Errorf("action %d (shape %d): pid %d (action pid %d) still alive after run returned, want gone", i, shape, p, pid)
				}
			}
		})
	}
	wg.Wait()
}
