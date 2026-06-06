// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build !linux && !darwin && !windows

package host

import "fmt"

func memoryTotal() (uint64, error) {
	return 0, fmt.Errorf("not implemented on this platform")
}
