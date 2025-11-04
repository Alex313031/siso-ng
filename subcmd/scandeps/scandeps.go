// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package scandeps is scandeps subcommand for debugging scandeps.
package scandeps

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"maps"
	"os"
	"path/filepath"

	"github.com/google/subcommands"

	"go.chromium.org/build/siso/build/buildconfig"
	"go.chromium.org/build/siso/build/ninjabuild"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/scandeps"
)

const usage = `run scandeps

 $ siso scandeps -C <dir> -req '<json scandeps request>'

<json scandeps request> can be found in siso.INFO log
for "scandeps failed Request". you can copy-and-paste
the json string from the log.
Or you can manually construct json string of
infra/build/siso/scandeps.Request.
`

// Cmd returns the Command for the `scandeps` subcommand provided by this package.
func Cmd() *Command {
	return &Command{}
}

func (*Command) Name() string {
	return "scandeps"
}

func (*Command) Synopsis() string {
	return "run scandeps"
}

func (*Command) Usage() string {
	return usage
}

// Command implements scandeps subcommand.
type Command struct {
	dir       string
	stateDir  string
	reqString string
}

func (c *Command) SetFlags(flagSet *flag.FlagSet) {
	flagSet.StringVar(&c.dir, "C", ".", "ninja running directory to find .siso_config and .siso_filegroup for input_deps in state dir")
	flagSet.StringVar(&c.stateDir, "state_dir", ".", "state directory (relative to -C)")
	flagSet.StringVar(&c.reqString, "req", "", "json format of scandeps request")
}

func (c *Command) Execute(ctx context.Context, flagSet *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	err := c.run(ctx)
	if err != nil {
		switch {
		case errors.Is(err, flag.ErrHelp):
			fmt.Fprintf(os.Stderr, "%v\n%s\n", err, usage)
			return subcommands.ExitUsageError
		default:
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return subcommands.ExitFailure
		}
	}
	return subcommands.ExitSuccess
}

func (c *Command) run(ctx context.Context) error {
	if c.reqString == "" {
		return fmt.Errorf("missing req: %w", flag.ErrHelp)
	}
	var req scandeps.Request
	err := json.Unmarshal([]byte(c.reqString), &req)
	if err != nil {
		return err
	}
	fmt.Printf("request=%#v\n", req)
	inputDeps, err := loadInputDeps(filepath.Join(c.dir, c.stateDir))
	if err != nil {
		return err
	}
	clog.Infof(ctx, "input_deps=%q\n", inputDeps)

	execRoot, err := os.Getwd()
	if err != nil {
		return err
	}
	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		return err
	}

	s := scandeps.New(hashFS, inputDeps, nil)

	result, err := s.Scan(ctx, execRoot, req)
	if err != nil {
		return err
	}
	for _, r := range result {
		fmt.Println(r)
	}
	return nil
}

func loadInputDeps(dir string) (map[string][]string, error) {
	buf, err := os.ReadFile(filepath.Join(dir, ".siso_config"))
	if err != nil {
		return nil, err
	}
	var stepConfig ninjabuild.StepConfig
	err = json.Unmarshal(buf, &stepConfig)
	if err != nil {
		return nil, fmt.Errorf("load %s/.siso_config: %w", dir, err)
	}
	inputDeps := stepConfig.InputDeps

	buf, err = os.ReadFile(filepath.Join(dir, ".siso_filegroups"))
	if err != nil {
		return nil, err
	}
	var filegroups buildconfig.Filegroups
	err = json.Unmarshal(buf, &filegroups)
	if err != nil {
		return nil, fmt.Errorf("load %s/.filegroups: %w", dir, err)
	}
	maps.Copy(inputDeps, filegroups.Filegroups)
	return inputDeps, nil
}
