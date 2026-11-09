// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build !windows

package osfs

import "os"

// openRead opens name for reading. On POSIX, unlinking a file with an open
// handle already succeeds, so plain os.Open suffices; the Windows build adds
// FILE_SHARE_DELETE to match that behavior.
func openRead(name string) (*os.File, error) {
	return os.Open(name)
}
