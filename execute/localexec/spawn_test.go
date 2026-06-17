// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build unix

package localexec

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.chromium.org/build/siso/execute"
)

func helperCmd(t *testing.T, args ...string) *execute.Cmd {
	t.Helper()
	return &execute.Cmd{
		Args:          args,
		Env:           os.Environ(),
		WorkspaceRoot: t.TempDir(),
	}
}

// waitForPid polls pidfile (written by a child via `echo $$ > pidfile`) until it
// holds a pid and returns it, so a test can wait for the action to actually be
// running instead of assuming a fixed startup delay. Fails if none appears.
func waitForPid(t *testing.T, pidfile string) int {
	t.Helper()
	for range 500 {
		if b, err := os.ReadFile(pidfile); err == nil {
			if p, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && p > 0 {
				return p
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("child did not record its pid")
	return 0
}

// TestRunViaHelperOutputRoundTrip exercises the whole stdout/stderr transport in
// one action: it generates 8 MiB of random bytes (binary and non-UTF-8, and
// above protodelim's 4 MiB default message cap), writes them to stdout and their
// sha256 to stderr, then exits non-zero. Recomputing sha256(stdout) in Go and
// matching it against the digest on stderr proves both streams round-trip
// verbatim through the bytes-typed ActionResult and the delimited wire; checking
// the exit code confirms a non-zero exit is reported. The random source is fine
// despite being non-deterministic: any corruption or truncation breaks the
// checksum and fails the test rather than flaking.
func TestRunViaHelperOutputRoundTrip(t *testing.T) {
	const n = 8 << 20 // 8 MiB: above protodelim's 4 MiB default message cap.
	// Capture the random bytes to a file (reading /dev/urandom twice would yield
	// different bytes), cat them to stdout, and print the hash field of
	// sha256sum/shasum (whichever exists) to stderr. ${h%% *} keeps just the hex.
	script := "head -c " + strconv.Itoa(n) + " /dev/urandom > rnd; " +
		"h=$(sha256sum rnd 2>/dev/null || shasum -a 256 rnd 2>/dev/null); " +
		"cat rnd; " +
		`printf %s "${h%% *}" 1>&2; ` +
		"exit 3"
	cmd := helperCmd(t, "sh", "-c", script)
	res, err := runViaHelper(t.Context(), cmd)
	if err != nil {
		t.Fatalf("runViaHelper: %v", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("exit code = %d, want 3", res.ExitCode)
	}
	if got, want := len(res.StdoutRaw), n; got != want {
		t.Fatalf("stdout len = %d, want %d (truncated round-trip)", got, want)
	}
	sum := sha256.Sum256(res.StdoutRaw)
	want := hex.EncodeToString(sum[:])
	if got := strings.TrimSpace(string(res.StderrRaw)); got != want {
		t.Errorf("stderr sha256 = %q, want sha256(stdout) = %q (corrupted round-trip)", got, want)
	}
}

// TestRunViaHelperConcurrent runs many actions at once over the one multiplexed
// connection and checks each gets its own result and output (no crossed wires).
func TestRunViaHelperConcurrent(t *testing.T) {
	const n = 16
	var wg sync.WaitGroup
	errs := make([]error, n)
	outs := make([]string, n)
	for i := range n {
		wg.Go(func() {
			marker := fmt.Sprintf("marker-%d", i)
			cmd := helperCmd(t, "sh", "-c", "echo "+marker)
			res, err := runViaHelper(t.Context(), cmd)
			if err != nil {
				errs[i] = err
				return
			}
			outs[i] = strings.TrimSpace(string(res.StdoutRaw))
		})
	}
	wg.Wait()
	for i := range n {
		if errs[i] != nil {
			t.Errorf("action %d: %v", i, errs[i])
			continue
		}
		if want := fmt.Sprintf("marker-%d", i); outs[i] != want {
			t.Errorf("action %d: stdout = %q, want %q", i, outs[i], want)
		}
	}
}

// TestRunViaHelperCancel checks that cancelling the context promptly terminates
// the helper's child instead of waiting for it to finish on its own. It waits for
// the child to record its pid (so the action is provably running) before
// cancelling, rather than assuming the helper starts within a fixed delay.
func TestRunViaHelperCancel(t *testing.T) {
	tmp := t.TempDir()
	pidfile := filepath.Join(tmp, "pid")
	ctx, cancel := context.WithCancel(t.Context())
	cmd := &execute.Cmd{
		// Record the pid, then become a long sleep (same process, via exec) so the
		// pidfile signals the action is running and cancel hits exactly this pid.
		Args:          []string{"sh", "-c", "echo $$ > " + pidfile + "; exec sleep 60"},
		Env:           os.Environ(),
		WorkspaceRoot: tmp,
	}

	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := runViaHelper(ctx, cmd)
		done <- err
	}()

	// Cancel only once the child is actually running.
	pid := waitForPid(t, pidfile)
	cancel()

	select {
	case err := <-done:
		// The cancellation must surface as context.Canceled, not a generic error:
		// the helper's reply crosses the wire as a string, so the client restores
		// the real cause (see Run's ctx.Done path) to match the in-process path.
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
		if elapsed := time.Since(start); elapsed > 10*time.Second {
			t.Errorf("cancellation took %v, want it to be prompt", elapsed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runViaHelper did not return after cancellation")
	}

	// The child must have been killed by the cancellation, not left running.
	if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
		time.Sleep(500 * time.Millisecond) // brief grace while the helper reaps it
		if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
			t.Errorf("child pid %d still alive after cancel (kill(0)=%v), want ESRCH", pid, err)
		}
	}
}

// TestHelperExitsOnConnClose verifies the parent-death contract: when siso's end
// of the control socket closes, the helper observes EOF and exits.
func TestHelperExitsOnConnClose(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	c, err := launch(exe, "")
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if err := c.conn.close(); err != nil {
		t.Fatalf("close conn: %v", err)
	}
	exited := make(chan struct{})
	go func() {
		_ = c.cmd.Wait()
		close(exited)
	}()
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		_ = c.cmd.Process.Kill()
		t.Fatal("helper did not exit after the control connection closed")
	}
}

// TestRunViaHelperEnv checks that cmd.Env reaches the child through the helper.
func TestRunViaHelperEnv(t *testing.T) {
	cmd := &execute.Cmd{
		Args:          []string{"sh", "-c", `printf %s "$SPAWN_TEST_VAR"`},
		Env:           append(os.Environ(), "SPAWN_TEST_VAR=hello"),
		WorkspaceRoot: t.TempDir(),
	}
	res, err := runViaHelper(t.Context(), cmd)
	if err != nil {
		t.Fatalf("runViaHelper: %v", err)
	}
	if got, want := string(res.StdoutRaw), "hello"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
}

// TestWrappedActionViaHelper exercises the nsjail/strace executor pattern (copy
// the Cmd, wrap Args, delegate to Run) through the helper; `env` is the wrapper.
func TestWrappedActionViaHelper(t *testing.T) {
	inner := []string{"sh", "-c", "echo real-out; echo real-err 1>&2; exit 7"}
	cmd := &execute.Cmd{
		Args:          inner,
		Env:           os.Environ(),
		WorkspaceRoot: t.TempDir(),
	}
	cmd.StdoutWriter()
	cmd.StderrWriter()
	wrapped := &execute.Cmd{}
	*wrapped = *cmd
	wrapped.Args = append([]string{"env"}, inner...)

	err := LocalExec{}.Run(t.Context(), wrapped)
	wrappedRes, _ := wrapped.ActionResult()
	cmd.SetActionResult(wrappedRes, false)

	ee, ok := errors.AsType[execute.ExitError](err)
	if !ok {
		t.Fatalf("Run err = %v, want execute.ExitError", err)
	}
	if ee.ExitCode != 7 {
		t.Errorf("exit = %d, want 7", ee.ExitCode)
	}
	res, _ := cmd.ActionResult()
	if got, want := strings.TrimSpace(string(res.StdoutRaw)), "real-out"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
	if got, want := strings.TrimSpace(string(res.StderrRaw)), "real-err"; got != want {
		t.Errorf("stderr = %q, want %q", got, want)
	}
}

// TestHelperDeathUnblocksRun verifies that if the helper dies mid-action,
// runViaHelper still returns: the closed control socket fails the pending
// Run via readLoop, rather than hanging.
func TestHelperDeathUnblocksRun(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	c, err := launch(exe, "")
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	orig := helper.Swap(c)
	t.Cleanup(func() { helper.Store(orig) })

	// ctx stays live while we wait below; the unblock under test must come from
	// the helper dying, not from ctx cancellation (which only happens at cleanup).
	ctx := t.Context()
	tmp := t.TempDir()
	done := make(chan error, 1)
	go func() {
		cmd := &execute.Cmd{
			Args:          []string{"sleep", "30"},
			Env:           os.Environ(),
			WorkspaceRoot: tmp,
		}
		_, err := runViaHelper(ctx, cmd)
		done <- err
	}()

	// Let the action start its child, then kill the helper (not the child).
	time.Sleep(300 * time.Millisecond)
	if err := c.cmd.Process.Kill(); err != nil {
		t.Fatalf("kill helper: %v", err)
	}

	select {
	case err := <-done:
		if err == nil {
			t.Errorf("runViaHelper returned nil, want an error after helper death")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("runViaHelper hung after helper death")
	}
}

// TestHelperDrainsOnConnClose verifies that if the control socket closes mid-action
// (siso gone), the helper cancels and reaps the child before exiting.
func TestHelperDrainsOnConnClose(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	c, err := launch(exe, "")
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	orig := helper.Swap(c)
	t.Cleanup(func() { helper.Store(orig) })

	tmp := t.TempDir()
	pidfile := filepath.Join(tmp, "pid")
	ctx := t.Context()
	go func() {
		cmd := &execute.Cmd{
			// Record the pid, then become a long sleep (same process, via exec)
			// so cancelling the action signals exactly this pid.
			Args:          []string{"sh", "-c", "echo $$ > " + pidfile + "; exec sleep 30"},
			Env:           os.Environ(),
			WorkspaceRoot: tmp,
		}
		_, _ = runViaHelper(ctx, cmd)
	}()

	// Wait for the child to record its pid.
	pid := waitForPid(t, pidfile)

	// Simulate siso death: close the control socket. The helper must drain
	// (cancel + kill + reap the child) before exiting.
	if err := c.conn.close(); err != nil {
		t.Fatalf("close conn: %v", err)
	}
	exited := make(chan struct{})
	go func() { _ = c.cmd.Wait(); close(exited) }()
	select {
	case <-exited:
	case <-time.After(15 * time.Second):
		_ = c.cmd.Process.Kill()
		t.Fatal("helper did not exit after the control connection closed")
	}

	// The child must have been killed during the drain, not orphaned.
	if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
		// It may still be in the middle of being reaped; give it a brief grace.
		time.Sleep(500 * time.Millisecond)
		if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
			t.Errorf("child pid %d still alive after helper exit (kill(0)=%v), want ESRCH (orphaned)", pid, err)
		}
	}
}

// TestLaunchSetsOwnProcessGroup verifies launch puts the helper in its own process
// group, so a terminal Ctrl-C or group SIGTERM to siso doesn't reach it directly.
func TestLaunchSetsOwnProcessGroup(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	c, err := launch(exe, "")
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	t.Cleanup(func() {
		_ = c.conn.close()
		_ = c.cmd.Process.Kill()
		_ = c.cmd.Wait()
	})

	pid := c.cmd.Process.Pid
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		t.Fatalf("Getpgid(%d): %v", pid, err)
	}
	if pgid != pid {
		t.Errorf("helper pgid = %d, want it to lead its own group (== pid %d)", pgid, pid)
	}
	if self, err := syscall.Getpgid(0); err == nil && pgid == self {
		t.Errorf("helper pgid = %d shares the test process group %d; want its own", pgid, self)
	}
}

// TestRunViaHelperWorkDir checks that WorkspaceRoot and WorkDir are passed
// separately to the helper, which joins them as the action's working directory.
func TestRunViaHelperWorkDir(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "out", "Default"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := &execute.Cmd{
		Args:          []string{"sh", "-c", "pwd"},
		Env:           os.Environ(),
		WorkspaceRoot: ws,
		WorkDir:       "out/Default",
	}
	res, err := runViaHelper(t.Context(), cmd)
	if err != nil {
		t.Fatalf("runViaHelper: %v", err)
	}
	// macOS reports TempDir under /private; resolve symlinks on both sides.
	want, _ := filepath.EvalSymlinks(filepath.Join(ws, "out", "Default"))
	got, _ := filepath.EvalSymlinks(strings.TrimSpace(string(res.StdoutRaw)))
	if got != want {
		t.Errorf("action cwd = %q, want %q", got, want)
	}
}
