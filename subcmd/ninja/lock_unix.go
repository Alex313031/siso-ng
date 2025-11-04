// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build unix

package ninja

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

type lockFile struct {
	f *os.File
}

func newLockFile(ctx context.Context, fname string) (*lockFile, error) {
	f, err := os.OpenFile(fname, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return nil, err
	}
	return &lockFile{f: f}, nil
}

func (l *lockFile) Close() error {
	return l.f.Close()
}

func (l *lockFile) Lock() error {
	err := unix.Flock(int(l.f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			_, _ = l.f.Seek(0, io.SeekStart)
			buf, bufErr := io.ReadAll(l.f)
			return &errAlreadyLocked{
				err:     err,
				bufErr:  bufErr,
				fname:   l.f.Name(),
				pidfile: "",
				owner:   string(buf),
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

func (l *lockFile) Unlock() error {
	return unix.Flock(int(l.f.Fd()), unix.LOCK_UN)
}
