// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package metadata

import (
	"context"
	"errors"
	"runtime"

	"github.com/klauspost/cpuid/v2"

	"go.chromium.org/build/siso/build/metadata/host"
	"go.chromium.org/build/siso/o11y/clog"
)

// MachineInfo represents information about the machine that the build was invoked on.
type MachineInfo struct {
	// Platform reports platform information of the machine that the build was invoked on.
	Platform PlatformInfo `json:"platform"`
	// CPU reports CPU information, e.g. brand name and vendor string.
	CPU CPUInfo `json:"cpu"`
	// Memory reports memory information.
	Memory MemoryInfo `json:"memory"`
}

// GatherMachineInfo gathers and returns machine information of the build environment.
func GatherMachineInfo(ctx context.Context) MachineInfo {
	total, err := host.MemoryTotal()
	if err != nil {
		clog.Warningf(ctx, "failed to get machine memory: %v", err)
	}
	osVersion, err := host.OSVersion()
	if errors.Is(err, host.ErrUnsupportedOS) {
		osVersion = ""
	} else {
		clog.Warningf(ctx, "failed to get os version: %v", err)
	}
	return MachineInfo{
		Platform: PlatformInfo{
			Architecture: runtime.GOARCH,
			OS:           runtime.GOOS,
			OSVersion:    osVersion,
		},
		CPU: CPUInfo{
			BrandName:     cpuid.CPU.BrandName,
			VendorString:  cpuid.CPU.VendorString,
			LogicalCores:  cpuid.CPU.LogicalCores,
			PhysicalCores: cpuid.CPU.PhysicalCores,
		},
		Memory: MemoryInfo{
			Total: total,
		},
	}
}

// PlatformInfo reports platform information of the machine that the build was invoked on.
type PlatformInfo struct {
	// Architecture is the host's architecture (uses standard GOARCH values).
	Architecture string `json:"architecture"`

	// OS is the host's operating system (uses standard GOOS values).
	OS string `json:"os"`

	// OSVersion is the version of the running system.
	// For macOS and Windows, this is the OS version. For Linux, it's the kernel version.
	OSVersion string `json:"os_version,omitempty"`
}

// CPUInfo reports CPU information.
type CPUInfo struct {
	// BrandName is the brand name reported by the CPU, e.g. "Intel(R) Xeon(R) CPU @ 2.20GHz".
	BrandName string `json:"brand"`
	// VendorString is the raw vendor string reported by the CPU, e.g. "GenuineIntel".
	VendorString string `json:"vendor"`
	// LogicalCores is the number of logical cores usable by the current process.
	LogicalCores int `json:"logical_cores"`
	// PhysicalCores is the number of physical cores on the machine.
	PhysicalCores int `json:"physical_cores"`
}

// MemoryInfo reports memory information.
type MemoryInfo struct {
	// Total is the total amount of memory on the machine in bytes.
	Total uint64 `json:"total,omitempty"`
}
