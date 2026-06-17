// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build unix

package localexec

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"go.chromium.org/build/siso/execute"
)

// TestMain lets the test binary act as the spawn helper when re-exec'd by launch,
// so runViaHelper exercises the real cross-process path.
func TestMain(m *testing.M) {
	for i, a := range os.Args {
		if a != "spawn-helper" {
			continue
		}
		connFD := 0
		for j := i + 1; j < len(os.Args)-1; j++ {
			if os.Args[j] == "-conn-fd" {
				connFD, _ = strconv.Atoi(os.Args[j+1])
			}
		}
		_ = ServeSpawnHelper(context.Background(), connFD, log.New(os.Stderr, "", log.LstdFlags))
		os.Exit(0)
	}
	// Production won't auto-launch under `go test`, so launch one explicitly; the
	// re-exec is safe here because this binary dispatches the subcommand above.
	exe, err := os.Executable()
	if err == nil {
		if c, lerr := launch(exe, ""); lerr == nil {
			helper.Store(c)
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

// TestRunViaHelperInProcessFallback checks that when no helper was started - as in
// any package whose tests exercise local execution without launching one -
// runViaHelper runs the action in-process instead of re-exec'ing (which would
// fork-bomb a test binary that has no spawn-helper subcommand).
func TestRunViaHelperInProcessFallback(t *testing.T) {
	orig := helper.Swap(nil)
	t.Cleanup(func() { helper.Store(orig) })

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
