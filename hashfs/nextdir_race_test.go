// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package hashfs

import (
	"io/fs"
	"sync"
	"testing"
)

func TestNextDirErrNotExistRace(t *testing.T) {
	ctx := t.Context()

	// Run enough iterations to reliably trigger the race condition
	for i := range 10000 {
		hfs, err := New(ctx, Option{})
		if err != nil {
			t.Fatal(err)
		}

		var wg sync.WaitGroup
		wg.Add(2)

		// Goroutine A: Simulates filegroup A storing ErrNotExist for "sysroot".
		go func() {
			defer wg.Done()
			errEntry := newLocalEntry()
			errEntry.err = fs.ErrNotExist
			hfs.directory.store(ctx, "sysroot", errEntry)
		}()

		// Goroutine B: Simulates filegroup B lazily creating "sysroot" as a directory
		// to store a nested file.
		var storeErr error
		go func() {
			defer wg.Done()
			nestedEntry := newLocalEntry()
			nestedEntry.err = fs.ErrNotExist
			_, storeErr = hfs.directory.store(ctx, "sysroot/usr/lib", nestedEntry)
		}()

		wg.Wait()

		if storeErr != nil {
			t.Fatalf("Race triggered on iteration %d: %v", i, storeErr)
		}
	}
}
