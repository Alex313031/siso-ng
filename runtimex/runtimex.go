// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package runtimex fixes the following API in standard runtime package.
// - NumCPU()
package runtimex

import "runtime"

var (
	ncpu int
)

func init() {
	ncpu = getproccount()
	if ncpu == 0 {
		ncpu = runtime.NumCPU()
	}
}

// NumCPU returns the number of logical CPUs usable by the current process.
// On Windows, runtime.NumCPU() only returns the information for a single Processor Group (up to 64).
// runtimex.NumCPU() uses GetActiveProcessorCount to get cpu counts from all Processor Groups.
// See the solution in kubernetes.
// https://github.com/kubernetes/kubernetes/blob/a4b8a3b2e33a3b591884f69b64f439e6b880dc40/pkg/kubelet/winstats/perfcounter_nodestats_windows.go#L205
// On non-Windows, runtime.NumCPU() is used as is.
func NumCPU() int {
	return ncpu
}
