// Copyright 2025 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package version

import (
	"fmt"
	"runtime/debug"
	"strings"
	"sync"

	"go.chromium.org/build/siso/toolsupport/cipdutil"
)

// Version contains version info.
type Version struct {
	CIPD  *cipdutil.VersionInfo
	Build *debug.BuildInfo
}

var (
	once       sync.Once
	currentVer Version
	currentErr error
)

// Current returns current version info.
func Current() (Version, error) {
	once.Do(func() {
		cipdver, err := cipdutil.StartupVersion()
		if err != nil {
			currentErr = fmt.Errorf("cannot determine CIPD package version: %w", err)
		}
		buildInfo, ok := debug.ReadBuildInfo()
		if !ok && err == nil {
			currentErr = fmt.Errorf("cannot read go build info")
		}
		if cipdver.InstanceID != "" {
			currentVer.CIPD = &cipdver
		}
		currentVer.Build = buildInfo
	})
	return currentVer, currentErr
}

// ToolName returns tool's name.
func (v Version) ToolName() string {
	if v.CIPD != nil {
		return v.CIPD.PackageName
	}
	if v.Build != nil {
		return "siso " + v.Build.Main.Path
	}
	return "siso"
}

// ToolVersion returns tool's version.
func (v Version) ToolVersion() string {
	if v.CIPD != nil {
		return v.CIPD.InstanceID
	}
	if v.Build != nil {
		return v.Build.Main.Version
	}
	return "unknown"
}

func (v Version) BuildSettings() map[string]string {
	bs := make(map[string]string)
	if v.Build == nil {
		return bs
	}
	for _, s := range v.Build.Settings {
		if strings.HasPrefix(s.Key, "vcs.") || strings.HasPrefix(s.Key, "-") {
			bs[s.Key] = s.Value
		}
	}
	return bs
}
