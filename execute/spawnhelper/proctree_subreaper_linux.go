// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build linux

package spawnhelper

import "golang.org/x/sys/unix"

// becomeSubreaper marks the helper as a child subreaper (Linux
// PR_SET_CHILD_SUBREAPER), so descendants orphaned mid-run reparent to the
// helper instead of to init. The helper can then reap it with
// wait4(-pgid) when it drains a process group. Idempotent; call once.
func becomeSubreaper() error {
	return unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0)
}
