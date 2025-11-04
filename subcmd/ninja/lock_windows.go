// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build windows

package ninja

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"

	"golang.org/x/sys/windows"
)

type lockFile struct {
	f       *os.File
	pidfile string
}

func newLockFile(ctx context.Context, fname string) (*lockFile, error) {
	f, err := os.OpenFile(fname, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return nil, err
	}
	return &lockFile{f: f, pidfile: fname + ".pid"}, nil
}

func (l *lockFile) Close() error {
	return l.f.Close()
}

func (l *lockFile) Lock() error {
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
			return &errAlreadyLocked{
				err:     err,
				bufErr:  bufErr,
				fname:   l.f.Name(),
				pidfile: l.pidfile,
				owner:   string(buf),
			}
		}
		return err
	}
	return os.WriteFile(l.pidfile, []byte(fmt.Sprintf("pid=%d", os.Getpid())), 0644)
}

func (l *lockFile) Unlock() error {
	const reserved = 0
	const lowByteRange = math.MaxUint32
	const highByteRange = math.MaxUint32
	return windows.UnlockFileEx(
		windows.Handle(l.f.Fd()),
		reserved, lowByteRange, highByteRange,
		&windows.Overlapped{})
}
