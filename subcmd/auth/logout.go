// Copyright 2025 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package auth

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/google/subcommands"

	"go.chromium.org/build/siso/auth/cred"
)

// LogoutCmd creates new LogoutCommand.
func LogoutCmd(authOpts func() cred.Options) *LogoutCommand {
	return &LogoutCommand{
		authOpts: authOpts,
	}
}

func (*LogoutCommand) Name() string {
	return "logout"
}

func (*LogoutCommand) Synopsis() string {
	return "logout from siso system"
}

func (*LogoutCommand) Usage() string {
	return "logout from siso system.\n"
}

// LogoutCommand implements logout subcommand.
type LogoutCommand struct {
	authOpts func() cred.Options
}

func (*LogoutCommand) SetFlags(flagSet *flag.FlagSet) {}

func (c *LogoutCommand) Execute(ctx context.Context, flagSet *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	authOps := c.authOpts()
	fmt.Printf("using %s for auth\n", authOps.Type)
	err := authOps.Logout(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return subcommands.ExitFailure
	}
	return subcommands.ExitSuccess
}
