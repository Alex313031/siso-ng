// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package e2etests

import (
	"context"
	"flag"
	"os"
	"testing"

	"go.chromium.org/build/siso/subcmd/spawnhelper"
)

// TestMain lets the test binary act as the spawn helper when re-exec'd by
// localexec.StartHelper (tests that drive ninja.Command.Run start one): under
// `go test`, os.Executable() is this test binary. Without the dispatch, the
// re-exec would run the whole test suite recursively (fork bomb).
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "spawn-helper" {
		c := spawnhelper.Cmd()
		fs := flag.NewFlagSet("spawn-helper", flag.ExitOnError)
		c.SetFlags(fs)
		_ = fs.Parse(os.Args[2:]) // ExitOnError: exits on parse failure
		os.Exit(int(c.Execute(context.Background(), fs)))
	}
	m.Run()
}
