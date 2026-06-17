// Copyright 2025 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build linux

package localexec

import (
	"context"
	"fmt"
	"os"
	"strconv"

	"go.chromium.org/build/siso/o11y/clog"
)

func oomScoreAdj(ctx context.Context, pid int, score int) {
	err := os.WriteFile(fmt.Sprintf("/proc/%d/oom_score_adj", pid), strconv.AppendInt(nil, int64(score), 10), 0644)
	if err != nil {
		clog.Warningf(ctx, "failed to set %d/oom_score_adj %d: %v", pid, score, err)
	}
}
