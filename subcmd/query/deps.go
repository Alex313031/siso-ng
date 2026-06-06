// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package query

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/google/subcommands"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/build/ninjabuild"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/reapi/digest"
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
	outDir      ninjabuild.DirFlag
	stateDir    string
	fname       string
	fsopt       *hashfs.Option
	depsLogFile string
	raw         bool
	depfile     bool
	format      string
}

type dependencies struct {
	Target     string                  `json:"target"`
	DepType    string                  `json:"dep_type"`
	Deps       []string                `json:"deps"`
	DepsTime   time.Time               `json:"deps_time"`
	DepsDigest digest.Digest           `json:"deps_digest"`
	DepState   ninjabuild.DepsLogState `json:"dep_state"`
}

type marshaller interface{ Marshal(dependencies) error }

type jsonMarshaller struct{ e *json.Encoder }

func (m jsonMarshaller) Marshal(dep dependencies) error {
	return m.e.Encode(dep)
}

type textMarshaller struct{ w io.Writer }

func (m textMarshaller) Marshal(dep dependencies) error {
	var buf bytes.Buffer
	var key string
	switch dep.DepState {
	case ninjabuild.DepsLogValid:
		key = fmt.Sprintf("mtime %d", dep.DepsTime.Nanosecond())
	case ninjabuild.DepsLogValidDigest:
		key = fmt.Sprintf("digest %s", dep.DepsDigest)
	default:
		key = fmt.Sprintf("mtime %d digest %s", dep.DepsTime.Nanosecond(), dep.DepsDigest)
	}
	fmt.Fprintf(&buf, "%s: #%s %d, deps %s (%s)\n",
		dep.Target, dep.DepType, len(dep.Deps), key, dep.DepState)
	for _, d := range dep.Deps {
		fmt.Fprintf(&buf, "    %s\n", d)
	}
	fmt.Fprintln(m.w, buf.String())
	return nil
}

func (c *depsCommand) SetFlags(flagSet *flag.FlagSet) {
	c.outDir.RegisterFlags(flagSet)
	flagSet.StringVar(&c.stateDir, "state_dir", ".", "state directory (relative to -C)")
	flagSet.StringVar(&c.fname, "f", "build.ninja", "input build filename (relative to -C)")
	c.fsopt = new(hashfs.Option)
	c.fsopt.StateFile = ".siso_fs_state"
	c.fsopt.RegisterFlags(flagSet)
	flagSet.StringVar(&c.depsLogFile, "deps_log", ".siso_deps", "deps log filename (relative to -C, -state_dir)")
	flagSet.BoolVar(&c.raw, "raw", false, "just check deps log. (no build.ninja nor .siso_fs_state needed)")
	flagSet.BoolVar(&c.depfile, "depfile", false, "check depfile too")
	flagSet.StringVar(&c.format, "format", "text", "the format of dependencies (use text or json)")
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
	_, workspaceRoot, outDir, err := ninjabuild.InitDir(ctx, c.outDir)
	if err != nil {
		return err
	}
	if c.fsopt.StateFile != "" {
		c.fsopt.StateFile = filepath.Join(c.stateDir, c.fsopt.StateFile)
	}
	depsLogFile := filepath.Join(c.stateDir, c.depsLogFile)
	depsLog, err := ninjabuild.NewDepsLog(ctx, depsLogFile)
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

	bpath := build.NewPath(workspaceRoot, outDir)

	var m marshaller
	w := bufio.NewWriter(os.Stdout)
	switch c.format {
	case "json":
		m = jsonMarshaller{e: json.NewEncoder(w)}
	case "text":
		m = textMarshaller{w: w}
	default:
		return fmt.Errorf("invalid format %q", c.format)
	}

	for _, target := range targets {
		depType, deps, key, depState, err := lookupDeps(ctx, state, hashFS, depsLog, bpath, target)
		if err != nil {
			if errors.Is(err, ninjautil.ErrNoDepsLog) {
				continue
			}
			fmt.Fprintf(os.Stderr, "%s: deps log error: %v\n", target, err)
			continue
		}
		if err = m.Marshal(dependencies{Target: target, DepType: depType, Deps: deps, DepsTime: key.Mtime, DepsDigest: key.Digest, DepState: depState}); err != nil {
			return fmt.Errorf("failed to encode: %w for %q format", err, c.format)
		}
	}
	return w.Flush()
}

func lookupDeps(ctx context.Context, state *ninjautil.State, hashFS *hashfs.HashFS, depsLog *ninjabuild.DepsLog, bpath *build.Path, target string) (string, []string, ninjabuild.DepsLogKey, ninjabuild.DepsLogState, error) {
	var depState ninjabuild.DepsLogState
	deps, key, err := depsLog.RetrievePaths(ctx, target)
	if err == nil {
		if hashFS != nil {
			depState, _ = depsLog.CheckKey(ctx, hashFS, bpath, key)
		}
		return "deps", deps, key, depState, err
	}
	if state == nil {
		return "", nil, key, depState, ninjautil.ErrNoDepsLog
	}
	node, ok := state.LookupNodeByPath(target)
	if !ok {
		return "", nil, key, depState, fmt.Errorf("no such target in build graph: %q", target)
	}
	edge, ok := node.InEdge()
	if !ok {
		return "", nil, key, depState, fmt.Errorf("no rule to build target: %q", target)
	}
	depsType := edge.Binding("deps")
	switch depsType {
	case "gcc", "msvc":
		// for deps=gcc|msvc, deps is recorded in deps log.
		return "", nil, key, depState, ninjautil.ErrNoDepsLog
	case "":
		// check depfile
	default:
		return "", nil, key, depState, fmt.Errorf("unknown deps=%q in rule to build target %q", depsType, target)
	}
	depfile := edge.UnescapedBinding("depfile")
	if depfile == "" {
		// the rule has no deps,depfile.
		return "", nil, key, depState, ninjautil.ErrNoDepsLog
	}
	df := bpath.MaybeFromRelative(ctx, depfile)
	fi, err := hashFS.Stat(ctx, bpath.WorkspaceRoot, df)
	if err != nil {
		return "", nil, key, depState, fmt.Errorf("no depfile=%q to build target %q: %w", depfile, target, err)
	}
	ents, err := hashFS.Entries(ctx, bpath.WorkspaceRoot, []string{df})
	if err != nil || len(ents) == 0 {
		return "", nil, key, depState, fmt.Errorf("failed to get entry for depfile=%q %d to build target %q: %w", depfile, len(ents), target, err)
	}
	fsys := hashFS.FileSystem(ctx, bpath.WorkspaceRoot)
	deps, err = makeutil.ParseDepsFile(ctx, fsys, df)
	if err != nil {
		return "", nil, key, depState, fmt.Errorf("failed to read depfile=%q to build target %q: %w", depfile, target, err)
	}
	key.Target = target
	key.Mtime = fi.ModTime()
	key.Digest = ents[0].Data.Digest()
	return fmt.Sprintf("depfile=%q", depfile), deps, key, ninjabuild.DepsLogValid, nil
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
