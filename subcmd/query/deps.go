// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package query

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/google/subcommands"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/build/ninjabuild"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/toolsupport/makeutil"
	"go.chromium.org/build/siso/toolsupport/ninjautil"
)

const depsUsage = `show dependencies stored in the deps log or depfile

 $ siso query deps -C <dir> [<targets>]

print dependencies for targets stored in the deps log.

----
<target>: #deps <num> deps mtime <mtime> ([STALE|VALID])
  <deps>
  ...

----

or depfile
----
<target>: #depfile=<depfile> <num> deps mtime <mtime> VALID
  <deps>
  ...

----

`

func (*depsCommand) Name() string {
	return "deps"
}

func (*depsCommand) Synopsis() string {
	return "show dependencies stored in the deps log"
}

func (*depsCommand) Usage() string {
	return depsUsage
}

type depsCommand struct {
	dir         string
	stateDir    string
	fname       string
	fsopt       *hashfs.Option
	depsLogFile string
	raw         bool
	depfile     bool
}

func (c *depsCommand) SetFlags(flagSet *flag.FlagSet) {
	flagSet.StringVar(&c.dir, "C", ".", "ninja running directory to find dpes log")
	flagSet.StringVar(&c.stateDir, "state_dir", ".", "state directory (relative to -C)")
	flagSet.StringVar(&c.fname, "f", "build.ninja", "input build filename (relative to -C)")
	c.fsopt = new(hashfs.Option)
	c.fsopt.StateFile = ".siso_fs_state"
	c.fsopt.RegisterFlags(flagSet)
	flagSet.StringVar(&c.depsLogFile, "deps_log", ".siso_deps", "deps log filename (relative to -C, -state_dir)")
	flagSet.BoolVar(&c.raw, "raw", false, "just check deps log. (no build.ninja nor .siso_fs_state needed)")
	flagSet.BoolVar(&c.depfile, "depfile", false, "check depfile too")
}

func (c *depsCommand) Execute(ctx context.Context, flagSet *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	err := c.run(ctx, flagSet.Args())
	if err != nil {
		switch {
		case errors.Is(err, flag.ErrHelp):
			fmt.Fprintf(os.Stderr, "%v\n%s\n", err, depsUsage)
			return subcommands.ExitUsageError
		default:
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return subcommands.ExitFailure
		}
	}
	return subcommands.ExitSuccess
}

func (c *depsCommand) run(ctx context.Context, args []string) error {
	execRoot, err := os.Getwd()
	if err != nil {
		return err
	}
	execRoot, err = filepath.EvalSymlinks(execRoot)
	if err != nil {
		return err
	}
	err = os.Chdir(c.dir)
	if err != nil {
		return err
	}

	if c.fsopt.StateFile != "" {
		c.fsopt.StateFile = filepath.Join(c.stateDir, c.fsopt.StateFile)
	}
	depsLogFile := filepath.Join(c.stateDir, c.depsLogFile)
	depsLog, err := ninjautil.NewDepsLog(ctx, depsLogFile)
	if err != nil {
		return err
	}

	var hashFS *hashfs.HashFS
	var state *ninjautil.State
	targets := args
	if c.raw {
		if len(targets) == 0 {
			targets = depsLog.RecordedTargets()
		}
	} else {
		var err error
		hashFS, err = hashfs.New(ctx, hashfs.Option{})
		if err != nil {
			return err
		}
		fsstate, err := hashfs.Load(ctx, hashfs.Option{StateFile: c.fsopt.StateFile})
		if err != nil {
			return err
		}
		err = hashFS.SetState(ctx, fsstate)
		if err != nil {
			return err
		}

		state = ninjautil.NewState()
		p := ninjautil.NewManifestParser(state)
		err = p.Load(ctx, c.fname)
		if err != nil {
			return err
		}
		targets, err = depsTargets(state, args)
		if err != nil {
			return err
		}
	}
	if !c.depfile {
		state = nil
	}
	w := bufio.NewWriter(os.Stdout)
	bpath := build.NewPath(execRoot, c.dir)
	for _, target := range targets {
		depType, deps, depsTime, depState, err := lookupDeps(ctx, state, hashFS, depsLog, bpath, target)
		if err != nil {
			if errors.Is(err, ninjautil.ErrNoDepsLog) {
				continue
			}
			fmt.Fprintf(w, "%s: deps log error: %v\n", target, err)
			continue
		}
		var buf bytes.Buffer
		fmt.Fprintf(&buf, "%s: #%s %d, deps mtime %d (%s)\n",
			target, depType, len(deps), depsTime.Nanosecond(), depState)
		for _, d := range deps {
			fmt.Fprintf(&buf, "    %s\n", d)
		}
		fmt.Fprintln(w, buf.String())
	}
	return w.Flush()
}

