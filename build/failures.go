// Copyright 2025 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"errors"
)

// failures manages number of failures.
type failures struct {
	allowed  int
	n        int
	firstErr error
}

func (f *failures) shouldFail(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	if f.firstErr == nil {
		f.firstErr = err
	}
	f.n++
	return f.n >= f.allowed
}
