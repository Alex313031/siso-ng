// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package hashfs

import (
	"testing"
)

// TestAcquireFlushStaticFallback covers the nil-gate path: acquireFlush
// must fall back to the static FlushSemaphore and expose the same
// release(error) shape as the adaptive gate. Releasing must return the
// slot so a subsequent acquire succeeds.
func TestAcquireFlushStaticFallback(t *testing.T) {
	ctx := t.Context()

	gotCtx, done, err := acquireFlush(ctx, nil)
	if err != nil {
		t.Fatalf("acquireFlush(nil gate): %v", err)
	}
	if gotCtx == nil {
		t.Fatal("acquireFlush returned nil ctx")
	}
	if done == nil {
		t.Fatal("acquireFlush returned nil release")
	}
	done(nil) // release the static semaphore slot; must not panic

	// The slot was returned, so another acquire/release round works.
	_, done2, err := acquireFlush(ctx, nil)
	if err != nil {
		t.Fatalf("second acquireFlush(nil gate): %v", err)
	}
	done2(nil)
}