func lookupDeps(ctx context.Context, state *ninjautil.State, hashFS *hashfs.HashFS, depsLog *ninjautil.DepsLog, bpath *build.Path, target string) (string, []string, time.Time, ninjabuild.DepsLogState, error) {
	var depState ninjabuild.DepsLogState
	deps, depsTime, err := depsLog.RetrievePaths(ctx, target)
	if err == nil {
		if hashFS != nil {
			depState, _ = ninjabuild.CheckDepsLogState(ctx, hashFS, bpath, target, depsTime)
		}
		return "deps", deps, depsTime, depState, err
	}
	if state == nil {
		return "", nil, time.Time{}, depState, ninjautil.ErrNoDepsLog
	}
	node, ok := state.LookupNodeByPath(target)
	if !ok {
		return "", nil, time.Time{}, depState, fmt.Errorf("no such target in build graph: %q", target)
	}
	edge, ok := node.InEdge()
	if !ok {
		return "", nil, time.Time{}, depState, fmt.Errorf("no rule to build target: %q", target)
	}
	depsType := edge.Binding("deps")
	switch depsType {
	case "gcc", "msvc":
		// for deps=gcc|msvc, deps is recorded in deps log.
		return "", nil, time.Time{}, depState, ninjautil.ErrNoDepsLog
	case "":
		// check depfile
	default:
		return "", nil, time.Time{}, depState, fmt.Errorf("unknown deps=%q in rule to build target %q", depsType, target)
	}
	depfile := edge.UnescapedBinding("depfile")
	if depfile == "" {
		// the rule has no deps,depfile.
		return "", nil, time.Time{}, depState, ninjautil.ErrNoDepsLog
	}
	df := bpath.MaybeFromWD(ctx, depfile)
	fi, err := hashFS.Stat(ctx, bpath.ExecRoot, df)
	if err != nil {
		return "", nil, time.Time{}, depState, fmt.Errorf("no depfile=%q to build target %q: %w", depfile, target, err)
	}
	fsys := hashFS.FileSystem(ctx, bpath.ExecRoot)
	deps, err = makeutil.ParseDepsFile(ctx, fsys, df)
	if err != nil {
		return "", nil, time.Time{}, depState, fmt.Errorf("failed to read depfile=%q to build target %q: %w", depfile, target, err)
	}
	return fmt.Sprintf("depfile=%q", depfile), deps, fi.ModTime(), ninjabuild.DepsLogValid, nil
}

func depsTargets(state *ninjautil.State, args []string) ([]string, error) {
	var nodes []*ninjautil.Node
	if len(args) > 0 {
		var err error
		nodes, err = state.Targets(args)
		if err != nil {
			return nil, err
		}
	} else {
		// for empty args, not use "defaults", but use all deps log entries.
		nodes = state.AllNodes()
		slices.SortFunc(nodes, func(a, b *ninjautil.Node) int {
			return strings.Compare(a.Path(), b.Path())
		})
	}
	targets := make([]string, 0, len(nodes))
	for _, node := range nodes {
		targets = append(targets, node.Path())
	}
	return targets, nil
}
