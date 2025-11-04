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
	"path/filepath"
	"sort"

	"github.com/google/subcommands"

	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/toolsupport/ninjautil"
)

const inputsUsage = `list all inputs required to rebuild given targets

 $ siso query inputs -C <dir> <targets>

prints all inputs required to rebuild given targets.
`

func (*inputsCommand) Name() string {
	return "inputs"
}

func (*inputsCommand) Synopsis() string {
	return "list all inputs required to rebuild given targets"
}

func (*inputsCommand) Usage() string {
	return inputsUsage
}

type inputsCommand struct {
	dir      string
	stateDir string
	fname    string

	includeDeps bool
	depsLogFile string
}

func (c *inputsCommand) SetFlags(flagSet *flag.FlagSet) {
	// TODO(b/340381100): extract common flags for ninja commands
	flagSet.StringVar(&c.dir, "C", ".", "ninja running directory to find build.ninja")
	flagSet.StringVar(&c.stateDir, "state_dir", ".", "state directory (relative to -C)")
	flagSet.StringVar(&c.fname, "f", "build.ninja", "input build filename (relative to -C)")
	flagSet.BoolVar(&c.includeDeps, "include_deps", false, "include inputs recorded in deps log file")
	flagSet.StringVar(&c.depsLogFile, "deps_log", ".siso_deps", "deps log filename (relative to -C, -stateDir)")
}

func (c *inputsCommand) Execute(ctx context.Context, flagSet *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	err := c.run(ctx, flagSet.Args())
	if err != nil {
		switch {
		case errors.Is(err, flag.ErrHelp):
			fmt.Fprintf(os.Stderr, "%v\n%s\n", err, inputsUsage)
			return subcommands.ExitUsageError
		default:
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return subcommands.ExitFailure
		}
	}
	return subcommands.ExitSuccess
}

func (c *inputsCommand) run(ctx context.Context, args []string) error {
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
	var depsLog *ninjautil.DepsLog
	if c.includeDeps {
		depsLogFile := filepath.Join(c.stateDir, c.depsLogFile)
		depsLog, err = ninjautil.NewDepsLog(ctx, depsLogFile)
		if err != nil {
			return fmt.Errorf("failed to load deps log: %w\nYou would need to build once?", err)
		}
	}
	nodes, err := state.Targets(args)
	if err != nil {
		return err
	}
	targets := make([]string, 0, len(nodes))
	for _, n := range nodes {
		targets = append(targets, n.Path())
	}
	g := &inputsGraph{
		state:   state,
		depsLog: depsLog,
		seen:    make(map[string]bool),
	}
	isTargets := make(map[string]bool)
	for _, t := range targets {
		isTargets[t] = true
		err := g.Traverse(ctx, t)
		if err != nil {
			return err
		}
	}
	var inputs []string
	for t := range g.seen {
		if isTargets[t] {
			continue
		}
		inputs = append(inputs, t)
	}
	sort.Strings(inputs)
	for _, in := range inputs {
		fmt.Println(in)
	}
	return nil
}

type inputsGraph struct {
	state   *ninjautil.State
	depsLog *ninjautil.DepsLog
	seen    map[string]bool
}

type targetNotFoundError struct {
	target string
}

func (e targetNotFoundError) Error() string {
	return fmt.Sprintf("target not found: %q", e.target)
}

func (g *inputsGraph) Traverse(ctx context.Context, target string) error {
	if g.seen[target] {
		return nil
	}
	g.seen[target] = true
	n, ok := g.state.LookupNodeByPath(target)
	if !ok {
		return targetNotFoundError{target: target}
	}
	edge, ok := n.InEdge()
	if !ok {
		return nil
	}
	for _, in := range edge.Inputs() {
		p := in.Path()
		err := g.Traverse(ctx, p)
		if err != nil {
			return err
		}
	}
	if g.depsLog == nil {
		return nil
	}
	var deps []string
	var err error
	switch edge.Binding("deps") {
	case "gcc", "msvc":
		deps, _, err = g.depsLog.RetrievePaths(ctx, target)
		if err != nil {
			return fmt.Errorf("deps log for target not found %q: %w", target, err)
		}
	default:
		// TODO: read depfile?
	}
	if len(deps) == 0 {
		return nil
	}
	for _, dep := range deps {
		err := g.Traverse(ctx, dep)
		var terr targetNotFoundError
		if errors.As(err, &terr) {
			clog.Infof(ctx, "target for deps %q: implicit header?", dep)
			continue
		}
		if err != nil {
			return err
		}
	}
	return nil
}
