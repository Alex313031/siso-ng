// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package fscmd

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.chromium.org/build/hashigo/digest"
	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/hashfs"
	pb "go.chromium.org/build/siso/hashfs/proto"
)

// TestDiffDigestFunction verifies that fs diff interprets each state file
// under the digest function it was recorded with: a blake3 state diffed
// against an empty legacy (sha256) base must report its entries as new
// instead of silently discarding them via the SHA-256 mismatch guard.
func TestDiffDigestFunction(t *testing.T) {
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

	// An empty legacy base state (no digest function recorded).
	baseOpts := hashfs.Option{
		StateFile:     filepath.Join(dir, ".siso_fs_state.0"),
		CompressLevel: 1,
	}
	if err := hashfs.Save(ctx, &pb.State{}, baseOpts); err != nil {
		t.Fatalf("Save(base)=%v; want nil", err)
	}

	c := &diffCommand{
		stateFile:     opts.StateFile,
		stateFileBase: baseOpts.StateFile,
	}
	var buf bytes.Buffer
	if err := c.run(ctx, &buf); err != nil {
		t.Fatalf("run(...)=%v; want nil", err)
	}
	stampName := filepath.ToSlash(filepath.Join(dir, "stamp"))
	if out := buf.String(); !strings.Contains(out, stampName) || !strings.Contains(out, `"new"`) {
		t.Errorf("diff output does not report %q as new; blake3 state discarded?\noutput: %s", stampName, out)
	}
}
