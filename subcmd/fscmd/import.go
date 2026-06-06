// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package fscmd

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/google/subcommands"
	"google.golang.org/protobuf/encoding/prototext"

	"go.chromium.org/build/siso/build/ninjabuild"
	"go.chromium.org/build/siso/hashfs"
	pb "go.chromium.org/build/siso/hashfs/proto"
)

func (*importCommand) Name() string {
	return "import"
}

func (*importCommand) Synopsis() string {
	return "import siso hashfs data"
}

func (*importCommand) Usage() string {
	return "import siso hashfs data from stdin."
}

type importCommand struct {
	outDir ninjabuild.DirFlag
	format string
}

func (c *importCommand) SetFlags(flagSet *flag.FlagSet) {
	c.outDir.RegisterFlags(flagSet)
	flagSet.StringVar(&c.format, "format", "json", "input format. json or prototext")
}

func (c *importCommand) Execute(ctx context.Context, flagSet *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	_, _, _, err := ninjabuild.InitDir(ctx, c.outDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to init dir %s: %v\n", c.outDir, err)
		return 1
	}

	buf, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to read from stdin: %v\n", err)
		return 1
	}

	st := &pb.State{}
	switch c.format {
	case "json":
		err = json.Unmarshal(buf, st)
	case "prototext":
		err = prototext.Unmarshal(buf, st)
	default:
		fmt.Fprintf(os.Stderr, "unknown format %s\n", c.format)
		return 2
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "unmarshal error: %v\n", err)
		return 1
	}
	os.Remove(".siso_last_targets")
	err = hashfs.Save(ctx, st, hashfs.Option{StateFile: stateFile})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to save %s: %v\n", stateFile, err)
		return 1
	}
	return 0
}
