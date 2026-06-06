// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build unix

package execute

import (
	"fmt"
	"syscall"
)

func exitCodeExplain(exitCode int) string {
	if exitCode <= 128 {
		return ""
	}
	return fmt.Sprintf(" # signal:%s", syscall.Signal(exitCode-128))
}
