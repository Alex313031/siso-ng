// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build windows

package execute

import (
	"fmt"

	"golang.org/x/sys/windows"
)

func exitCodeExplain(exitCode int) string {
	// https://learn.microsoft.com/en-us/windows/win32/api/processthreadsapi/nf-processthreadsapi-getexitcodeprocess
	// large exit code would be the exception value for an unhandled
	// exception, e.g. STATUS_ACCESS_VIOLATION.
	// https://cs.opensource.google/go/go/+/refs/tags/go1.26.0:src/os/exec_posix.go;l=134
	if uint(exitCode) < 1<<16 {
		return ""
	}
	return fmt.Sprintf(" # %v", windows.NTStatus(uint32(exitCode)))
}
