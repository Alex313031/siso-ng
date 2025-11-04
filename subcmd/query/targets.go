// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package query

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/google/subcommands"

	"go.chromium.org/build/siso/toolsupport/ninjautil"
)

const targetsUsage = `list targets by their rule or depth in the DAG

 $ siso query targets -C <dir> [--rule <rule>] [--depth <depth>]

prints targets by <rule> or in <depth>.
`

func (*targetsCommand) Name() string {
	return "targets"
}

func (*targetsCommand) Synopsis() string {
	return "list targets by their rule or depth in the DAG"
}

func (*targetsCommand) Usage() string {
	return targetsUsage
}

type targetsCommand struct {
	w io.Writer

	dir   string
	fname string

	rule  targetRuleFlag
	depth int
	all   bool
}

type targetRuleFlag struct {
	rule      string
	requested bool
}

func (f *targetRuleFlag) String() string {
	return f.rule
}

func (f *targetRuleFlag) Set(v string) error {
	f.rule = v
	f.requested = true
	return nil
}

func (c *targetsCommand) SetFlags(flagSet *flag.FlagSet) {
	// TODO(b/340381100): extract common flags for ninja commands.
	flagSet.StringVar(&c.dir, "C", ".", "ninja running directory to find build.ninja")
	flagSet.StringVar(&c.fname, "f", "build.ninja", "input build filename (relative to -C)")

	flagSet.Var(&c.rule, "rule", "rule name for the targets")
	flagSet.IntVar(&c.depth, "depth", 1, "max depth of the targets. 0 does not check depth")
	flagSet.BoolVar(&c.all, "all", false, "list all targets")
}

func (c *targetsCommand) Execute(ctx context.Context, flagSet *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	if c.w == nil {
		c.w = os.Stdout
	}
	err := c.run(ctx)
	if err != nil {
		switch {
		case errors.Is(err, flag.ErrHelp):
			fmt.Fprintf(os.Stderr, "%v\n%s\n", err, targetsUsage)
			return subcommands.ExitUsageError
		default:
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return subcommands.ExitFailure
		}
	}
	return subcommands.ExitSuccess
}

func (c *targetsCommand) run(ctx context.Context) error {
	if c.rule.requested {
		c.depth = 0
	}
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
	nodes, err := state.RootNodes()
	if err != nil {
		return err
	}
	g := &targetsGraph{
		seen:        make(map[*ninjautil.Node]bool),
		w:           c.w,
		all:         c.all,
		rule:        &c.rule,
		ruleTargets: make(map[string]bool),
	}
	for _, n := range nodes {
		err := g.Traverse(ctx, state, n, c.depth, 0)
		if err != nil {
			return err
		}
	}
	if c.rule.requested {
		var targets []string
		for t := range g.ruleTargets {
			targets = append(targets, t)
		}
		sort.Strings(targets)
		for _, t := range targets {
			fmt.Fprintln(c.w, t)
		}
	}
	return nil
}

type targetsGraph struct {
	seen        map[*ninjautil.Node]bool
	w           io.Writer
	all         bool
	rule        *targetRuleFlag
	ruleTargets map[string]bool
}

func (g *targetsGraph) Traverse(ctx context.Context, state *ninjautil.State, node *ninjautil.Node, depth, indent int) error {
	if g.seen[node] {
		return nil
	}
	g.seen[node] = true
	prefix := strings.Repeat(" ", indent)
	edge, ok := node.InEdge()
	if !ok {
		if g.rule.requested && g.rule.rule == "" {
			g.ruleTargets[node.Path()] = true
		}
		return nil
	}
	switch {
	case g.rule.requested:
		if g.rule.rule == edge.RuleName() {
			g.ruleTargets[node.Path()] = true
		}
	case g.all:
		fmt.Fprintf(g.w, "%s: %s\n", node.Path(), edge.RuleName())
	default:
		fmt.Fprintf(g.w, "%s%s: %s\n", prefix, node.Path(), edge.RuleName())
	}
	if !g.all && depth == 1 {
		return nil
	}
	for _, in := range edge.Inputs() {
		err := g.Traverse(ctx, state, in, depth-1, indent+1)
		if err != nil {
			return err
		}
	}
	return nil
}
