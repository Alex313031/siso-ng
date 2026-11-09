// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package localexec

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/types/known/anypb"

	"go.chromium.org/build/siso/execute"
	epb "go.chromium.org/build/siso/execute/proto"
	"go.chromium.org/build/siso/path"
)

// Spawner is the localexec implementation of spawnhelper.Spawner and SpawnHelper.
// The spawn helper calls it for each action, so the helper shares all of
// localexec's spawn, wait, cancel, rusage, and OOM handling.
type Spawner struct{}

// Run implements SpawnHelper by calling Spawn.
func (s Spawner) Run(ctx context.Context, req *epb.SpawnRequest) (*epb.SpawnResult, error) {
	return s.Spawn(ctx, req)
}

// Spawn runs req as a local subprocess via run() and returns its
// ActionResult packed into a SpawnResult.
func (Spawner) Spawn(ctx context.Context, req *epb.SpawnRequest) (*epb.SpawnResult, error) {
	var oomScoreAdj *int
	if req.OomScoreAdj != nil {
		oomScoreAdj = new(int(req.GetOomScoreAdj()))
	}
	cmd := &execute.Cmd{
		ID:            req.GetId(),
		Args:          req.GetArgs(),
		Env:           req.GetEnv(),
		WorkspaceRoot: req.GetWorkspaceRoot(),
		WorkDir:       path.New(req.GetWorkDir()),
		OOMScoreAdj:   oomScoreAdj,
	}

	res, err := run(ctx, cmd)
	if err != nil {
		return nil, err
	}
	anyRes, err := anypb.New(res)
	if err != nil {
		return nil, fmt.Errorf("pack result: %w", err)
	}
	return &epb.SpawnResult{
		ActionResult: anyRes,
	}, nil
}
