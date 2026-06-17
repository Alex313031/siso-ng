// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build unix && !linux

package localexec

// becomeSubreaper is a no-op outside Linux (no PR_SET_CHILD_SUBREAPER
// equivalent): orphaned descendants reparent to init/launchd, which reaps them,
// so kill(-pgid) still reaches ESRCH and drainGroup terminates.
func becomeSubreaper() error {
	return nil
}
