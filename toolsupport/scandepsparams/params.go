// Copyright 2025 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package scandepsparams

// ScanDepsParams holds parameters used for scandeps.
type ScanDepsParams struct {
	// Sources are source files.
	Sources []string

	// Includes are include files specified by -include or /FI.
	Includes []string

	// Files are input files, such as sanitizer ignore list.
	Files []string

	// Dirs are include directories.
	Dirs []string

	// QuoteDirs are include directories specified by -iquote.
	QuoteDirs []string

	// Frameworks are framework directories.
	Frameworks []string

	// Sysroots are sysroot directories and toolchain root directories.
	Sysroots []string

	// Defines are defined macros.
	Defines map[string]string
}
