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

const ruleUsage = `query build rule

 $ siso query rule -C <dir> <targets>

prints filtered ninja build rules for <targets>.
`

func (*ruleCommand) Name() string {
	return "rule"
}

func (*ruleCommand) Synopsis() string {
	return "query build step rule"
}

func (*ruleCommand) Usage() string {
	return ruleUsage
}

type ruleCommand struct {
	dir     string
	fname   string
	binding string
}

func (c *ruleCommand) SetFlags(flagSet *flag.FlagSet) {
	flagSet.StringVar(&c.dir, "C", ".", "ninja running directory to find build.ninja")
	flagSet.StringVar(&c.fname, "f", "build.ninja", "input build filename (relative to -C")
	flagSet.StringVar(&c.binding, "binding", "", "print binding value for the target")
}

func (c *ruleCommand) Execute(ctx context.Context, flagSet *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	err := c.run(ctx, flagSet.Args())
	if err != nil {
		switch {
		case errors.Is(err, flag.ErrHelp):
			fmt.Fprintf(os.Stderr, "%v\n%s\n", err, ruleUsage)
			return subcommands.ExitUsageError
		default:
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return subcommands.ExitFailure
		}
	}
	return subcommands.ExitSuccess
}

func (c *ruleCommand) run(ctx context.Context, args []string) error {
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
	for _, node := range nodes {
		edge, ok := node.InEdge()
		if !ok {
			fmt.Printf("# no rule to build %s\n\n", node.Path())
			continue
		}
		if c.binding != "" {
			fmt.Println(edge.Binding(c.binding))
			continue
		}
		edge.Print(os.Stdout)
		fmt.Printf("# %s is used by the following targets\n", node.Path())
		for _, edge := range node.OutEdges() {
			outs := edge.Outputs()
			if len(outs) == 0 {
				continue
			}
			fmt.Printf("#  %s\n", outs[0].Path())
		}
		fmt.Printf("\n\n")
	}
	return nil
}
