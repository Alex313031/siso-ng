// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package metadata

import "time"

// InvocationInfo represents info logged about a ninja build invocation.
// It is intended to be written at the start of the build, and therefore does not
// provide information about whether the build succeeded or not.
type InvocationInfo struct {
	// SisoVersion is the SemVer of siso.
	SisoVersion string `json:"siso_version"`
	// StartTime is the time that the ninja build started.
	StartTime time.Time `json:"start_time"`
	// BuildID is the Ninja build ID used for analytics and identification.
	BuildID string `json:"build_id"`
	// Targets of the build.
	Targets []string `json:"targets,omitempty"`
	// MetricsLabels are arbitrary labels for the build.
	// These can include RBE metrics labels, as well as other user-defined labels.
	MetricsLabels map[string]string `json:"metrics_labels,omitempty"`
	// Machine reports machine information.
	Machine MachineInfo `json:"machine"`
}
