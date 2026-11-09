// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build !unix

package localexec

import "context"

// StartSpawnHelper is a no-op on non-unix platforms: there is no spawn helper to start.
func StartSpawnHelper(ctx context.Context, helperCommand, logFile string, blockNetwork bool) error {
	return nil
}
