// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build windows

package lockfile

import (
	"errors"
	"fmt"
	"math"
	"os"

	"golang.org/x/sys/windows"
)

// LockFile represents an active lock on a file.
type LockFile struct {
	f       *os.File
	pidfile string
}

// New creates a new LockFile.
func New(fname string) (*LockFile, error) {
	f, err := os.OpenFile(fname, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return nil, err
	}
	return &LockFile{f: f, pidfile: fname + ".pid"}, nil
}

// Close releases the lock file.
func (l *LockFile) Close() error {
	return l.f.Close()
}

// Lock attempts to acquire an exclusive lock on the file.
// It returns ErrAlreadyLocked if the lock is already held.
func (l *LockFile) Lock() error {
	const reserved = 0
	const lowByteRange = math.MaxUint32
	const highByteRange = math.MaxUint32
	err := windows.LockFileEx(
		windows.Handle(l.f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		reserved, lowByteRange, highByteRange,
		&windows.Overlapped{})
	if err != nil {
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			// can't read lockfile when locked.
			buf, bufErr := os.ReadFile(l.pidfile)
			if bufErr != nil {
				err = bufErr
			}
			return &ErrAlreadyLocked{
				err:     err,
				bufErr:  bufErr,
				fname:   l.f.Name(),
				pidfile: l.pidfile,
				Owner:   string(buf),
			}
		}
		return err
	}
	return os.WriteFile(l.pidfile, []byte(fmt.Sprintf("pid=%d", os.Getpid())), 0644)
}

// Unlock releases the lock.
func (l *LockFile) Unlock() error {
	const reserved = 0
	const lowByteRange = math.MaxUint32
	const highByteRange = math.MaxUint32
	return windows.UnlockFileEx(
		windows.Handle(l.f.Fd()),
		reserved, lowByteRange, highByteRange,
		&windows.Overlapped{})
}
