// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package shutil

import "strings"

// Join joins a command line args to a single string.
func Join(args []string) string {
	// TODO(ukai): Use appropriate quote for arg. This would make it easier to
	// debug, or rerun the same command. (Compared than callers directly using
	// strings.Join instead of this function)
	return strings.Join(args, " ")
}
