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

// LoginCmd creates new LoginCommand.
func LoginCmd(authOpts cred.Options) *LoginCommand {
	return &LoginCommand{
		authOpts: authOpts,
	}
}

func (*LoginCommand) Name() string {
	return "login"
}

func (*LoginCommand) Synopsis() string {
	return "login to siso system"
}

func (*LoginCommand) Usage() string {
	return "login to siso system."
}

// LoginCommand implements login subcommand.
type LoginCommand struct {
	authOpts cred.Options
}

func (*LoginCommand) SetFlags(flagSet *flag.FlagSet) {}

func (c *LoginCommand) Execute(ctx context.Context, flagSet *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	fmt.Printf("using %s for auth\n", c.authOpts.Type)
	err := c.authOpts.Login(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		fmt.Fprintf(os.Stderr, "run `siso auth-check` to check auth status?\n")
		return subcommands.ExitFailure
	}
	return subcommands.ExitSuccess
}
