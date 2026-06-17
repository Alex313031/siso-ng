// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build linux

package localexec

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"go.chromium.org/build/siso/execute"
)

// writeBusyProg creates an executable in a temp dir and returns its path
// together with an open write-mode fd to it. While that fd is open, execve(2)
// of the program fails with ETXTBSY on Linux (inode i_writecount > 0).
func writeBusyProg(t *testing.T) (string, *os.File) {
	t.Helper()
	prog := filepath.Join(t.TempDir(), "prog")
	if err := os.WriteFile(prog, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(prog, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	return prog, f
}

// TestRunRetriesETXTBSY checks that run() retries when the executable is
// transiently open for writing - the residual race after install-helper's
// removal: an inherited CLOEXC write fd in a forked child that hasn't
// exec'd yet, or a short-lived external writer (golang/go#22315).
// The writer here releases the fd after ~50ms; the first exec attempt fails
// deterministically with ETXTBSY, so without the retry the action fails.
func TestRunRetriesETXTBSY(t *testing.T) {
	prog, f := writeBusyProg(t)
	closed := make(chan struct{})
	go func() {
		time.Sleep(50 * time.Millisecond)
		f.Close()
		close(closed)
	}()
	defer func() { <-closed }()

	cmd := &execute.Cmd{
		Args:          []string{prog},
		Env:           os.Environ(),
		WorkspaceRoot: filepath.Dir(prog),
	}
	res, err := run(t.Context(), cmd)
	if err != nil {
		t.Fatalf("run: %v, want success after ETXTBSY retry", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", res.ExitCode)
	}
}

// TestRunETXTBSYBudgetExhausted checks that a writer held past the retry
// budget surfaces ETXTBSY instead of retrying forever: a build output open
// for writing that long means something is genuinely wrong (e.g. a second
// build in the same outdir), and must fail loudly.
func TestRunETXTBSYBudgetExhausted(t *testing.T) {
	prog, f := writeBusyProg(t)
	defer f.Close() // held for the whole test: the budget must run out

	cmd := &execute.Cmd{
		Args:          []string{prog},
		Env:           os.Environ(),
		WorkspaceRoot: filepath.Dir(prog),
	}
	_, err := run(t.Context(), cmd)
	if !errors.Is(err, syscall.ETXTBSY) {
		t.Errorf("run err = %v, want ETXTBSY after retry budget exhausted", err)
	}
}
