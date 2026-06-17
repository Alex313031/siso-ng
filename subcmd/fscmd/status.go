// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package fscmd

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/google/subcommands"

	"go.chromium.org/build/siso/build/ninjabuild"
	"go.chromium.org/build/siso/hashfs"
)

func (*statusCommand) Name() string {
	return "status"
}

func (*statusCommand) Synopsis() string {
	return "show workspace status"
}

func (*statusCommand) Usage() string {
	return `show workspace status compared with siso hashfs status.

 $ siso fs status -C <dir>

`
}

type statusCommand struct {
	outDir ninjabuild.DirFlag

	stateFile string
}

func (c *statusCommand) SetFlags(flagSet *flag.FlagSet) {
	c.outDir.RegisterFlags(flagSet)
	flagSet.StringVar(&c.stateFile, "fs_state", stateFile, "fs_state filename")
}

func (c *statusCommand) Execute(ctx context.Context, flagSet *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	err := c.run(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return subcommands.ExitFailure
	}
	return subcommands.ExitSuccess
}

func (c *statusCommand) run(ctx context.Context) error {
	_, _, _, err := ninjabuild.InitDir(ctx, c.outDir)
	if err != nil {
		return fmt.Errorf("failed to init dir %s: %w", c.outDir, err)
	}

	hfs, err := hashfs.New(ctx, hashfs.Option{
		KeepTainted:    true,
		SetStateLogger: os.Stdout,
	})
	if err != nil {
		return fmt.Errorf("hashfs.New: %w", err)
	}
	defer hfs.Close(ctx)

	started := time.Now()
	st, err := hashfs.Load(ctx, hashfs.Option{
		StateFile: c.stateFile,
	})
	if err != nil {
		return fmt.Errorf("hashfs.Load(%q): %w", c.stateFile, err)
	}
	fmt.Printf("load fs state %s\n", time.Since(started))

	err = hfs.SetState(ctx, st)
	if err != nil {
		return fmt.Errorf("set state: %w", err)
	}
	err = hfs.WaitReady(ctx)
	if err != nil {
		return fmt.Errorf("wait ready: %w", err)
	}
	return nil
}
