// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build unix

package cmdutil

import (
	"fmt"
	"runtime"
)

// Split splits cmd.exe's cmdline. Windows only.
func Split(cmdline string) ([]string, error) {
	return nil, fmt.Errorf("cmdutil.Split is not supported on %s", runtime.GOOS)
}
