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

type subcommandEntry struct {
	cmd   subcommands.Command
	group string
}

func subcommandsList() []subcommandEntry {
	return []subcommandEntry{
		{cmd: &commandsCommand{}, group: ""},
		{cmd: &depsCommand{}, group: ""},
		{cmd: &digraphCommand{}, group: "advanced"},
		{cmd: &graphCommand{}, group: ""},
		{cmd: &ideAnalysisCommand{}, group: "advanced"},
		{cmd: &inputsCommand{}, group: ""},
		{cmd: &ruleCommand{}, group: ""},
		{cmd: &targetsCommand{}, group: ""},
	}
}

// Subcommands returns the list of subcommands under `query`.
func Subcommands() []subcommands.Command {
	entries := subcommandsList()
	cmds := make([]subcommands.Command, 0, len(entries))
	for _, entry := range entries {
		cmds = append(cmds, entry.cmd)
	}
	return cmds
}

func (c Command) Execute(ctx context.Context, flagSet *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	commander := subcommands.NewCommander(flagSet, c.Name())
	for _, entry := range subcommandsList() {
		commander.Register(entry.cmd, entry.group)
	}
	commander.Register(commander.HelpCommand(), "command-help")
	// TODO: add more subcommands?
	return commander.Execute(ctx)
}
