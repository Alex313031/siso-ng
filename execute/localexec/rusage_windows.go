// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build windows

package localexec

import (
	"os/exec"

	epb "go.chromium.org/build/siso/execute/proto"
)

func rusage(cmd *exec.Cmd) *epb.Rusage {
	return nil
}
