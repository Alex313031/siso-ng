// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build unix

package localexec

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"

	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/execute"
	epb "go.chromium.org/build/siso/execute/proto"
)

// helper holds the running spawn helper once StartHelper launches it. runViaHelper
// uses it when set; when unset - under `go test`, or a subcommand that doesn't drive
// a build - actions run in-process. atomic so build workers read it without locking.
// Tests Store a helper directly.
var helper atomic.Pointer[client]

// StartHelper launches the spawn helper, so the large-heap siso never fork()s. Call
// it once, early, while siso's heap is still small. logFile names the file the helper
// writes its diagnostics to; the caller rotates any previous one and the helper
// creates it fresh.
func StartHelper(ctx context.Context, logFile string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("spawn helper: %w", err)
	}
	c, err := launch(exe, logFile)
	if err != nil {
		return fmt.Errorf("spawn helper: %w", err)
	}
	helper.Store(c)
	return nil
}

// runViaHelper runs cmd through the spawn helper. When no helper was started it
// runs in-process; in a real build StartHelper ran first (and aborted the build on
// failure), so siso never silently fork()s its large heap here.
func runViaHelper(ctx context.Context, cmd *execute.Cmd) (*rpb.ActionResult, error) {
	c := helper.Load()
	if c == nil {
		return run(ctx, cmd)
	}

	var oomScoreAdj *int32
	if cmd.OOMScoreAdj != nil {
		oomScoreAdj = new(int32(*cmd.OOMScoreAdj))
	}
	req := &epb.SpawnRequest{
		Id:            cmd.ID,
		Args:          cmd.Args,
		Env:           cmd.Env,
		WorkspaceRoot: cmd.WorkspaceRoot,
		WorkDir:       cmd.WorkDir,
		OomScoreAdj:   oomScoreAdj,
	}
	res, err := c.Run(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("spawn helper: %w", err)
	}

	// The helper buffered the child's output and returned it inline; copy it into
	// cmd's buffers so callers reading cmd.Stdout()/Stderr() see it (mirrors the
	// remote-exec path in remoteexec.go and the cache path in cache.go).
	if len(res.StdoutRaw) > 0 {
		cmd.StdoutWriter().Write(res.StdoutRaw)
	}
	if len(res.StderrRaw) > 0 {
		cmd.StderrWriter().Write(res.StderrRaw)
	}
	return res, nil
}
