// Copyright 2025 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package proxy is proxy subcommand to proxy RE-API service.
package proxy

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/google/subcommands"

	"go.chromium.org/build/siso/auth/cred"
	"go.chromium.org/build/siso/reapi"
	"go.chromium.org/build/siso/signals"
)

const usage = `proxy RE API service.

 $ siso proxy -reapi_address <addr> -reapi_instance <instance> \
    -addr unix:///<path>

 $ siso ninja -reapi_address unix:///<path> --reapi_insecure=true ...
`

// Cmd returns the Command for the `proxy` subcommand provided by this package.
func Cmd(authOpts cred.Options) *Command {
	return &Command{
		authOpts: authOpts,
	}
}

func (*Command) Name() string {
	return "proxy"
}

func (*Command) Synopsis() string {
	return "proxy RE-API service"
}

func (*Command) Usage() string {
	return usage
}

// Command implements proxy subcommand.
type Command struct {
	authOpts  cred.Options
	projectID string
	reopt     *reapi.Option
	addr      string
}

func (c *Command) SetFlags(flagSet *flag.FlagSet) {
	flagSet.StringVar(&c.projectID, "project", os.Getenv("SISO_PROJECT"), "cloud project ID. can be set by $SISO_PROJECT")
	c.reopt = new(reapi.Option)
	c.reopt.RegisterFlags(flagSet, reapi.Envs("REAPI"))
	flagSet.StringVar(&c.addr, "addr", "", "address to listen on")
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
	ctx, cancel := context.WithCancel(ctx)
	defer signals.HandleInterrupt(ctx, cancel)()

	c.reopt.UpdateProjectID(c.projectID)
	var credential cred.Cred
	err := c.reopt.CheckValid()
	if err != nil {
		return fmt.Errorf("reapi option is invalid: %w", err)
	}
	if c.reopt.NeedCred() {
		credential, err = cred.New(ctx, c.reopt.ServiceURI(), c.authOpts)
		if err != nil {
			return err
		}
	}
	client, err := reapi.New(ctx, credential, *c.reopt)
	if err != nil {
		return err
	}
	defer client.Close()

	proxy := reapi.NewProxy(client, c.addr)
	fmt.Printf("listening on %s\n", c.addr)
	return proxy.Serve(ctx)
}
