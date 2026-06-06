// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build windows

package ninja

import (
	"context"

	"go.chromium.org/build/siso/build"
)

func (c *Command) checkResourceLimits(ctx context.Context, limits build.Limits) {
}
