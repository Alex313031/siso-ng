// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package abfsutil

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	"go.chromium.org/build/hashigo/digest"

	"go.chromium.org/build/siso/blob"
	"go.chromium.org/build/siso/path"
	"go.chromium.org/build/siso/reapi/merkletree"
)

func TestFake(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()

	file1Path := filepath.Join(dir, "src/foo.txt")
	if err := os.MkdirAll(filepath.Dir(file1Path), 0755); err != nil {
		t.Fatalf("MkdirAll() = %v", err)
	}
	file1Content := []byte("hello world")
	if err := os.WriteFile(file1Path, file1Content, 0644); err != nil {
		t.Fatalf("WriteFile() = %v", err)
	}
	h1 := sha256.Sum256(file1Content)
	d1 := digest.Digest{Hash: hex.EncodeToString(h1[:]), SizeBytes: int64(len(file1Content))}

	symlinkPath := filepath.Join(dir, "src/link.txt")
	if err := os.Symlink("foo.txt", symlinkPath); err != nil {
		t.Fatalf("Symlink() = %v", err)
	}
	hLink := sha256.Sum256([]byte("foo.txt"))
	dLink := digest.Digest{Hash: hex.EncodeToString(hLink[:]), SizeBytes: int64(len("foo.txt"))}

	setFileContent := []byte("generated content")
	hSet := sha256.Sum256(setFileContent)
	dSet := digest.Digest{Hash: hex.EncodeToString(hSet[:]), SizeBytes: int64(len(setFileContent))}

	fake := &Fake{
		Dir: dir,
		Store: map[digest.Digest][]byte{
			dSet: setFileContent,
		},
	}

	sockPath := filepath.Join(t.TempDir(), "abfs.sock")
	client := fake.Start(ctx, t, sockPath)

	// Test Digest (single get)
	gotD1, err := client.Digest(ctx, file1Path)
	if err != nil {
		t.Errorf("client.Digest(file1) = %v, want nil", err)
	}
	if gotD1 != d1 {
		t.Errorf("client.Digest(file1) = %v, want %v", gotD1, d1)
	}

	gotDLink, err := client.Digest(ctx, symlinkPath)
	if err != nil {
		t.Errorf("client.Digest(symlink) = %v, want nil", err)
	}
	if gotDLink != dLink {
		t.Errorf("client.Digest(symlink) = %v, want %v", gotDLink, dLink)
	}

	_, err = client.Digest(ctx, filepath.Join(dir, "nonexistent.txt"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("client.Digest(nonexistent) = %v, want fs.ErrNotExist", err)
	}

	// Test BatchDigests
	batchReq := []string{
		file1Path,
		symlinkPath,
		filepath.Join(dir, "nonexistent.txt"),
	}
	gotBatch, err := client.BatchDigests(ctx, batchReq)
	if err != nil {
		t.Fatalf("client.BatchDigests() = %v, want nil", err)
	}
	wantBatch := []digest.Digest{
		d1,
		dLink,
		{}, // nonexistent file has empty digest
	}
	if diff := cmp.Diff(wantBatch, gotBatch); diff != "" {
		t.Errorf("BatchDigests diff (-want +got):\n%s", diff)
	}

	// Test RegisterFiles (set-rbe-digests)
	entries := []*Registration{
		{
			Entry: merkletree.Entry{
				Name: path.Path("out/gen.txt"),
				Data: blob.NewData(nil, dSet),
			},
		},
		{
			Entry: merkletree.Entry{
				Name: path.Path("out/unknown.txt"),
				Data: blob.NewData(nil, digest.Digest{Hash: "unknown", SizeBytes: 99}),
			},
		},
	}
	err = client.RegisterFiles(ctx, dir, entries)
	if err != nil {
		t.Fatalf("client.RegisterFiles() = %v, want nil", err)
	}
	if entries[0].Err != nil {
		t.Errorf("entries[0].Err = %v, want nil", entries[0].Err)
	}
	if entries[1].Err == nil {
		t.Errorf("entries[1].Err = nil, want error")
	}

	// Verify file was written to disk
	writtenContent, err := os.ReadFile(filepath.Join(dir, "out/gen.txt"))
	if err != nil {
		t.Fatalf("ReadFile(out/gen.txt) = %v", err)
	}
	if string(writtenContent) != string(setFileContent) {
		t.Errorf("ReadFile(out/gen.txt) = %q, want %q", writtenContent, setFileContent)
	}

	// Verify request counts
	wantGetDigests := map[string]int{
		"src/foo.txt":     2,
		"src/link.txt":    2,
		"nonexistent.txt": 2,
	}
	if diff := cmp.Diff(wantGetDigests, fake.GetRBEDigests()); diff != "" {
		t.Errorf("GetRBEDigests() diff (-want +got):\n%s", diff)
	}

	wantSetDigests := map[string]int{
		"out/gen.txt":     1,
		"out/unknown.txt": 1,
	}
	if diff := cmp.Diff(wantSetDigests, fake.SetRBEDigests()); diff != "" {
		t.Errorf("SetRBEDigests() diff (-want +got):\n%s", diff)
	}
}
