// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build !linux

package trace

import (
	"context"
	"time"
)

// TODO: add system resource metrics for non linux.
type sysRecord struct {
	start time.Time
}

func (*sysRecord) get(ctx context.Context) {}

func (*sysRecord) sample(context.Context, int64, time.Time) []Event {
	return nil
}
