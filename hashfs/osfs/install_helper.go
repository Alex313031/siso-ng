// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package osfs

import (
	"context"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/google/subcommands"
)

// HelperCmd creates new HelperCommand.
func HelperCmd() *HelperCommand {
	return &HelperCommand{}
}

func (*HelperCommand) Name() string {
	return "install-helper"
}

func (*HelperCommand) Synopsis() string {
	return "helper tool to install executable"
}

func (*HelperCommand) Usage() string {
	return `helper tool to install executable.

User would not need to run this sub command.
It is intended as workaround for https://github.com/golang/go/issues/22315.
This tool just writes new executable in specified file with mode
by using content given in stdin.
`
}

// HelperCommand implements install-helper command,
// which is invoked by siso to install executable file.
type HelperCommand struct {
	mode int
	file string
}

func (c *HelperCommand) SetFlags(flagSet *flag.FlagSet) {
	flagSet.IntVar(&c.mode, "m", 0, "file mode")
	flagSet.StringVar(&c.file, "o", "", "output filename")
}

func (c *HelperCommand) Execute(ctx context.Context, flagSet *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	err := c.run()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return subcommands.ExitFailure
	}
	return subcommands.ExitSuccess
}

func (c *HelperCommand) run() error {
	if c.mode&0700 == 0 {
		return fmt.Errorf("invalid mode 0%o: %s", c.mode, fs.FileMode(c.mode))
	}
	if c.file == "" {
		return fmt.Errorf("file not specified")
	}
	w, err := os.OpenFile(c.file, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, os.FileMode(c.mode))
	if err != nil {
		return err
	}
	_, err = io.Copy(w, os.Stdin)
	cerr := w.Close()
	if err != nil {
		return err
	}
	if cerr != nil {
		return cerr
	}
	return nil
}
