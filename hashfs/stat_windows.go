// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package hashfs

import "io/fs"

func isHardlink(fi fs.FileInfo) bool {
	return false
}
