// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build !unix

package spawnhelper

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/google/subcommands"
)

func (c *Command) Execute(_ context.Context, _ *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	fmt.Fprintln(os.Stderr, "spawn-helper: not supported on this platform")
	return subcommands.ExitFailure
}
