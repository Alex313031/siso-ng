// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build windows

package host

import (
	"fmt"

	"golang.org/x/sys/windows"
)

func osVersion() (string, error) {
	osvi := windows.RtlGetVersion()

	// Format version as Major.Minor.Build (e.g., 10.0.22631)
	// NOTE: Be aware that Windows 11 reports Major=10, but Build >= 22000
	return fmt.Sprintf("%d.%d.%d", osvi.MajorVersion, osvi.MinorVersion, osvi.BuildNumber), nil
}
