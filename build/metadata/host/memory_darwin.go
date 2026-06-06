// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build darwin

package host

import (
	"golang.org/x/sys/unix"
)

func memoryTotal() (uint64, error) {
	val, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0, err
	}
	return val, nil
}
