// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package lockfile

import (
	"errors"
	"fmt"
)

// ErrAlreadyLocked indicates that the lock is already held.
type ErrAlreadyLocked struct {
	err     error
	bufErr  error
	fname   string
	pidfile string
	// Owner holds information about the current lock holder, if available.
	Owner string
}

// Error implements the Error interface.
func (e *ErrAlreadyLocked) Error() string {
	msg := fmt.Sprintf("lock %s: already locked", e.fname)
	if e.Owner != "" {
		msg = fmt.Sprintf("%s by %s", msg, e.Owner)
	}
	if e.pidfile != "" {
		if e.bufErr != nil {
			msg = fmt.Sprintf("%s: failed to read pidfile %s: %v", msg, e.pidfile, e.bufErr)
		}
	}
	return msg
}

// Unwrap returns the underlying error.
func (e *ErrAlreadyLocked) Unwrap() error {
	return e.err
}

// Is checks if the target error is the same as the underlying error.
func (e *ErrAlreadyLocked) Is(target error) bool {
	return errors.Is(e.err, target)
}
