// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package scandeps

import (
	"hash/maphash"
	"testing"
	"time"

	"go.chromium.org/build/siso/hashfs"
)

func TestFilesystemUpdate(t *testing.T) {
	dir := t.TempDir()
	ctx := t.Context()
	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}

	fsys := &filesystem{
		hashfs: hashFS,
		seed:   maphash.MakeSeed(),
	}
	hashFS.Notify(fsys.update)

	done := make(chan struct{})
	go func() {
		err := hashFS.Mkdir(ctx, dir, "out/siso/gen", nil, nil)
		if err != nil {
			t.Errorf("hashFS.Mkdir(ctx, %q, %q)=%v; want nil err", dir, "out/siso/gen", err)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatalf("too slow hashFS.Mkdir")
	}
}
