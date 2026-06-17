// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package localexec

import (
	"os"
	"strconv"
	"testing"
	"time"

	"go.chromium.org/build/siso/execute"
	epb "go.chromium.org/build/siso/execute/proto"
)

func init() {
	if os.Getenv("TEST_ALLOCATE_MEM") != "" {
		mb, err := strconv.Atoi(os.Getenv("TEST_ALLOCATE_MEM"))
		if err != nil {
			os.Exit(1)
		}
		// Allocate mb megabytes of memory
		size := mb * 1024 * 1024
		buf := make([]byte, size)
		// Write to every page to ensure it is resident in physical memory (Working Set / RSS)
		pageSize := 4096
		for i := 0; i < size; i += pageSize {
			buf[i] = 1
		}
		// Keep reference so garbage collector doesn't free it
		_ = buf[0]

		// Spin for 200ms to consume CPU
		start := time.Now()
		for time.Since(start) < 200*time.Millisecond {
			// CPU activity
		}
		os.Exit(0)
	}
}

func TestLocalExecRusage(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	ctx := t.Context()
	cmd := &execute.Cmd{
		Args:          []string{exe},
		Env:           append(os.Environ(), "TEST_ALLOCATE_MEM=50"),
		WorkspaceRoot: t.TempDir(),
	}

	err = Run(ctx, cmd)
	if err != nil {
		t.Fatalf("localexec.Run failed: %v", err)
	}

	res, cached := cmd.ActionResult()
	if cached {
		t.Fatal("action was cached, but expected real execution")
	}
	if res == nil {
		t.Fatal("action result is nil")
	}

	if res.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", res.ExitCode)
	}

	var ru *epb.Rusage
	for _, any := range res.ExecutionMetadata.AuxiliaryMetadata {
		r := &epb.Rusage{}
		if err := any.UnmarshalTo(r); err == nil {
			ru = r
			break
		}
	}

	if ru == nil {
		t.Fatal("failed to find Rusage in ActionResult.ExecutionMetadata.AuxiliaryMetadata")
	}

	t.Logf("Captured Rusage: MaxRss=%d, Majflt=%d, Utime=%v, Stime=%v",
		ru.MaxRss, ru.Majflt, ru.Utime.AsDuration(), ru.Stime.AsDuration())

	// MaxRss is in bytes.
	// Since we allocated 50MB (52428800 bytes), let's verify.
	// Check that we captured at least a reasonable portion of the allocation (e.g. 40MB).
	var minExpectedRSS int64 = 40 * 1024 * 1024 // 40 MB
	if ru.MaxRss < minExpectedRSS {
		t.Errorf("captured peak RSS = %d; want at least %d", ru.MaxRss, minExpectedRSS)
	}

	cpuTime := ru.Utime.AsDuration() + ru.Stime.AsDuration()
	if cpuTime <= 0 {
		t.Errorf("captured CPU time = %v; want > 0", cpuTime)
	}
}
