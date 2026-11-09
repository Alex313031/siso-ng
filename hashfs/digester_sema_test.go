// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package hashfs_test

import (
	"runtime"
	"testing"

	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/toolsupport/abfsutil"
)

// initGOMAXPROCS is GOMAXPROCS at package initialization, when the local
// digest semaphore is sized. go test -cpu changes GOMAXPROCS later.
var initGOMAXPROCS = runtime.GOMAXPROCS(0)

func TestDigestSemaphore(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	abfs, err := abfsutil.New(ctx, "unix://abfs.sock", dir)
	if err != nil {
		t.Fatalf("abfsutil.New: %v", err)
	}
	for _, tc := range []struct {
		name string
		opt  hashfs.Option
		want int
	}{
		{name: "local", want: initGOMAXPROCS},
		{name: "abfs", opt: hashfs.Option{ABFS: abfs}, want: min(16, initGOMAXPROCS)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hfs, err := hashfs.New(ctx, tc.opt)
			if err != nil {
				t.Fatalf("hashfs.New: %v", err)
			}
			defer func() {
				if err := hfs.Close(ctx); err != nil {
					t.Errorf("hfs.Close: %v", err)
				}
			}()
			if got := hfs.DigestSemaphore().Capacity(); got != tc.want {
				t.Errorf("DigestSemaphore().Capacity() = %d; want %d", got, tc.want)
			}
		})
	}
}
