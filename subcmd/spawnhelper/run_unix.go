// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build unix

package spawnhelper

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/google/subcommands"

	"go.chromium.org/build/siso/execute/localexec"
)

func (c *Command) Execute(ctx context.Context, _ *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	if c.connFD <= 0 {
		fmt.Fprintln(os.Stderr, "spawn-helper: -conn-fd is required")
		return subcommands.ExitUsageError
	}

	// Diagnostics go to the helper's own log file (siso_spawn_helper), so they
	// never clash with siso's stdout-based progress UI. Fall back to stderr when
	// no log file was given (e.g. under `go test`).
	logw := os.Stderr
	if c.logFile != "" {
		f, err := os.Create(c.logFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "spawn-helper: create log file %q: %v\n", c.logFile, err)
			return subcommands.ExitFailure
		}
		defer f.Close()
		logw = f
	}
	logger := log.New(logw, "", log.LstdFlags|log.Lmsgprefix)

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := localexec.ServeSpawnHelper(ctx, c.connFD, logger); err != nil {
		fmt.Fprintf(os.Stderr, "spawn-helper: %v\n", err)
		return subcommands.ExitFailure
	}
	return subcommands.ExitSuccess
}
