// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package query

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/google/subcommands"

	"go.chromium.org/build/siso/build/ninjabuild"
	"go.chromium.org/build/siso/toolsupport/ninjautil"
)

const graphUsage = `query ninja build graph

 $ siso query graph -C <dir>

prints deterministic JSON Lines (JSONL) encoding of the build graph.
`

type graphCommand struct {
	w io.Writer

	outDir ninjabuild.DirFlag
	fname  string
}

func (*graphCommand) Name() string {
	return "graph"
}

func (*graphCommand) Synopsis() string {
	return "JSON Lines dump of ninja build graph"
}

func (*graphCommand) Usage() string {
	return graphUsage
}

func (c *graphCommand) SetFlags(flagSet *flag.FlagSet) {
	c.outDir.RegisterFlags(flagSet)
	flagSet.StringVar(&c.fname, "f", "build.ninja", "input build filename (relative to -C)")
}

func (c *graphCommand) Execute(ctx context.Context, flagSet *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	if len(flagSet.Args()) > 0 {
		fmt.Fprintf(os.Stderr, "graph does not take positional arguments\n%s\n", graphUsage)
		return subcommands.ExitUsageError
	}
	if c.w == nil {
		c.w = os.Stdout
	}
	err := c.run(ctx)
	if err != nil {
		switch {
		case errors.Is(err, flag.ErrHelp):
			fmt.Fprintf(os.Stderr, "%v\n%s\n", err, graphUsage)
			return subcommands.ExitUsageError
		default:
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return subcommands.ExitFailure
		}
	}
	return subcommands.ExitSuccess
}

type edgeRecord struct {
	Output           string   `json:"output"`
	Rule             string   `json:"rule"`
	ExtraOutputs     []string `json:"extra_outputs,omitempty"`
	ImplicitOutputs  []string `json:"implicit_outputs,omitempty"`
	Inputs           []string `json:"inputs,omitempty"`
	ImplicitInputs   []string `json:"implicit_inputs,omitempty"`
	OrderOnlyInputs  []string `json:"order_only_inputs,omitempty"`
	ValidationInputs []string `json:"validation_inputs,omitempty"`
	Command          string   `json:"command,omitempty"`
	Rspfile          string   `json:"rspfile,omitempty"`
	RspfileContent   string   `json:"rspfile_content,omitempty"`
	Pool             string   `json:"pool,omitempty"`
	Depfile          string   `json:"depfile,omitempty"`
	Deps             string   `json:"deps,omitempty"`
	Restat           string   `json:"restat,omitempty"`
	Generator        string   `json:"generator,omitempty"`
}

func (c *graphCommand) run(ctx context.Context) error {
	state := ninjautil.NewState()
	p := ninjautil.NewManifestParser(state)
	_, _, _, err := ninjabuild.InitDir(ctx, c.outDir)
	if err != nil {
		return err
	}
	err = p.Load(ctx, c.fname)
	if err != nil {
		return err
	}

	edges := state.Edges()
	// Sort the graph by output name for determinism, thus allowing diff to verify if a graph has changed.
	slices.SortFunc(edges, func(a, b *ninjautil.Edge) int {
		return strings.Compare(a.ExplicitOutputs()[0].Path(), b.ExplicitOutputs()[0].Path())
	})

	bw := bufio.NewWriter(c.w)
	defer bw.Flush()

	// Stream rather than mashalling the entire graph to a json object to avoid
	// massive memory consumption on large graphs.
	// This can save many gigabytes of memory, and significantly improves
	// performance as well (likely due to the memory being reused).
	for _, edge := range edges {
		outs := edge.ExplicitOutputs()
		if len(outs) == 0 {
			continue
		}
		rec := edgeRecord{
			Output:           outs[0].Path(),
			Rule:             edge.RuleName(),
			ExtraOutputs:     sortedNodePaths(outs[1:]),
			ImplicitOutputs:  sortedNodePaths(edge.ImplicitOutputs()),
			Inputs:           sortedNodePaths(edge.Ins()),
			ImplicitInputs:   sortedNodePaths(edge.ImplicitInputs()),
			OrderOnlyInputs:  sortedNodePaths(edge.OrderOnlyInputs()),
			ValidationInputs: sortedNodePaths(edge.Validations()),
			Command:          edge.Binding("command"),
			Rspfile:          edge.Binding("rspfile"),
			RspfileContent:   edge.Binding("rspfile_content"),
			Depfile:          edge.Binding("depfile"),
			Deps:             edge.Binding("deps"),
			Restat:           edge.Binding("restat"),
			Generator:        edge.Binding("generator"),
		}
		if edge.Pool() != nil {
			rec.Pool = edge.Pool().Name()
		}

		b, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		_, err = bw.Write(b)
		if err != nil {
			return err
		}
		err = bw.WriteByte('\n')
		if err != nil {
			return err
		}
	}
	return nil
}

func sortedNodePaths(nodes []*ninjautil.Node) []string {
	if len(nodes) == 0 {
		return nil
	}
	paths := make([]string, len(nodes))
	for i, n := range nodes {
		paths[i] = n.Path()
	}
	slices.Sort(paths)
	return paths
}
