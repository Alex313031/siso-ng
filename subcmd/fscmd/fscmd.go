// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package fscmd provides fs subcommand.
package fscmd

import (
	"context"
	"flag"

	"github.com/google/subcommands"

	"go.chromium.org/build/siso/auth/cred"
)

const stateFile = ".siso_fs_state"

// Cmd returns the Command for the `fs` subcommand provided by this package.
func Cmd(authOpts func() cred.Options) *Command {
	return &Command{
		authOpts: authOpts,
	}
}

// Command implements fs subcommand.
type Command struct {
	authOpts func() cred.Options
}

func (*Command) Name() string {
	return "fs"
}

func (*Command) Synopsis() string {
	return "command group to access siso hashfs data"
}

func (*Command) Usage() string {
	return `command group to access siso hashfs data

Use "siso fs" to display subcommands.
Use "siso fs help [subcommand]" for more information about a subcommand.
`
}

func (*Command) SetFlags(flagSet *flag.FlagSet) {}

// Subcommands returns the list of subcommands under `fs`.
func Subcommands(authOpts func() cred.Options) []subcommands.Command {
	var opts cred.Options
	if authOpts != nil {
		opts = authOpts()
	}
	return []subcommands.Command{
		&diffCommand{},
		&exportCommand{},
		&flushCommand{authOpts: opts},
		&gcCommand{},
		&importCommand{},
		&statusCommand{},
	}
}

func (c *Command) Execute(ctx context.Context, flagSet *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	commander := subcommands.NewCommander(flagSet, c.Name())
	for _, cmd := range Subcommands(c.authOpts) {
		commander.Register(cmd, "")
	}
	commander.Register(commander.HelpCommand(), "command-help")
	return commander.Execute(ctx)
}
