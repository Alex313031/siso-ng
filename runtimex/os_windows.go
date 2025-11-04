// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build windows

package runtimex

import (
	"golang.org/x/sys/windows"
)

const allProcessorGroups = 0xFFFF

func getproccount() int {
	return int(windows.GetActiveProcessorCount(allProcessorGroups))
}
