// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build !windows

package host

import "github.com/klauspost/cpuid/v2"

// cpuCores returns the number of physical and logical CPU cores on the machine,
// independent of any cgroup, processor-group, or CPU affinity restrictions on the
// current process.
// On non-Windows platforms, fall back to the default cpuid implementation.
// TODO(b/527352649): Might be wrong on Linux?
func cpuCores() (int, int, error) {
	return cpuid.CPU.PhysicalCores, cpuid.CPU.LogicalCores, nil
}
