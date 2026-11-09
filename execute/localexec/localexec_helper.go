// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package localexec

import (
	"context"
	"fmt"
	"io"
	"sync/atomic"

	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/execute"
	epb "go.chromium.org/build/siso/execute/proto"
	"go.chromium.org/build/siso/o11y/clog"
)

// SpawnHelper is the interface for a spawn helper.
type SpawnHelper interface {
	Run(ctx context.Context, req *epb.SpawnRequest) (*epb.SpawnResult, error)
}

// helper holds the running spawn helper once StartHelper launches it or SetHelper
// sets it. runViaHelper uses it when set; when unset - under `go test`, or a
// subcommand that doesn't drive a build - actions run in-process. atomic so build
// workers read it without locking.
var helper atomic.Pointer[SpawnHelper]

// SetSpawnHelper sets h as the spawn helper and returns the previous helper.
func SetSpawnHelper(h SpawnHelper) SpawnHelper {
	if h == nil {
		old := helper.Swap(nil)
		if old == nil {
			return nil
		}
		return *old
	}
	old := helper.Swap(&h)
	if old == nil {
		return nil
	}
	return *old
}

// StopSpawnHelper closes the control socket so the helper drains and exits, then waits
// for it. The helper reaps every local action, so its rusage cutime holds all of
// their CPU time. Waiting here is what rolls that up into siso's own rusage, so
// `time siso` and the build-time graph account for the local build's CPU. No-op
// when no helper was started (tests, non-build subcommands).
func StopSpawnHelper(ctx context.Context) error {
	h := SetSpawnHelper(nil)
	if h == nil {
		return nil
	}
	if c, ok := h.(io.Closer); ok {
		if err := c.Close(); err != nil {
			clog.Warningf(ctx, "spawn helper close: %v", err)
		}
	}
	if w, ok := h.(interface{ Wait() error }); ok {
		return w.Wait()
	}
	return nil
}

// runViaHelper runs cmd through the spawn helper. When no helper was started it
// runs in-process; in a real build StartHelper ran first (and aborted the build on
// failure), so siso never silently fork()s its large heap here.
func runViaHelper(ctx context.Context, cmd *execute.Cmd) (*rpb.ActionResult, error) {
	p := helper.Load()
	if p == nil {
		return run(ctx, cmd)
	}
	c := *p

	var oomScoreAdj *int32
	if cmd.OOMScoreAdj != nil {
		oomScoreAdj = new(int32(*cmd.OOMScoreAdj))
	}
	req := &epb.SpawnRequest{
		Id:            cmd.ID,
		Args:          cmd.Args,
		Env:           cmd.Env,
		WorkspaceRoot: cmd.WorkspaceRoot,
		WorkDir:       string(cmd.WorkDir),
		OomScoreAdj:   oomScoreAdj,
	}
	res, err := c.Run(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("spawn helper: %w", err)
	}
	ar := &rpb.ActionResult{}
	if err := res.ActionResult.UnmarshalTo(ar); err != nil {
		return nil, fmt.Errorf("unmarshal action result: %w", err)
	}
	// The helper buffered the child's output and returned it inline; copy it into
	// cmd's buffers so callers reading cmd.Stdout()/Stderr() see it (mirrors the
	// remote-exec path in remoteexec.go and the cache path in cache.go).
	if len(ar.StdoutRaw) > 0 {
		cmd.StdoutWriter().Write(ar.StdoutRaw)
	}
	if len(ar.StderrRaw) > 0 {
		cmd.StderrWriter().Write(ar.StderrRaw)
	}
	return ar, nil
}
