// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjautil

import "fmt"

// UnknownTargetError is returned when querying for unknown ninja target.
type UnknownTargetError struct {
	name string
}

// Error implements the Error interface.
func (e UnknownTargetError) Error() string {
	return fmt.Sprintf("unknown target %q", e.name)
}
