// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build unix

package spawnhelper

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/google/subcommands"

	"go.chromium.org/build/siso/execute/localexec"
)

func (c *Command) Execute(ctx context.Context, _ *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := c.server.Serve(ctx, localexec.Spawner{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "spawn-helper: %v\n", err)
		return subcommands.ExitFailure
	}
	return subcommands.ExitSuccess
}
