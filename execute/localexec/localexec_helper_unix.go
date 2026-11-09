// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build unix

package localexec

import (
	"context"
	"fmt"
	"os"

	"go.chromium.org/build/siso/execute/spawnhelper"
)

// StartSpawnHelper launches the spawn helper, so the large-heap siso never fork()s. Call
// it once, early, while siso's heap is still small. logFile names the file the helper
// writes its diagnostics to; the caller rotates any previous one and the helper
// creates it fresh.
func StartSpawnHelper(ctx context.Context, helperCommand, logFile string, blockNetwork bool) error {
	args := []string{helperCommand}
	if helperCommand == "" {
		exe, err := os.Executable()
		if err != nil {
			return fmt.Errorf("spawn helper: %w", err)
		}
		args = []string{exe, "spawn-helper"}
	}

	c, err := spawnhelper.Launch(args, logFile, blockNetwork)
	if err != nil {
		return fmt.Errorf("spawn helper: %w", err)
	}
	SetSpawnHelper(c)
	return nil
}
