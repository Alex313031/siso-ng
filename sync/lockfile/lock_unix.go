// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build unix

package lockfile

import (
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// LockFile represents an active lock on a file.
type LockFile struct {
	f *os.File
}

// New creates a new LockFile.
func New(fname string) (*LockFile, error) {
	f, err := os.OpenFile(fname, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return nil, err
	}
	return &LockFile{f: f}, nil
}

// Close releases the lock file.
func (l *LockFile) Close() error {
	return l.f.Close()
}

// Lock attempts to acquire an exclusive lock on the file.
// It returns ErrAlreadyLocked if the lock is already held.
func (l *LockFile) Lock() error {
	err := unix.Flock(int(l.f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			_, _ = l.f.Seek(0, io.SeekStart)
			buf, bufErr := io.ReadAll(l.f)
			return &ErrAlreadyLocked{
				err:     err,
				bufErr:  bufErr,
				fname:   l.f.Name(),
				pidfile: "",
				Owner:   string(buf),
			}
		}
		return err
	}
	if err = l.f.Truncate(0); err != nil {
		return err
	}
	if _, err = l.f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	fmt.Fprintf(l.f, "pid=%d", os.Getpid())
	return nil
}

// Unlock releases the lock.
func (l *LockFile) Unlock() error {
	return unix.Flock(int(l.f.Fd()), unix.LOCK_UN)
}
