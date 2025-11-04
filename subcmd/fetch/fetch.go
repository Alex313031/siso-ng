// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package fetch is fetch subcommand to fetch data from CAS.
package fetch

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"strings"

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/google/subcommands"
	"google.golang.org/protobuf/proto"

	"go.chromium.org/build/siso/auth/cred"
	"go.chromium.org/build/siso/reapi"
	"go.chromium.org/build/siso/reapi/digest"
	"go.chromium.org/build/siso/reapi/merkletree/exporter"
	"go.chromium.org/build/siso/signals"
)

const usage = `fetch contents from CAS.
Print contents to stdout, or extract in <dir> for -type dir-extract.

 $ siso fetch -project <project> -reapi_instnace <instnace> \
          [-type <type>] \
          <digest> [<dir>]

 $ siso fetch [-type <type>] \
  bytesteam://<endpoint>/projects/<project>/instances/<instance>/blobs/<digest>

<type> is
  raw: raw content
  command: command message in text proto format
  action: action message in text proto format
  dir: directory message in text proto format
  tree: tree message in text proto format
  dir-extract: extract to <dir> (if <dir> is specified)
               or list (if <dir> is not specified)
`

// Cmd returns the Command for the `fetch` subcommand provided by this package.
func Cmd(authOpts cred.Options) *Command {
	return &Command{
		authOpts: authOpts,
	}
}

func (*Command) Name() string {
	return "fetch"
}

func (*Command) Synopsis() string {
	return "fetch contents"
}

func (*Command) Usage() string {
	return usage
}

// Command implements fetch subcommand.
type Command struct {
	Flags     *flag.FlagSet
	authOpts  cred.Options
	projectID string
	reopt     *reapi.Option
	dataType  string
}

func (c *Command) SetFlags(flagSet *flag.FlagSet) {
	flagSet.StringVar(&c.projectID, "project", os.Getenv("SISO_PROJECT"), "cloud project ID. can be set by $SISO_PROJECT")
	c.reopt = new(reapi.Option)
	c.reopt.RegisterFlags(flagSet, reapi.Envs("REAPI"))
	flagSet.StringVar(&c.dataType, "type", "raw", `data type. "raw", "command", "action", "dir", "tree", "dir-extract"`)
}

func (c *Command) Execute(ctx context.Context, flagSet *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	c.Flags = flagSet
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

	if c.Flags.NArg() == 0 {
		return fmt.Errorf("no digest nor bytestream uri: %w", flag.ErrHelp)
	}
	var digestStr string
	if strings.HasPrefix(c.Flags.Arg(0), "bytestream://") {
		bsurl, err := url.Parse(c.Flags.Arg(0))
		if err != nil {
			return fmt.Errorf("invalid bytestream uri: %w", err)
		}
		c.reopt.Address = bsurl.Host
		elems := strings.Split(strings.TrimPrefix(bsurl.EscapedPath(), "/"), "/")
		// projects/<project>/instances/<instance>/blobs/<hash>/<size>
		if len(elems) != 7 {
			return fmt.Errorf("invlaid bytestream uri: path=%q", strings.Join(elems, "/"))
		}
		if elems[0] != "projects" || elems[2] != "instances" || elems[4] != "blobs" {
			return fmt.Errorf("invlaid bytestream uri: path=%q", strings.Join(elems, "/"))
		}
		c.projectID = elems[1]
		c.reopt.Instance = path.Join(elems[0:4]...)
		digestStr = path.Join(elems[5:]...)
	} else {
		digestStr = c.Flags.Arg(0)
	}

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
	d, err := digest.Parse(digestStr)
	if err != nil {
		return err
	}
	client, err := reapi.New(ctx, credential, *c.reopt)
	if err != nil {
		return err
	}
	defer client.Close()

	var pmsg proto.Message
	switch c.dataType {
	case "raw":
	case "command":
		pmsg = &rpb.Command{}
	case "action":
		pmsg = &rpb.Action{}
	case "dir":
		pmsg = &rpb.Directory{}
	case "tree":
		pmsg = &rpb.Tree{}
	case "dir-extract":
		var w io.Writer
		dir := "."
		if c.Flags.NArg() > 1 {
			dir = c.Flags.Arg(1)
			fmt.Printf("extract %s to %s\n", d, dir)
		} else {
			w = os.Stdout
			fmt.Printf("list %s\n", d)
		}

		b, err := client.Get(ctx, d, d.String())
		if err != nil {
			return err
		}
		pmsg := &rpb.Directory{}
		err = c.protoUnmarshal(b, pmsg)
		if err != nil {
			return fmt.Errorf("failed to unmarshal %s as %T: %w", d, pmsg, err)
		}
		exporter := exporter.New(client)
		err = exporter.Export(ctx, dir, d, w)
		if err != nil {
			return fmt.Errorf("error from exporter.Export: %w", err)
		}
		return nil
	default:
		var w strings.Builder
		c.Flags.SetOutput(&w)
		c.Flags.PrintDefaults()
		return fmt.Errorf("unknown type %s\n%s\n%w", c.dataType, w.String(), flag.ErrHelp)
	}
	b, err := client.Get(ctx, d, d.String())
	if err != nil {
		return fmt.Errorf("error from client.Get: %w", err)
	}
	if pmsg == nil {
		_, err = os.Stdout.Write(b)
		if err != nil {
			return err
		}
		return nil
	}
	err = c.protoUnmarshal(b, pmsg)
	if err != nil {
		return fmt.Errorf("failed to unmarshal %s as %T: %w", d, pmsg, err)
	}
	fmt.Println(pmsg)
	return nil
}

func (c *Command) protoUnmarshal(b []byte, msg proto.Message) error {
	err := proto.Unmarshal(b, msg)
	if err != nil {
		return err
	}
	unknown := msg.ProtoReflect().GetUnknown()
	if len(unknown) > 0 {
		return fmt.Errorf("unknown fields in marshaled proto: %v", msg)
	}
	return nil
}
