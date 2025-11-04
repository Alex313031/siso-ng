// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build windows

package build

import (
	"time"
)

type usageRecord struct {
	start time.Time
}

func (u *usageRecord) get() {
}

func (u *usageRecord) sample(time.Time) []traceEventObject {
	// TODO(b/273653666): resource usage collection on windows
	return nil
}
