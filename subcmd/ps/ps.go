// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package ps is ps subcommand to list up active steps of ninja build.
package ps

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/google/subcommands"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/signals"
	"go.chromium.org/build/siso/ui"
)

// Cmd returns the Command for the `ps` subcommand.
func Cmd() *Command {
	return &Command{}
}

func (*Command) Name() string {
	return "ps"
}

func (*Command) Synopsis() string {
	return "display running steps of ninja build"
}

func (*Command) Usage() string {
	return `Display running steps of ninja build.

for local build
 $ siso ps [-C dir]

for buiders build
 $ siso ps --stdout_url <compile-step-stdout-URL>
`
}

// Command implements ps subcommand.
type Command struct {
	stdoutURL string
	dir       string
	stateDir  string
	n         int
	interval  time.Duration
	termui    bool
	loc       string
}

func (c *Command) SetFlags(flagSet *flag.FlagSet) {
	flagSet.StringVar(&c.stdoutURL, "stdout_url", "", "stdout streaming URL")
	flagSet.StringVar(&c.dir, "C", ".", "ninja running directory")
	flagSet.StringVar(&c.stateDir, "state_dir", ".", "state directory (relative to -C)")
	flagSet.IntVar(&c.n, "n", 0, "limit number of steps if it is positive")
	flagSet.DurationVar(&c.interval, "interval", -1, "query interval if it is positive. default 1s on terminal")
}

type source interface {
	location() string
	text() string
	fetch(context.Context) ([]build.ActiveStepInfo, error)
}

func (c *Command) Execute(ctx context.Context, flagSet *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	ctx, cancel := context.WithCancel(ctx)
	defer signals.HandleInterrupt(ctx, func() {
		cancel()
	})()

	u, ok := ui.Default.(*ui.TermUI)
	if ok {
		c.termui = true
		if c.n == 0 {
			c.n = u.Height() - 2
		}
		if c.interval < 0 {
			c.interval = 1 * time.Second
		}
	}

	var src source
	var err error
	if c.stdoutURL != "" {
		src, err = newStdoutURLSource(ctx, c.stdoutURL)
	} else {
		src, err = newLocalSource(c.dir, c.stateDir)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return subcommands.ExitFailure
	}
	c.loc = src.location()
	ret := subcommands.ExitSuccess
	for {
		activeSteps, err := src.fetch(ctx)
		if err != nil {
			if c.termui {
				fmt.Fprintf(os.Stderr, "\033[H\033[J%s\n", err)
			} else {
				fmt.Fprintf(os.Stderr, "%s\n", err)
			}
			ret = subcommands.ExitFailure
		} else {
			var lines []string
			if c.termui {
				// move to 0,0 and clear to the end of screen.
				lines = append(lines, fmt.Sprintf("\033[H\033[JSiso is running in %s", c.loc))
				lines = append(lines, fmt.Sprintf("%10s %9s %s", "DURATION", "PHASE", "DESC"))
			} else {
				lines = append(lines, "\f\n")
				lines = append(lines, fmt.Sprintf("%10s %9s %s\n", "DURATION", "PHASE", "DESC"))
			}
			c.render(lines, activeSteps)
		}
		if c.interval <= 0 {
			break
		}
		if errors.Is(err, io.EOF) {
			fmt.Println(src.text())
			c.termui = false
			ui.Default = ui.LogUI{}
			c.render(nil, activeSteps)
			return ret
		}
		select {
		case <-time.After(c.interval):
		case <-ctx.Done():
			return ret
		}
	}
	return ret
}

func (c *Command) render(lines []string, activeSteps []build.ActiveStepInfo) {
	headings := len(lines)
	for _, as := range activeSteps {
		dur := as.ServDur
		if dur == "" {
			dur = "(" + as.Dur + ")"
		}
		if c.termui {
			lines = append(lines, fmt.Sprintf("%10s %9s %s", dur, as.Phase, as.Desc))
		} else {
			lines = append(lines, fmt.Sprintf("%10s %9s %s\n", dur, as.Phase, as.Desc))
		}
		if c.n > 0 && len(lines) >= c.n {
			break
		}
	}
	lines = append(lines, fmt.Sprintf("steps=%d out of %d\n", len(lines)-headings, len(activeSteps)))
	ui.Default.PrintLines(lines...)
}
