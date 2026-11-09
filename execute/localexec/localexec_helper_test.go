// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build unix

package localexec

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"go.chromium.org/build/siso/execute"
	"go.chromium.org/build/siso/execute/spawnhelper"
)

// cpuSink keeps the burn-cpu loop's work from being optimized away.
var cpuSink int

// selfCPU returns this process's own user+system CPU time so far.
func selfCPU() time.Duration {
	var ru unix.Rusage
	_ = unix.Getrusage(unix.RUSAGE_SELF, &ru)
	return time.Duration(ru.Utime.Nano()) + time.Duration(ru.Stime.Nano())
}

// TestMain lets the test binary act as the spawn helper when re-exec'd by launch,
// so runViaHelper exercises the real cross-process path.
func TestMain(m *testing.M) {
	for i, a := range os.Args {
		// burn-cpu <ms>: spin until this process has consumed ms of CPU, then exit.
		// Used as an action to prove its CPU rolls up through the helper. Targeting
		// consumed CPU (not wall time or a fixed iteration count) keeps it robust to
		// machine speed and to a loaded CI host.
		if a == "burn-cpu" {
			ms, _ := strconv.Atoi(os.Args[i+1])
			want := time.Duration(ms) * time.Millisecond
			for selfCPU() < want {
				for j := range 2_000_000 {
					cpuSink += j
				}
			}
			os.Exit(0)
		}
		if a != "spawn-helper" {
			continue
		}
		fs := flag.NewFlagSet("spawn-helper", flag.ContinueOnError)
		var server spawnhelper.Server
		server.RegisterFlags(fs)
		fs.Parse(os.Args[i+1:])
		_ = server.Serve(context.Background(), Spawner{})
		os.Exit(0)
	}
	// Production won't auto-launch under `go test`, so launch one explicitly; the
	// re-exec is safe here because this binary dispatches the subcommand above.
	exe, err := os.Executable()
	if err == nil {
		if c, lerr := spawnhelper.Launch([]string{exe, "spawn-helper"}, "", false); lerr == nil {
			SetSpawnHelper(c)
		}
	}
	m.Run()
}

// TestRunViaHelperExecutableLookup checks argv0 resolution: a bare name goes
// through PATH, a path with a separator resolves relative to the action's WorkDir.
func TestRunViaHelperExecutableLookup(t *testing.T) {
	ws := t.TempDir()
	// Put the program in a subdirectory so the action runs it via a relative path
	// that only resolves correctly against WorkDir, not siso's cwd.
	if err := os.MkdirAll(filepath.Join(ws, "out"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "prog"), []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		args []string
	}{
		// argv0 contains a separator → passed through, resolved relative to WorkDir
		// (out/), so ../prog points at ws/prog.
		{name: "relative_with_separator", args: []string{"../prog"}},
		// bare name → resolved on PATH; /bin/sh is reachable everywhere we run.
		{name: "bare_name_on_path", args: []string{"true"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &execute.Cmd{
				Args:          tc.args,
				Env:           os.Environ(),
				WorkspaceRoot: ws,
				WorkDir:       "out",
			}
			res, err := runViaHelper(t.Context(), cmd)
			if err != nil {
				t.Fatalf("runViaHelper: %v", err)
			}
			if res.ExitCode != 0 {
				t.Errorf("exit code = %d, want 0; stderr=%q", res.ExitCode, cmd.Stderr())
			}
		})
	}
}

// TestStopSpawnHelperAccountsChildCPU is the regression test for b/531402317: an action
// runs under the out-of-process spawn helper, which reaps it, so the action's CPU
// lands in the helper's rusage. StopSpawnHelper must wait on the helper so that CPU rolls
// up into this process's RUSAGE_CHILDREN. Without the wait it is orphaned to init and
// getrusage never sees it, which is what made `time siso` report almost no CPU.
func TestStopSpawnHelperAccountsChildCPU(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	// A dedicated helper so StopHelper can reap it without disturbing the shared one
	// TestMain installed for the other tests; restore that afterwards.
	c, err := spawnhelper.Launch([]string{exe, "spawn-helper"}, "", false)
	if err != nil {
		t.Fatalf("launch helper: %v", err)
	}
	orig := SetSpawnHelper(c)
	t.Cleanup(func() { SetSpawnHelper(orig) })

	const burn = 500 * time.Millisecond
	cmd := &execute.Cmd{
		Args:          []string{exe, "burn-cpu", strconv.Itoa(int(burn.Milliseconds()))},
		Env:           os.Environ(),
		WorkspaceRoot: t.TempDir(),
	}
	res, err := runViaHelper(t.Context(), cmd)
	if err != nil {
		t.Fatalf("runViaHelper: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("burn-cpu exit = %d, want 0; stderr=%q", res.ExitCode, cmd.Stderr())
	}

	// The helper has reaped the action (its CPU is now in the helper's rusage) but we
	// have not reaped the helper, so RUSAGE_CHILDREN does not include it yet.
	var before unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_CHILDREN, &before); err != nil {
		t.Fatalf("getrusage before: %v", err)
	}
	if err := StopSpawnHelper(t.Context()); err != nil {
		t.Fatalf("StopHelper: %v", err)
	}
	var after unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_CHILDREN, &after); err != nil {
		t.Fatalf("getrusage after: %v", err)
	}

	got := time.Duration(after.Utime.Nano()) - time.Duration(before.Utime.Nano())
	// The child burned ~burn of user CPU; require at least half to absorb noise while
	// still failing hard (delta ~0) if the helper's CPU was not rolled up.
	if want := burn / 2; got < want {
		t.Errorf("RUSAGE_CHILDREN user time delta after StopSpawnHelper = %v, want >= %v; child CPU was not accounted (helper not reaped)", got, want)
	}
}

// TestRunViaHelperInProcessFallback checks that when no helper was started - as in
// any package whose tests exercise local execution without launching one -
// runViaHelper runs the action in-process instead of re-exec'ing (which would
// fork-bomb a test binary that has no spawn-helper subcommand).
func TestRunViaHelperInProcessFallback(t *testing.T) {
	orig := SetSpawnHelper(nil)
	t.Cleanup(func() { SetSpawnHelper(orig) })

	cmd := &execute.Cmd{
		Args:          []string{"true"},
		Env:           os.Environ(),
		WorkspaceRoot: t.TempDir(),
	}
	res, err := runViaHelper(t.Context(), cmd)
	if err != nil {
		t.Fatalf("runViaHelper: %v", err)
	}
	if res == nil {
		t.Fatal("res = nil, want non-nil")
	}
	if res.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", res.ExitCode)
	}
}
