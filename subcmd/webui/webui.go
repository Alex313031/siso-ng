// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package webui provides webui subcommand.
package webui

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/google/subcommands"

	"go.chromium.org/build/siso/webui"
)

// Cmd returns the Command for the `webui` subcommand.
func Cmd(version string) *Command {
	return &Command{
		version: version,
	}
}

func (*Command) Name() string {
	return "webui"
}

func (*Command) Synopsis() string {
	return "starts the experimental webui"
}

func (*Command) Usage() string {
	return "Starts the experimental webui. Not ready for wide use yet, requires static files to work. This is subject to breaking changes at any moment."
}

// Command implements webui subcommand.
type Command struct {
	version          string
	localDevelopment bool
	port             int
	outdir           string
	configRepoDir    string
	fname            string
	metricsFile      string
}

func (c *Command) SetFlags(flagSet *flag.FlagSet) {
	flagSet.BoolVar(&c.localDevelopment, "local_development", false, "whether to use local instead of embedded files")
	flagSet.IntVar(&c.port, "port", 8080, "port to use (defaults to 8080)")
	flagSet.StringVar(&c.outdir, "C", ".", "path to outdir")
	flagSet.StringVar(&c.configRepoDir, "config_repo_dir", "build/config/siso", "config repo directory (relative to exec root)")
	flagSet.StringVar(&c.fname, "f", "build.ninja", "input build manifest filename (relative to -C)")
	flagSet.StringVar(&c.metricsFile, "metrics_file", "", "optional path to siso_metrics.json to load (experimental, -C is still required for now)")
}

func (c *Command) Execute(ctx context.Context, flagSet *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	s, err := webui.NewServer(c.version, c.localDevelopment, c.port, c.outdir, c.configRepoDir, c.fname)
	if err != nil {
		var execrootNotExist *webui.ErrExecrootNotExist
		var manifestNotExist *webui.ErrManifestNotExist
		if errors.As(err, &execrootNotExist) {
			fmt.Fprintf(os.Stderr, "%v: need `-config_repo_dir <dir>` and/or `-C <dir>`?\n", execrootNotExist)
		} else if errors.As(err, &manifestNotExist) {
			fmt.Fprintf(os.Stderr, "%v: need `-C <dir>` and/or `-f <manifest>`?\n", manifestNotExist)
		} else {
			fmt.Fprintf(os.Stderr, "failed to init server: %v\n", err)
		}
		return subcommands.ExitFailure
	}
	if c.metricsFile != "" {
		err = s.LoadStandaloneMetrics(c.metricsFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to load metrics_file: %v\n", err)
			return subcommands.ExitFailure
		}
	}
	r := s.Serve()
	if r != 0 {
		return subcommands.ExitFailure
	}
	return subcommands.ExitSuccess
}
