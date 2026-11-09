// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build windows

package osfs

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// openRead opens name for reading with FILE_SHARE_DELETE in addition to the
// share modes os.Open uses. Without FILE_SHARE_DELETE, a concurrent read (e.g.
// an in-flight digest computation) blocks a RemoveAll of the same path with a
// sharing violation; with it, the delete is accepted and completes once the
// last handle closes. POSIX unlink-while-open already behaves this way.
func openRead(name string) (*os.File, error) {
	p, err := createFilePath(name)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: name, Err: err}
	}
	h, err := windows.CreateFile(
		p,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: name, Err: err}
	}
	return os.NewFile(uintptr(h), name), nil
}

// createFilePath converts name to the UTF-16 argument for CreateFile.
func createFilePath(name string) (*uint16, error) {
	p, err := extendedLengthPath(name)
	if err != nil {
		return nil, err
	}
	return windows.UTF16PtrFromString(p)
}

// extendedLengthPath reproduces os.Open's fixLongPath handling, which this code
// bypasses by calling CreateFile directly: for a path at or beyond the
// MAX_PATH-derived limit it prepends the \\?\ extended-length prefix, without
// which CreateFile cannot open deep paths on a Windows host that lacks global
// long-path support.
//
// The length that matters is the resolved absolute length: a relative name
// (report's WalkDir-relative FileSource names reach here) can be short yet
// resolve past MAX_PATH from a deep working directory, exactly as os.Open's
// fixLongPath accounts for. So only a short *absolute* path takes the fast path
// unchanged; anything else is resolved with GetFullPathName (which also
// converts the forward slashes hashfs paths use to the backslashes an extended
// path requires) before the length is judged.
func extendedLengthPath(name string) (string, error) {
	if filepath.IsAbs(name) && len(name) < 248 {
		return name, nil
	}
	full, err := windows.FullPath(name)
	if err != nil {
		return "", err
	}
	if len(full) < 248 {
		// Short once resolved; CreateFile opens the original name fine.
		return name, nil
	}
	switch {
	case strings.HasPrefix(full, `\\?\`):
		// Already extended.
	case strings.HasPrefix(full, `\\.\`):
		// Device path; the extended prefix would change its meaning.
	case strings.HasPrefix(full, `\\`):
		full = `\\?\UNC\` + full[2:] // \\server\share -> \\?\UNC\server\share
	default:
		full = `\\?\` + full
	}
	return full, nil
}
