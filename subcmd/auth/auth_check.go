// Copyright 2023 The Chromium Authors
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
	"go.chromium.org/build/siso/reapi"
)

// CheckCmd creates new CheckCommand.
func CheckCmd(authOpts cred.Options) *CheckCommand {
	return &CheckCommand{
		authOpts: authOpts,
	}
}

func (*CheckCommand) Name() string {
	return "auth-check"
}

func (*CheckCommand) Synopsis() string {
	return "prints current auth status"
}

func (*CheckCommand) Usage() string {
	return "Prints current auth status."
}

// CheckCommand implements auth-check subcommands.
type CheckCommand struct {
	authOpts  cred.Options
	projectID string
	reopt     *reapi.Option
}

func (c *CheckCommand) SetFlags(flagSet *flag.FlagSet) {
	flagSet.StringVar(&c.projectID, "project", os.Getenv("SISO_PROJECT"), "cloud project ID. can set by $SISO_PROJECT")

	c.reopt = new(reapi.Option)
	c.reopt.RegisterFlags(flagSet, reapi.Envs("REAPI"))
}

func (c *CheckCommand) Execute(ctx context.Context, flagSet *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	if flagSet.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "position arguments not expected\n")
		return subcommands.ExitUsageError
	}
	c.reopt.UpdateProjectID(c.projectID)
	err := c.reopt.CheckValid()
	if err != nil {
		fmt.Fprintf(os.Stderr, "reapi option is invalid: %v\n", err)
		return subcommands.ExitFailure
	}
	var credential cred.Cred
	if c.reopt.NeedCred() {
		credential, err = cred.New(ctx, c.reopt.ServiceURI(), c.authOpts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "auth error: %v\n", err)
			return subcommands.ExitFailure
		}
		fmt.Printf("Logged in by %s\n", credential.Type)
		if credential.Email != "" {
			fmt.Printf(" as %s\n", credential.Email)
		}
	} else {
		fmt.Fprintf(os.Stderr, "no credential required for reapi: %s\n", c.reopt)
	}
	client, err := reapi.New(ctx, credential, *c.reopt)
	fmt.Printf("use %s\n", c.reopt)
	if err != nil {
		fmt.Printf("access error: %v\n", err)
		return subcommands.ExitFailure
	}
	client.Close()
	return subcommands.ExitSuccess
}
