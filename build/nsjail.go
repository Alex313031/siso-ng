// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	log "github.com/golang/glog"

	"go.chromium.org/build/siso/execute"
	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/toolsupport/nsjailutil"
)

type nsjailExecutor struct {
	b        *Builder
	executor execute.Executor
	req      nsjailutil.Request

	jail *nsjailutil.NSJail
}

func newNSJailExecutor(ctx context.Context, b *Builder, executor execute.Executor, sandboxConfig map[string]string) (*nsjailExecutor, error) {
	exePath := sandboxConfig["nsjail_path"]
	if exePath == "" {
		return nil, errors.New("nsjail_path is not specified")
	}
	workDir := sandboxConfig["nsjail_workdir"]
	if workDir == "" {
		return nil, errors.New("nsjail_workdir is not specified")
	}
	if !filepath.IsAbs(workDir) {
		workDir = filepath.Join(b.path.WorkspaceRoot, workDir)
	}
	err := os.MkdirAll(workDir, 0755)
	if err != nil {
		return nil, fmt.Errorf("failed to setup nsjail_workdir: %w", err)
	}
	outDir := sandboxConfig["nsjail_outdir"]
	if log.V(1) {
		clog.Infof(ctx, "use nsjail=%q", exePath)
	}
	return &nsjailExecutor{
		b:        b,
		executor: executor,
		req: nsjailutil.Request{
			ExePath:       exePath,
			JailRootDir:   workDir,
			WorkspaceRoot: b.path.WorkspaceRoot,
			WorkDir:       b.path.BaseDir,
			OutDir:        outDir,
		},
	}, nil
}

func (n *nsjailExecutor) Close() error {
	if n.jail == nil {
		return nil
	}
	err := n.jail.Close()
	if err != nil {
		return fmt.Errorf("failed to cleanup nsjail %s: %w", n.jail.Dir(), err)
	}
	return nil
}

func (n *nsjailExecutor) Run(ctx context.Context, cmd *execute.Cmd) (err error) {
	fsys := n.b.hashFS.FileSystem(ctx, "/")
	req := n.req
	req.Inputs = cmd.AllInputs()
	req.Outputs = cmd.AllOutputs()
	jail, err := nsjailutil.New(ctx, fsys, req)
	if err != nil {
		return err
	}
	cmd.StdoutWriter()
	cmd.StderrWriter()
	newCmd := &execute.Cmd{}
	*newCmd = *cmd
	newCmd.Args, err = jail.Args(ctx, cmd.Args...)
	if err != nil {
		return fmt.Errorf("failed to setup nsjail: %w", err)
	}
	// phony_output would have no cmd.Outputs, so no need to capture outputs in jail.
	if len(cmd.Outputs) > 0 {
		newCmd.ExecRootInJailDir = jail.ExecRoot()
	}
	clog.Infof(ctx, "run %q", newCmd.Args)
	err = n.executor.Run(ctx, newCmd)
	cmd.SetActionResult(newCmd.ActionResult())
	if err != nil {
		return fmt.Errorf("failed to run nsjail in %s: %w", jail.Dir(), err)
	}
	n.jail = jail
	return nil
}

func (n *nsjailExecutor) logLocalExec(ctx context.Context, step *Step, dur time.Duration) error {
	command := step.def.Binding("command")
	if len(command) > 256 {
		command = command[:256] + " ..."
	}
	allOutputs := step.cmd.AllOutputs()
	var output string
	if len(allOutputs) > 0 {
		output = allOutputs[0]
	}
	var buf bytes.Buffer
	fmt.Fprintf(&buf, `cmd: %s pure:%t/nsjail restat:%t %s
action: %s %s
command: %q %d

`,
		step, step.cmd.Pure, step.cmd.Restat, dur,
		step.cmd.ActionName, output,
		command, dur.Milliseconds())
	_, err := n.b.localexecLogWriter.Write(buf.Bytes())
	if err != nil {
		clog.Warningf(ctx, "failed to log localexec: %v", err)
	}
	return nil
}
