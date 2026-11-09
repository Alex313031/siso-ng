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
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/google/subcommands"
	"golang.org/x/net/trace"
	"google.golang.org/grpc"

	"go.chromium.org/build/siso/auth/cred"
	"go.chromium.org/build/siso/reapi"
)

const usage = `proxy RE API service.

 $ siso proxy [-project <project>] [-reapi_address <addr>] [-reapi_instance <instance>] \
    -addr unix:///<path>

 $ siso ninja -reapi_address unix:///<path> --reapi_insecure=true ...
`

// Cmd returns the Command for the `proxy` subcommand provided by this package.
func Cmd(authOpts func() cred.Options) *Command {
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
	authOpts  func() cred.Options
	projectID string
	reopt     *reapi.Option
	addr      string
	traceAddr string
}

func (c *Command) SetFlags(flagSet *flag.FlagSet) {
	flagSet.StringVar(&c.projectID, "project", os.Getenv("SISO_PROJECT"), "cloud project ID. can be set by $SISO_PROJECT")
	c.reopt = new(reapi.Option)
	c.reopt.RegisterFlags(flagSet, reapi.Envs("REAPI"))
	flagSet.StringVar(&c.addr, "addr", "", "address to listen on")
	flagSet.StringVar(&c.traceAddr, "trace_addr", os.Getenv("SISO_PROXY_TRACE_ADDR"), "address to listen on for trace debug")
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
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	if c.traceAddr != "" {
		grpc.EnableTracing = true
		// allow any data from anywhere
		trace.AuthRequest = func(*http.Request) (bool, bool) { return true, true }
		go func() {
			err := http.ListenAndServe(c.traceAddr, nil)
			if err != nil {
				log.Fatalf("trace page: %v", err)
			}
		}()
	}

	c.reopt.UpdateProjectID(c.projectID)
	var credential cred.Cred
	err := c.reopt.CheckValid()
	if err != nil {
		return fmt.Errorf("reapi option is invalid: %w", err)
	}
	if c.reopt.NeedCred() {
		credential, err = cred.New(ctx, c.reopt.ServiceURI(), c.authOpts())
		if err != nil {
			return err
		}
	}
	ctx = reapi.NewContext(ctx, nil)
	client, err := reapi.New(ctx, credential, *c.reopt)
	if err == nil {
		err = client.Init(ctx)
	}
	if err != nil {
		return err
	}
	defer client.Close()

	proxy := reapi.NewProxy(client, c.addr)
	fmt.Printf("listening on %s\n", c.addr)
	return proxy.Serve(ctx)
}
