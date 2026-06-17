// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package sandbox is sandbox subcommand for debugging sandbox.
package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/google/subcommands"
	"google.golang.org/protobuf/encoding/prototext"

	"go.chromium.org/build/siso/build/ninjabuild"
	"go.chromium.org/build/siso/execute"
	"go.chromium.org/build/siso/execute/localexec"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/toolsupport/nsjailutil"
)

const usage = `run in sandbox

 $ siso sandbox -nsjail '<json nsjail request>' -- args ...

You can manually construct json string of
go.chromium.org/build/siso/toolsupport/nsjail.Request.
`

// Cmd returns the Command for the `sandbox` subcommand provided by this package.
func Cmd() *Command {
	return &Command{}
}

func (*Command) Name() string {
	return "sandbox"
}

func (*Command) Synopsis() string {
	return "run in sandbox"
}

func (*Command) Usage() string {
	return usage
}

// Command implements sandbox subcommand.
type Command struct {
	outDir              ninjabuild.DirFlag
	fsopt               *hashfs.Option
	nsjailReqJSONString string
	cleanup             bool
	cmdline             []string
}

func (c *Command) SetFlags(flagSet *flag.FlagSet) {
	c.outDir.RegisterFlags(flagSet)
	c.fsopt = new(hashfs.Option)
	c.fsopt.StateFile = ".siso_fs_state"
	c.fsopt.RegisterFlags(flagSet)
	flagSet.StringVar(&c.nsjailReqJSONString, "nsjail", "", "json format of nsjail request")
	flagSet.BoolVar(&c.cleanup, "cleanup", true, "cleanup sandbox after execution")
}

func (c *Command) Execute(ctx context.Context, flagSet *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	c.cmdline = flagSet.Args()
	err := c.run(ctx)
	if err != nil {
		switch {
		case errors.Is(err, flag.ErrHelp):
			fmt.Fprintf(os.Stderr, "%v\n%s", err, usage)
			return subcommands.ExitUsageError
		default:
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return subcommands.ExitFailure
		}
	}
	return subcommands.ExitSuccess
}

func (c *Command) run(ctx context.Context) error {
	_, workspaceRoot, outDir, err := ninjabuild.InitDir(ctx, c.outDir)
	if err != nil {
		return err
	}
	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		return err
	}
	defer hashFS.Close(ctx)
	fsstate, err := hashfs.Load(ctx, *c.fsopt)
	if err != nil {
		return err
	}
	err = hashFS.SetState(ctx, fsstate)
	if err != nil {
		return err
	}

	var req nsjailutil.Request
	if c.nsjailReqJSONString == "" {
		return fmt.Errorf("no nsjail request")
	}
	err = json.Unmarshal([]byte(c.nsjailReqJSONString), &req)
	if err != nil {
		return err
	}
	req.WorkspaceRoot = workspaceRoot
	req.WorkDir = outDir
	clog.Infof(ctx, "req: %#v", req)
	fsys := hashFS.FileSystem(ctx, "/")
	jail, err := nsjailutil.New(ctx, fsys, req)
	if err != nil {
		return err
	}
	if c.cleanup {
		defer jail.Close()
	}

	cmd := &execute.Cmd{
		WorkspaceRoot:     workspaceRoot,
		WorkDir:           outDir,
		Inputs:            req.Inputs,
		Outputs:           req.Outputs,
		HashFS:            hashFS,
		ExecRootInJailDir: jail.ExecRoot(),
	}
	cmd.Args, err = jail.Args(ctx, c.cmdline...)
	if err != nil {
		return err
	}
	clog.Infof(ctx, "args=%q", cmd.Args)
	cmd.InitOutputs()
	fmt.Printf("run %q in jail %q\n", c.cmdline, jail.Dir())
	err = localexec.Run(ctx, cmd)
	if err != nil {
		return err
	}
	result, _ := cmd.ActionResult()
	outputEntries, err := hashFS.Entries(ctx, cmd.WorkspaceRoot, cmd.AllOutputs())
	if err != nil {
		return err
	}
	// Set the outputs on the result
	execute.ResultFromEntries(ctx, result, cmd.WorkDir, outputEntries)

	buf, err := prototext.MarshalOptions{
		Multiline: true,
		Indent:    " ",
	}.Marshal(result)
	if err != nil {
		return err
	}
	fmt.Printf("%s\n", buf)
	return nil
}
