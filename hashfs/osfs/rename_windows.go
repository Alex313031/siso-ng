// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build windows

package osfs

import (
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// rename replaces newpath with POSIX semantics, so it succeeds even while a
// share-delete handle holds newpath open. Plain os.Rename (MoveFileEx) fails
// there with ERROR_ACCESS_DENIED. Needs Win10 1709+ NTFS/ReFS.
func rename(oldpath, newpath string) error {
	src, err := openForRename(oldpath)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(src)

	target, err := renameTarget(newpath)
	if err != nil {
		return err
	}
	name, err := windows.UTF16FromString(target)
	if err != nil {
		return err
	}
	name = name[:len(name)-1] // exclude the NUL from FileNameLength

	// Keep the trailing NUL: this path reads FileName NUL-terminated, past
	// FileNameLength, so an unterminated buffer picks up adjacent garbage.
	const headerLen = unsafe.Offsetof(fileRenameInfo{}.FileName)
	buf := make([]byte, int(headerLen)+(len(name)+1)*2)
	info := (*fileRenameInfo)(unsafe.Pointer(&buf[0]))
	info.Flags = windows.FILE_RENAME_REPLACE_IF_EXISTS | windows.FILE_RENAME_POSIX_SEMANTICS
	info.FileNameLength = uint32(len(name) * 2)
	copy(unsafe.Slice((*uint16)(unsafe.Pointer(&buf[headerLen])), len(name)), name)

	return windows.SetFileInformationByHandle(src, windows.FileRenameInfoEx, &buf[0], uint32(len(buf)))
}

// fileRenameInfo is FILE_RENAME_INFO for the FileRenameInfoEx class; FileName is
// a variable-length trailer the buffer extends past.
type fileRenameInfo struct {
	Flags          uint32
	_              uint32 // pad: RootDirectory must be 8-byte aligned
	RootDirectory  windows.Handle
	FileNameLength uint32
	FileName       [1]uint16
}

// openForRename opens name for the rename with DELETE access, full sharing,
// BACKUP_SEMANTICS (dirs) and OPEN_REPARSE_POINT (symlink itself), like os.Rename.
func openForRename(name string) (windows.Handle, error) {
	p, err := createFilePath(name)
	if err != nil {
		return windows.InvalidHandle, &os.PathError{Op: "rename", Path: name, Err: err}
	}
	h, err := windows.CreateFile(
		p,
		windows.DELETE|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return windows.InvalidHandle, &os.PathError{Op: "rename", Path: name, Err: err}
	}
	return h, nil
}

// renameTarget makes an NT-namespace path (\??\C:\...): FILE_RENAME_INFO with a
// NULL RootDirectory rejects a bare Win32 path.
func renameTarget(name string) (string, error) {
	// hashfs paths use forward slashes (the flush destination reaches here as
	// C:/dir/file). The NT object namespace only accepts backslashes, so
	// normalize before resolving and prefixing.
	full, err := windows.FullPath(filepath.FromSlash(name))
	if err != nil {
		return "", err
	}
	full = filepath.FromSlash(full)
	switch {
	case strings.HasPrefix(full, `\\?\`), strings.HasPrefix(full, `\??\`):
		// Already an extended or NT path; reuse as-is.
	case strings.HasPrefix(full, `\\`):
		full = `\??\UNC\` + full[2:] // \\server\share -> \??\UNC\server\share
	default:
		full = `\??\` + full
	}
	return full, nil
}
