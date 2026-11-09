// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package sandbox

import (
	"path/filepath"
	"testing"
	"time"

	"go.chromium.org/build/hashigo/digest"
	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/hashfs"
)

// TestNewHashFSForState verifies the persisted state of a non-sha256 build is
// interpreted under the digest function it was recorded with, instead of
// being silently discarded by the SHA-256 mismatch guard.
func TestNewHashFSForState(t *testing.T) {
	ctx := t.Context()

	dir := t.TempDir()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	blake3, err := digest.Lookup(rpb.DigestFunction_BLAKE3)
	if err != nil {
		t.Fatal(err)
	}
	opts := hashfs.Option{
		StateFile:      filepath.Join(dir, ".siso_fs_state"),
		CompressLevel:  1,
		DigestFunction: blake3,
	}

	// Persist a state the way a blake3 build would.
	hashFS, err := hashfs.New(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := hashFS.WaitReady(ctx); err != nil {
		t.Fatalf("WaitReady=%v; want nil", err)
	}
	if err := hashFS.WriteFile(ctx, dir, "stamp", nil, false, time.Now(), []byte("dummy-cmdhash"), nil); err != nil {
		t.Fatalf("WriteFile(...)=%v; want nil", err)
	}
	st := hashFS.State(ctx)
	hashFS.Close(ctx)
	if err := hashfs.Save(ctx, st, opts); err != nil {
		t.Fatalf("Save(...)=%v; want nil", err)
	}

	// Read it back the way the subcommand does.
	loaded, err := hashfs.Load(ctx, hashfs.Option{StateFile: opts.StateFile})
	if err != nil {
		t.Fatalf("Load(...)=%v; want nil", err)
	}
	hfs, err := hashfs.NewFromState(ctx, loaded, hashfs.Option{})
	if err != nil {
		t.Fatalf("hashfs.NewFromState(...)=%v; want nil", err)
	}
	defer hfs.Close(ctx)
	if err := hfs.WaitReady(ctx); err != nil {
		t.Fatalf("WaitReady=%v; want nil", err)
	}

	stampName := filepath.ToSlash(filepath.Join(dir, "stamp"))
	if _, ok := hashfs.StateMap(blake3, hfs.State(ctx))[stampName]; !ok {
		t.Errorf("newHashFSForState discarded the blake3 state; want %q kept", stampName)
	}
}
