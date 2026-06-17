// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ui

import (
	"fmt"
	"strings"
	"time"
)

// DurationThreshold is a threshold to show event time only if
// duration is higher than this threshold. b/294443556
const DurationThreshold = 1 * time.Second

// FormatDuration formats duration in "X.XXs", "XmXX.XXs" or "XhXmXX.XXs".
func FormatDuration(d time.Duration) string {
	d = d.Round(10 * time.Millisecond)
	var sb strings.Builder
	sb.Grow(32)

	mins := d.Truncate(1 * time.Minute)
	d = d - mins
	if mins > 0 {
		fmt.Fprintf(&sb, "%s", strings.TrimSuffix(mins.String(), "0s"))
		if d < 10*time.Second {
			fmt.Fprint(&sb, "0")
		}
	}
	fmt.Fprintf(&sb, "%02.02fs", d.Seconds())
	return sb.String()
}
