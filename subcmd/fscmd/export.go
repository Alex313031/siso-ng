// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package fscmd

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/google/subcommands"
	"google.golang.org/protobuf/encoding/prototext"

	"go.chromium.org/build/siso/hashfs"
)

func (*exportCommand) Name() string {
	return "export"
}

func (*exportCommand) Synopsis() string {
	return "export siso hashfs data"
}

func (*exportCommand) Usage() string {
	return "export siso hashfs data to stdout."
}

type exportCommand struct {
	dir       string
	format    string
	stateFile string
}

func (c *exportCommand) SetFlags(flagSet *flag.FlagSet) {
	flagSet.StringVar(&c.dir, "C", ".", "ninja running directory")
	flagSet.StringVar(&c.format, "format", "json", "output format. json or prototext")
	flagSet.StringVar(&c.stateFile, "fs_state", stateFile, "fs state filename")
}

func (c *exportCommand) Execute(ctx context.Context, flagSet *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	err := os.Chdir(c.dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to chdir %s: %v\n", c.dir, err)
		return 1
	}

	st, err := hashfs.Load(ctx, hashfs.Option{StateFile: c.stateFile})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load %s: %v\n", c.stateFile, err)
		return 1
	}
	var buf []byte
	switch c.format {
	case "json":
		buf, err = json.MarshalIndent(st, "", " ")
	case "prototext":
		buf, err = prototext.MarshalOptions{
			Multiline: true,
			Indent:    " ",
		}.Marshal(st)
	default:
		fmt.Fprintf(os.Stderr, "unknown format %s\n", c.format)
		return 2
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "marshal error: %v\n", err)
		return 1
	}
	os.Stdout.Write(buf)
	return 0
}
