// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"fmt"

	"go.chromium.org/build/siso/execute"
	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/toolsupport/makeutil"
)

type depsDepfile struct{}

func (depsDepfile) DepsFastCmd(ctx context.Context, b *Builder, cmd *execute.Cmd) (*execute.Cmd, error) {
	newCmd := &execute.Cmd{}
	*newCmd = *cmd
	return newCmd, nil
}

func (depsDepfile) DepsAfterRun(ctx context.Context, b *Builder, step *Step) ([]string, error) {
	_, err := b.hashFS.Stat(ctx, b.path.ExecRoot, step.cmd.Depfile)
	if err != nil {
		return nil, err
	}
	fsys := b.hashFS.FileSystem(ctx, b.path.ExecRoot)
	depins, err := makeutil.ParseDepsFile(ctx, fsys, step.cmd.Depfile)
	if err != nil {
		return nil, fmt.Errorf("failed to parse depfile %q: %w", step.cmd.Depfile, err)
	}
	err = checkDeps(ctx, b, step, depins)
	if err != nil {
		return nil, fmt.Errorf("error in depfile %q: %w", step.cmd.Depfile, err)
	}
	clog.Infof(ctx, "depfile %s: %d", step.cmd.Depfile, len(depins))
	return depins, nil
}

func (depsDepfile) DepsCmd(ctx context.Context, b *Builder, step *Step) ([]string, error) {
	// depfile can use "remote" only for fastDeps case.
	// Otherwise, should disable "remote"
	clog.Infof(ctx, "deps= depfile=%s. no pure, no remote", step.cmd.Depfile)
	step.cmd.Pure = false
	return step.cmd.Inputs, nil
}
