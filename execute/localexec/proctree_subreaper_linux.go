// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build linux

package localexec

import "golang.org/x/sys/unix"

// becomeSubreaper marks the helper as a child subreaper so orphaned action
// descendants reparent to it for reaping via wait4(-pgid) (see the package
// comment in proctree_unix.go). Idempotent; call once.
func becomeSubreaper() error {
	return unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0)
}
