// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package query is ninja_query subcommand to query ninja build graph.
package query

import (
	"context"
	"flag"

	"github.com/google/subcommands"
)

// Cmd returns the Command for the `query` subcommand.
func Cmd() Command {
	return Command{}
}

// Command implements query subcommand.
type Command struct{}

func (Command) Name() string {
	return "query"
}

func (Command) Synopsis() string {
	return "command group to query ninja build graph"
}

func (Command) Usage() string {
	return `command group to query ninja build graph.

Use "siso query" to display subcommands.
Use "siso query help [subcommand]" for more information about a subcommand.
`
}

func (Command) SetFlags(flagSet *flag.FlagSet) {}

func (c Command) Execute(ctx context.Context, flagSet *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	commander := subcommands.NewCommander(flagSet, c.Name())
	commander.Register(&commandsCommand{}, "")
	commander.Register(&depsCommand{}, "")
	commander.Register(&digraphCommand{}, "advanced")
	commander.Register(&ideAnalysisCommand{}, "advanced")
	commander.Register(&inputsCommand{}, "")
	commander.Register(&ruleCommand{}, "")
	commander.Register(&targetsCommand{}, "")
	commander.Register(commander.HelpCommand(), "command-help")
	// TODO: add more subcommands?
	return commander.Execute(ctx)
}
