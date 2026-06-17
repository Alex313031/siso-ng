// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package spawnhelper implements the hidden "spawn-helper" subcommand that Siso
// re-execs itself as, to fork+exec local actions without forking its large heap.
package spawnhelper

import (
	"flag"

	"github.com/google/subcommands"
)

// Cmd returns the spawn-helper subcommand.
func Cmd() *Command {
	return &Command{}
}

// Command implements the spawn-helper subcommand.
type Command struct {
	connFD  int
	logFile string
}

func (*Command) Name() string { return "spawn-helper" }

func (*Command) Synopsis() string { return "internal: fork/exec server for local actions" }

func (*Command) Usage() string {
	return `internal helper that spawns local build actions on behalf of siso.

Users do not run this directly; siso launches it automatically for local
actions, to avoid fork()ing its large heap.
`
}

func (c *Command) SetFlags(f *flag.FlagSet) {
	f.IntVar(&c.connFD, "conn-fd", 0, "inherited socketpair fd to serve the spawn protocol on")
	f.StringVar(&c.logFile, "log-file", "", "file for the helper's diagnostics (default: stderr)")
}

var _ subcommands.Command = (*Command)(nil)
