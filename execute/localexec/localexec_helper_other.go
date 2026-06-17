// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build !unix

package localexec

import (
	"context"

	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/execute"
)

// runViaHelper has no use on non-unix platforms: we just spawn commands directly.
func runViaHelper(ctx context.Context, cmd *execute.Cmd) (*rpb.ActionResult, error) {
	return run(ctx, cmd)
}

// StartHelper is a no-op on non-unix platforms: there is no spawn helper to start.
func StartHelper(ctx context.Context, logFile string) error { return nil }
