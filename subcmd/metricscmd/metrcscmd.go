// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package metricscmd provides metrics subcommand.
package metricscmd

import (
	"context"
	"flag"

	"github.com/google/subcommands"
)

// Cmd returns the Command for the `metrics` subcommand provided by this package.
func Cmd() Command {
	return Command{}
}

// Command implements metrics subcommand.
type Command struct{}

func (Command) Name() string {
	return "metrics"
}

func (Command) Synopsis() string {
	return "command group to analyze siso_metrics.json"
}

func (Command) Usage() string {
	return `command group to analyze siso_metrics.json

Use "siso metrics" to display subcommands.
Use "siso metrics help [subcommand]" for more information about a subcommand.
`
}

func (Command) SetFlags(flagSet *flag.FlagSet) {
}

func (c Command) Execute(ctx context.Context, flagSet *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	commander := subcommands.NewCommander(flagSet, c.Name())
	commander.Register(&cmpCommand{}, "")
	commander.Register(&summaryCommand{}, "")
	commander.Register(commander.HelpCommand(), "command-help")
	return commander.Execute(ctx)
}
