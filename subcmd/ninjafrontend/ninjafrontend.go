// Copyright 2025 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package ninjafrontend is ninjafrontend subcommand to parse ninja frontend protos.
package ninjafrontend

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/template"

	"github.com/google/subcommands"
	"google.golang.org/protobuf/proto"

	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/signals"
	pb "go.chromium.org/build/siso/toolsupport/soongutil/proto"
)

const usage = `ninjafrontend reader
Parse ninjafrontend proto.

 $ siso ninja -frontend_file=- .. | siso ninjafrontend -template '...'

e.g.
show message only
 --template='{{- with .Message}}{{with .Message}}{{.}}{{end}}{{end}}'

show edge output only
 --template='{{- with .EdgeFinished}}{{with .Output}}{{.}}{{end}}{{end}}'

see proto definition:
 https://chromium.googlesource.com/infra/infra/go/src/infra/+/refs/heads/main/build/siso/toolsupport/soongutil/proto/frontend.proto
see template syntax: https://pkg.go.dev/text/template
`

// Cmd returns the Command for the `ninjafrontend` subcommand provided by this package.
func Cmd() *Command {
	return &Command{}
}

func (*Command) Name() string {
	return "ninjafrontend"
}

func (*Command) Synopsis() string {
	return "ninjafrontend reader"
}

func (*Command) Usage() string {
	return usage
}

// Command implements ninjafrontend subcommand.
type Command struct {
	template string
}

func (c *Command) SetFlags(flagSet *flag.FlagSet) {
	flagSet.StringVar(&c.template, "template", "{{.}}\n", "template for frontend.Status message")
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

	tmpl, err := template.New("").Parse(c.template)
	if err != nil {
		return err
	}

	// https://android.googlesource.com/platform/build/soong/+/refs/heads/main/ui/status/ninja.go
	r := bufio.NewReader(os.Stdin)
	n := 0
	for {
		size, err := readVarInt(r)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading size of %d: %w", n, err)
		}
		clog.Infof(ctx, "entry %d size=%d", n, size)
		buf := make([]byte, size)
		_, err = io.ReadFull(r, buf)
		if err != nil {
			return fmt.Errorf("reading data for %d size=%d: %w", n, size, err)
		}
		msg := &pb.Status{}
		err = proto.Unmarshal(buf, msg)
		if err != nil {
			return fmt.Errorf("parsing data for %d size=%d: %w", n, size, err)
		}

		var sb strings.Builder
		err = tmpl.Execute(&sb, msg)
		if err != nil {
			return fmt.Errorf("template error for %v: %w", msg, err)
		}
		fmt.Print(sb.String())
	}
}

func readVarInt(r *bufio.Reader) (int, error) {
	ret := 0
	shift := uint(0)

	for {
		b, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		ret += int(b&0x7f) << (shift * 7)
		if b&0x80 == 0 {
			break
		}
		shift += 1
		if shift > 4 {
			return 0, fmt.Errorf("expected varint32 length-delimited message")
		}
	}
	return ret, nil
}
