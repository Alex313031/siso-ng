// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build !linux

package build

import (
	"context"
	"time"
)

// TODO: add system resource metrics for non linux.
type sysRecord struct {
	start time.Time
}

func (*sysRecord) get(ctx context.Context) {}

func (*sysRecord) sample(context.Context, time.Time) []traceEventObject {
	return nil
}
