// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build unix && !linux

package spawnhelper

import (
	"fmt"
	"os/exec"
)

func setNetworkPolicy(cmd *exec.Cmd, blockNetwork bool) error {
	if blockNetwork {
		return fmt.Errorf("blockNetwork is only supported on Linux")
	}
	return nil
}

func ifaceUp(ifname string) error {
	return nil
}
