// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package host provides os-specific host information functions.
package host

import "errors"

var ErrUnsupportedOS = errors.New("unsupported os")

// MemoryTotal returns the total amount of physical memory on the machine in bytes.
func MemoryTotal() (uint64, error) {
	return memoryTotal()
}

// OSVersion returns the operating system version, or any other meaningful versioning identifier of the running system.
func OSVersion() (string, error) {
	return osVersion()
}
