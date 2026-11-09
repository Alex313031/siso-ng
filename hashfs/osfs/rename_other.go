// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build !windows

package osfs

import "os"

// rename is os.Rename; POSIX rename already replaces an open destination.
func rename(oldpath, newpath string) error {
	return os.Rename(oldpath, newpath)
}
