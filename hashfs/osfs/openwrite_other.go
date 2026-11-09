// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build !windows

package osfs

import "os"

// Create is like os.Create. Removing an open file already works here.
func Create(name string) (*os.File, error) {
	return os.Create(name)
}
