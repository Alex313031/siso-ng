// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package main

import (
	"strings"
	"testing"

	"github.com/google/subcommands"

	"go.chromium.org/build/siso/subcmd/fscmd"
	"go.chromium.org/build/siso/subcmd/metricscmd"
	"go.chromium.org/build/siso/subcmd/query"
)

func TestSubcommandsUsageEndsWithNewline(t *testing.T) {
	var allCmds []subcommands.Command

	for _, entry := range subcommandsList(nil) {
		allCmds = append(allCmds, entry.cmd)
	}
	allCmds = append(allCmds, fscmd.Subcommands(nil)...)
	allCmds = append(allCmds, metricscmd.Subcommands()...)
	allCmds = append(allCmds, query.Subcommands()...)

	for _, cmd := range allCmds {
		name := cmd.Name()
		t.Run(name, func(t *testing.T) {
			if name == "" {
				t.Errorf("command name should not be empty")
			}
			usage := cmd.Usage()
			if !strings.HasSuffix(usage, "\n") {
				t.Errorf("%q Usage() does not end with newline: %q", name, usage)
			}
		})
	}
}
