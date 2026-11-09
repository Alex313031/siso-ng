// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package hashfs_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"go.chromium.org/build/hashigo/digest"

	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/path"
	"go.chromium.org/build/siso/reapi/merkletree"
)

// remoteDirOutputName is the directory output recorded by setupRemoteDirOutput.
const remoteDirOutputName = "out/siso/gen"

// setupRemoteDirOutput records remoteDirOutputName as a directory output that
// is never on disk (IsLocal defaults to false), with the given action digest,
// and returns the hashfs and its root.
func setupRemoteDirOutput(ctx context.Context, t *testing.T, action digest.Digest) (*hashfs.HashFS, string) {
	t.Helper()
	dir := t.TempDir()
	hfs, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := hfs.Close(ctx); err != nil {
			t.Errorf("hfs.Close=%v", err)
		}
	})
	if err := hfs.Update(ctx, dir, []hashfs.UpdateEntry{{
		Name:    remoteDirOutputName,
		Entry:   &merkletree.Entry{Name: remoteDirOutputName},
		Mode:    0o755 | fs.ModeDir,
		CmdHash: []byte("dircmdhash"),
		Action:  action,
	}}); err != nil {
		t.Fatalf("Update(%q): %v", remoteDirOutputName, err)
	}
	return hfs, dir
}

// TestStat_BuildWithoutBytesDirOutput locks in the build-without-bytes contract
// for directory outputs. A remote-only dir output (recorded from an action:
// cmdhash + action digest, IsLocal=false) lives in CAS, not on disk, so Stat
// reports it present even when absent locally.
// TestStat_DirOutputWithoutActionDigest covers the other side of this
// condition.
func TestStat_BuildWithoutBytesDirOutput(t *testing.T) {
	ctx := t.Context()
	hfs, dir := setupRemoteDirOutput(ctx, t, digest.Digest{Hash: "actionhash", SizeBytes: 10})
	fi, err := hfs.Stat(ctx, dir, remoteDirOutputName)
	if err != nil {
		t.Fatalf("Stat(%q)=_, %v; want nil err (build-without-bytes dir output is present in CAS)", remoteDirOutputName, err)
	}
	if !fi.IsDir() {
		t.Errorf("Stat(%q).IsDir()=false; want true", remoteDirOutputName)
	}
}

// TestStat_DirOutputWithoutActionDigest verifies that a directory recorded
// without an action digest is not CAS-backed, so Stat reports it missing when
// absent on disk. This is what keeps StatIfExists's vanished-dir detection
// (TestStatIfExists_DirVanished) working, and is the other side of the
// condition pinned by TestStat_BuildWithoutBytesDirOutput.
func TestStat_DirOutputWithoutActionDigest(t *testing.T) {
	ctx := t.Context()
	hfs, dir := setupRemoteDirOutput(ctx, t, digest.Digest{})
	if _, err := hfs.Stat(ctx, dir, remoteDirOutputName); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Stat(%q)=_, %v; want %v (no action digest is not CAS-backed)", remoteDirOutputName, err, fs.ErrNotExist)
	}
}

// TestStat_BuildWithoutBytesDirOutputTrailingSlash verifies a trailing-slash
// Stat of a build-without-bytes dir output reaches the same hashfs entry as
// the bare name. A directory target is named with a trailing slash throughout
// the build graph (e.g. "gen/" nodes reach deps/step_config as "gen/"). A miss
// here made deps drop a build-without-bytes dir input as missing, so it was
// never flushed for a local consumer.
func TestStat_BuildWithoutBytesDirOutputTrailingSlash(t *testing.T) {
	const slashed = remoteDirOutputName + "/"
	ctx := t.Context()
	hfs, dir := setupRemoteDirOutput(ctx, t, digest.Digest{Hash: "actionhash", SizeBytes: 10})
	fi, err := hfs.Stat(ctx, dir, slashed)
	if err != nil {
		t.Fatalf("Stat(%q)=_, %v; want nil err (same entry as %q)", slashed, err, remoteDirOutputName)
	}
	if !fi.IsDir() {
		t.Errorf("Stat(%q).IsDir()=false; want true", slashed)
	}
}

// TestStat_LocalDirTrailingSlash verifies a trailing-slash Stat of an on-disk
// directory succeeds and lands on the same hashfs entry as the bare name
// rather than erroring or storing a duplicate entry under the slashed name.
func TestStat_LocalDirTrailingSlash(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	hfs, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := hfs.Close(ctx); err != nil {
			t.Errorf("hfs.Close=%v", err)
		}
	})

	fi, err := hfs.Stat(ctx, dir, "subdir/")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Stat(subdir/)=%v, %v; want %v (not yet on disk)", fi, err, fs.ErrNotExist)
	}

	if err := os.MkdirAll(filepath.Join(dir, "subdir"), 0755); err != nil {
		t.Fatal(err)
	}
	hfs.Forget(ctx, dir, []path.Path{"subdir"})
	fi, err = hfs.Stat(ctx, dir, "subdir/")
	if err != nil {
		t.Fatalf("Stat(subdir/)=_, %v; want nil err", err)
	}
	if !fi.IsDir() {
		t.Errorf("Stat(subdir/).IsDir()=false; want true")
	}
	fi2, err := hfs.Stat(ctx, dir, "subdir")
	if err != nil {
		t.Fatalf("Stat(subdir)=_, %v; want nil err (bare name reaches the entry the slashed Stat recorded)", err)
	}
	if !fi2.IsDir() {
		t.Errorf("Stat(subdir).IsDir()=false; want true")
	}
}
