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
	"strings"

	"github.com/google/subcommands"

	"go.chromium.org/build/siso/toolsupport/ninjautil"
)

const digraphUsage = `show digraph

 $ siso query digraph -C <dir> <targets>

prints directed graph for <targets> of build.ninja.
If <targets> is not give, it will print directed graph for default target specified by build.ninja.
Each line contains zero or more targets, and the first target depends on
the rest of the targets on the same line.

This output can be passed to digraph command, installed by
 $ go install golang.org/x/tools/cmd/digraph@latest

See https://pkg.go.dev/golang.org/x/tools/cmd/digraph
for digraph command.
`

func (*digraphCommand) Name() string {
	return "digraph"
}

func (*digraphCommand) Synopsis() string {
	return "show digraph"
}

func (*digraphCommand) Usage() string {
	return digraphUsage
}

type digraphCommand struct {
	dir   string
	fname string

	orderOnly bool
}

func (c *digraphCommand) SetFlags(flagSet *flag.FlagSet) {
	flagSet.StringVar(&c.dir, "C", ".", "ninja running directory to find build.ninja")
	flagSet.StringVar(&c.fname, "f", "build.ninja", "input build filename (relative to -C)")
	flagSet.BoolVar(&c.orderOnly, "order_only", true, "includes order_only deps")
}

func (c *digraphCommand) Execute(ctx context.Context, flagSet *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	err := c.run(ctx, flagSet.Args())
	if err != nil {
		switch {
		case errors.Is(err, flag.ErrHelp):
			fmt.Fprintf(os.Stderr, "%v\n%s\n", err, digraphUsage)
			return subcommands.ExitUsageError
		default:
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return subcommands.ExitFailure
		}
	}
	return subcommands.ExitSuccess
}

func (c *digraphCommand) run(ctx context.Context, args []string) error {
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
	d := &digraph{
		orderOnly: c.orderOnly,
		seen:      make(map[string]bool),
	}
	for _, t := range targets {
		err := d.Traverse(ctx, state, t)
		if err != nil {
			return err
		}
	}
	return nil
}

type digraph struct {
	orderOnly bool
	seen      map[string]bool
}

func (d *digraph) Traverse(ctx context.Context, state *ninjautil.State, target string) error {
	if d.seen[target] {
		return nil
	}
	d.seen[target] = true
	n, ok := state.LookupNodeByPath(target)
	if !ok {
		return fmt.Errorf("target not found: %q", target)
	}
	edge, ok := n.InEdge()
	if !ok {
		fmt.Printf("%s\n", target)
		return nil
	}
	var inputs []string
	var edgeInputs []*ninjautil.Node
	if d.orderOnly {
		edgeInputs = edge.Inputs()
	} else {
		edgeInputs = edge.TriggerInputs()
	}
	for _, in := range edgeInputs {
		p := in.Path()
		err := d.Traverse(ctx, state, p)
		if err != nil {
			return err
		}
		inputs = append(inputs, p)
	}
	fmt.Printf("%s %s\n", target, strings.Join(inputs, " "))
	return nil
}
