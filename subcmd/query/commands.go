// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package query

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/google/subcommands"

	"go.chromium.org/build/siso/toolsupport/ninjautil"
)

const commandsUsage = `list all commands required to rebuild given targets

 $ siso query commands -C <dir> <targets>

prints all commands required to rebuild given targets.
`

func (*commandsCommand) Name() string {
	return "commands"
}

func (*commandsCommand) Synopsis() string {
	return "list all commands required to rebuild given targets"
}

func (*commandsCommand) Usage() string {
	return commandsUsage
}

type commandsCommand struct {
	dir   string
	fname string
}

func (c *commandsCommand) SetFlags(flagSet *flag.FlagSet) {
	flagSet.StringVar(&c.dir, "C", ".", "ninja running directory to find build.ninja")
	flagSet.StringVar(&c.fname, "f", "build.ninja", "input build filename (relative to -C)")
}

func (c *commandsCommand) Execute(ctx context.Context, flagSet *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	err := c.run(ctx, flagSet.Args())
	if err != nil {
		switch {
		case errors.Is(err, flag.ErrHelp):
			fmt.Fprintf(os.Stderr, "%v\n%s\n", err, commandsUsage)
			return subcommands.ExitUsageError
		default:
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return subcommands.ExitFailure
		}
	}
	return subcommands.ExitSuccess
}

func (c *commandsCommand) run(ctx context.Context, args []string) error {
	state := ninjautil.NewState()
	p := ninjautil.NewManifestParser(state)
	err := os.Chdir(c.dir)
	if err != nil {
		return err
	}
	err = p.Load(ctx, c.fname)
	if err != nil {
		return err
	}
	nodes, err := state.Targets(args)
	if err != nil {
		return err
	}
	targets := make([]string, 0, len(nodes))
	for _, n := range nodes {
		targets = append(targets, n.Path())
	}
	g := &commandsGraph{
		seen: make(map[string]bool),
	}
	for _, t := range targets {
		err := g.Traverse(ctx, state, t)
		if err != nil {
			return err
		}
	}
	return nil
}

type commandsGraph struct {
	seen map[string]bool
}

func (g *commandsGraph) Traverse(ctx context.Context, state *ninjautil.State, target string) error {
	if g.seen[target] {
		return nil
	}
	g.seen[target] = true
	n, ok := state.LookupNodeByPath(target)
	if !ok {
		return fmt.Errorf("target not found: %q", target)
	}
	edge, ok := n.InEdge()
	if !ok {
		return nil
	}
	for _, in := range edge.Inputs() {
		p := in.Path()
		err := g.Traverse(ctx, state, p)
		if err != nil {
			return err
		}
	}
	fmt.Printf("%s\n", edge.Binding("command"))
	return nil
}
