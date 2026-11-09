// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import "strings"

// IsDirTarget reports whether path is a directory artifact (trailing slash).
func IsDirTarget(path string) bool {
	return strings.HasSuffix(path, "/")
}

// DirTargetPath returns path with any trailing slash removed.
func DirTargetPath(path string) string {
	return strings.TrimSuffix(path, "/")
}
