// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package hashfs_test

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/testing/protocmp"

	"go.chromium.org/build/siso/hashfs"
	pb "go.chromium.org/build/siso/hashfs/proto"
	"go.chromium.org/build/siso/reapi/digest"
	"go.chromium.org/build/siso/reapi/merkletree"
)

// mockState returns a mock hashfs state with two entries for testing.
func mockState(t *testing.T) *pb.State {
	t.Helper()

	// Add enough entries to make sure that the state file is > 100kB, so that we exercise
	// enough of any block-based compression code.
	state := &pb.State{}
	randomBytes := make([]byte, 1024)
	if _, err := rand.Read(randomBytes); err != nil {
		t.Fatalf("rand.Read(...)=%v; want nil error", err)
	}
	for i := range 100 {
		state.Entries = append(state.Entries, &pb.Entry{
			Id: &pb.FileID{
				ModTime: int64(i),
			},
			Name:    fmt.Sprintf("file%d", i),
			CmdHash: randomBytes,
		})
	}

	// Ensure that the state file when serialized is > 500kB.
	b, err := proto.Marshal(state)
	if err != nil {
		t.Fatalf("proto.Marshal(%v)=%v; want nil error", state, err)
	}
	if len(b) < 100*1024 {
		t.Fatalf("len(b)=%d; want >100kB", len(b))
	}

	return state
}

func TestLoadMissingStateFile(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	dir := t.TempDir()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}

	opts := hashfs.Option{
		StateFile:     filepath.Join(dir, ".siso_fs_state"),
		CompressZstd:  false,
		CompressLevel: 3,
	}

	// Handle the case where the state file doesn't exist.
	t.Logf("initial load (fs state doesn't exist)")
	loadedState, err := hashfs.Load(ctx, opts)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Load(...)=%v, %v; want %v", loadedState, err, fs.ErrNotExist)
	}
}

func TestLoadSaveEmptyState(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	dir := t.TempDir()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}

	opts := hashfs.Option{
		StateFile:     filepath.Join(dir, ".siso_fs_state"),
		CompressZstd:  false,
		CompressLevel: 3,
	}

	// Saving an empty state works.
	t.Logf("initial save (empty state)")
	savedState := &pb.State{}
	if err := hashfs.Save(ctx, savedState, opts); err != nil {
		t.Errorf("Save(...)=%v; want nil", err)
	}

	// Loading the saved empty state file works.
	t.Logf("second load (empty state)")
	loadedState, err := hashfs.Load(ctx, opts)
	if err != nil {
		t.Errorf("Load(...)=%v, %v; want nil err", loadedState, err)
	}

	// Loaded state should be equal to the saved state.
	if diff := cmp.Diff(savedState, loadedState, protocmp.Transform()); diff != "" {
		t.Errorf("Load(...) diff -want +got:\n%s", diff)
	}
}

func TestLoadSave(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	savedState := mockState(t)

	// Test with both gzip and zstd compression.
	tests := map[string]bool{
		"gzip": false,
		"zstd": true,
	}
	for name, useZstd := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			dir, err := filepath.EvalSymlinks(dir)
			if err != nil {
				t.Fatal(err)
			}

			opts := hashfs.Option{
				StateFile:     filepath.Join(dir, ".siso_fs_state"),
				CompressZstd:  useZstd,
				CompressLevel: 3,
			}

			// Save a mock state.
			t.Logf("save with useZstd=%v", opts.CompressZstd)
			if err := hashfs.Save(ctx, savedState, opts); err != nil {
				t.Errorf("Save(...)=%v; want nil", err)
			}

			// Check if the file was really saved with the chosen compression method by checking
			// whether the first bytes are the correct magic bytes.
			b, err := os.ReadFile(opts.StateFile)
			if err != nil {
				t.Fatalf("Could not read %q: %v", opts.StateFile, err)
			}
			if useZstd {
				if diff := cmp.Diff([]byte{0x28, 0xb5, 0x2f, 0xfd}, b[:4]); diff != "" {
					t.Errorf("Save(...) missing zstd header, diff -want +got:\n%s", diff)
				}
			} else {
				if diff := cmp.Diff([]byte{0x1f, 0x8b}, b[:2]); diff != "" {
					t.Errorf("Save(...) missing gzip header, diff -want +got:\n%s", diff)
				}
			}

			// Load the saved state.
			t.Logf("load with useZstd=%v", opts.CompressZstd)
			loadedState, err := hashfs.Load(ctx, opts)
			if err != nil {
				t.Errorf("Load(...)=%v, %v; want nil err", loadedState, err)
			}

			// Compare the loaded state with the saved state.
			if diff := cmp.Diff(savedState, loadedState, protocmp.Transform()); diff != "" {
				t.Errorf("Load(...) diff -want +got:\n%s", diff)
			}

			// Simulate the user flipping the -fs_state_use_zstd flag and verify that we can
			// still load the state file.
			opts.CompressZstd = !opts.CompressZstd
			t.Logf("load with useZstd=%v", opts.CompressZstd)
			loadedState, err = hashfs.Load(ctx, opts)
			if err != nil {
				t.Errorf("Load(...)=%v, %v; want nil err", loadedState, err)
			}

			// Compare the loaded state with the saved state.
			if diff := cmp.Diff(savedState, loadedState, protocmp.Transform()); diff != "" {
				t.Errorf("Load(...) diff -want +got:\n%s", diff)
			}
		})
	}
}

// TestBgzfCompatibility tests that a state file saved with plain gzip compression can be
// loaded with bgzf compression enabled, and vice versa.
func TestBgzfCompatibility(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	savedState := mockState(t)

	tests := map[string]bool{
		"save_bgzf_load_gzip": true,
		"save_gzip_load_bgzf": false,
	}
	for name, saveWithBgzf := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			dir, err := filepath.EvalSymlinks(dir)
			if err != nil {
				t.Fatal(err)
			}

			opts := hashfs.Option{
				StateFile:     filepath.Join(dir, ".siso_fs_state"),
				GzipUsesBgzf:  saveWithBgzf,
				CompressZstd:  false,
				CompressLevel: 3,
			}

			// Save a mock state.
			if err := hashfs.Save(ctx, savedState, opts); err != nil {
				t.Errorf("Save(...)=%v; want nil", err)
			}

			// Load it using the opposite bgzf setting.
			opts.GzipUsesBgzf = !opts.GzipUsesBgzf
			loadedState, err := hashfs.Load(ctx, opts)
			if err != nil {
				t.Errorf("Load(...)=%v, %v; want nil err", loadedState, err)
			}

			// Compare the loaded state with the saved state.
			if diff := cmp.Diff(savedState, loadedState, protocmp.Transform()); diff != "" {
				t.Errorf("Load(...) diff -want +got:\n%s", diff)
			}
		})
	}
}

func TestState(t *testing.T) {
	ctx := t.Context()

	dir := t.TempDir()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}

	opts := hashfs.Option{
		StateFile:     filepath.Join(dir, ".siso_fs_state"),
		CompressZstd:  false,
		CompressLevel: 3,
	}

	hashFS, err := hashfs.New(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer hashFS.Close(ctx)

	err = hashFS.WaitReady(ctx)
	if err != nil {
		t.Fatalf("WaitReady=%v; want nil", err)
	}

	err = hashFS.WriteFile(ctx, dir, "stamp", nil, false, time.Now(), []byte("dummy-cmdhash"), nil)
	if err != nil {
		t.Errorf("WriteFile(...)=%v; want nil error", err)
	}

	st := hashFS.State(ctx)
	m := hashfs.StateMap(st)
	_, ok := m[filepath.ToSlash(filepath.Join(dir, "stamp"))]
	if !ok {
		names := make([]string, 0, len(m))
		for k := range m {
			names = append(names, k)
		}
		sort.Strings(names)
		t.Errorf("stamp entry not exists? %q", names)
	}
}

func TestState_Dir(t *testing.T) {
	ctx := t.Context()

	dir := t.TempDir()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}

	opts := hashfs.Option{
		StateFile:     filepath.Join(dir, ".siso_fs_state"),
		CompressZstd:  false,
		CompressLevel: 3,
	}

	hashFS, err := hashfs.New(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer hashFS.Close(ctx)

	err = hashFS.WaitReady(ctx)
	if err != nil {
		t.Fatalf("WaitReady=%v; want nil", err)
	}

	mtime := time.Now()
	h := sha256.New()
	fmt.Fprint(h, "step command")
	cmdhash := h.Sum(nil)
	cmdhashStr := base64.StdEncoding.EncodeToString(cmdhash)
	d := digest.FromBytes("action digest", []byte("action proto")).Digest()
	t.Logf("record gen/generate_all dir. mtime=%s cmdhash=%s d=%s", mtime, cmdhashStr, d)
	err = update(ctx, hashFS, dir, []merkletree.Entry{
		{
			Name: "gen/generate_all",
		},
	}, mtime, cmdhash, d)
	if err != nil {
		t.Fatalf("Update %v; want nil err", err)
	}

	st := hashFS.State(ctx)
	m := hashfs.StateMap(st)
	ent, ok := m[filepath.ToSlash(filepath.Join(dir, "gen/generate_all"))]
	if !ok {
		t.Errorf("gen/generate_all entry not exists?")
	}
	if ent == nil {
		t.Fatalf("gen/generate_all entry is nil")
	}
	if ent.Id.ModTime != mtime.UnixNano() {
		t.Errorf("mtime=%d want=%d", ent.Id.ModTime, mtime.UnixNano())
	}
	if !bytes.Equal(ent.CmdHash, cmdhash) {
		t.Errorf("cmdhash=%s want=%s", base64.StdEncoding.EncodeToString(ent.CmdHash), cmdhashStr)
	}
	if ent.Action.Hash != d.Hash || ent.Action.SizeBytes != d.SizeBytes {
		t.Errorf("action=%s want=%s", ent.Action, d)
	}
}

func TestState_BadDirEntry(t *testing.T) {
	ctx := t.Context()

	dir := t.TempDir()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}

	opts := hashfs.Option{
		StateFile:     filepath.Join(dir, ".siso_fs_state"),
		CompressZstd:  false,
		CompressLevel: 3,
	}
	mtime := time.Now()
	h := sha256.New()
	fmt.Fprint(h, "step command")
	cmdhash := h.Sum(nil)
	cmdhashStr := base64.StdEncoding.EncodeToString(cmdhash)
	d := digest.FromBytes("action digest", []byte("action proto")).Digest()

	func() {
		t.Logf("-- generate .siso_fs_state for gen/output_file as dir")
		hashFS, err := hashfs.New(ctx, opts)
		if err != nil {
			t.Fatal(err)
		}
		defer hashFS.Close(ctx)

		err = hashFS.WaitReady(ctx)
		if err != nil {
			t.Fatalf("WaitReady=%v; want nil", err)
		}

		t.Logf("-- record gen/output_file. mtime=%s cmdhash=%s d=%s", mtime, cmdhashStr, d)
		err = update(ctx, hashFS, dir, []merkletree.Entry{
			{
				Name: "gen/output_file",
				// couldn't calculate file's digest.
			},
		}, mtime, cmdhash, d)
		if err != nil {
			t.Fatalf("Update %v; want nil err", err)
		}
	}()

	st, err := hashfs.Load(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	m := hashfs.StateMap(st)
	ent, ok := m[filepath.ToSlash(filepath.Join(dir, "gen/output_file"))]
	if !ok {
		t.Errorf("gen/output_file entry not exists?")
	}
	if ent == nil {
		t.Fatalf("gen/output_file entry is nil")
	}
	if ent.Id.ModTime != mtime.UnixNano() {
		t.Errorf("mtime=%d want=%d", ent.Id.ModTime, mtime.UnixNano())
	}
	if !bytes.Equal(ent.CmdHash, cmdhash) {
		t.Errorf("cmdhash=%s want=%s", base64.StdEncoding.EncodeToString(ent.CmdHash), cmdhashStr)
	}
	if ent.Action.Hash != d.Hash || ent.Action.SizeBytes != d.SizeBytes {
		t.Errorf("action=%s want=%s", ent.Action, d)
	}

	t.Logf("-- ensure gen/output_file exists on disk")
	err = os.MkdirAll(filepath.Join(dir, "gen"), 0755)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(filepath.Join(dir, "gen/output_file"), []byte("output file"), 0644)
	if err != nil {
		t.Fatal(err)
	}
	err = os.Chtimes(filepath.Join(dir, "gen/output_file"), time.Time{}, mtime)
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("-- gen/output_file is dir in fs_state, but file on disk. should be invalidated")
	hashFS, err := hashfs.New(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer hashFS.Close(ctx)

	err = hashFS.WaitReady(ctx)
	if err != nil {
		t.Fatalf("WaitReady=%v; want nil", err)
	}
	if hashFS.IsClean([]string{}) {
		t.Error("IsClean=true; want false")
	}
	st = hashFS.State(ctx)
	m = hashfs.StateMap(st)
	_, ok = m[filepath.ToSlash(filepath.Join(dir, "gen/output_file"))]
	if ok {
		t.Errorf("gen/output_file entry exists; want not exists")
	}
}

func TestState_Symlink(t *testing.T) {
	ctx := t.Context()

	dir := t.TempDir()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}

	opts := hashfs.Option{
		StateFile:     filepath.Join(dir, ".siso_fs_state"),
		CompressZstd:  false,
		CompressLevel: 3,
	}

	err = os.WriteFile(filepath.Join(dir, "target.0"), []byte("target.0"), 0644)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(filepath.Join(dir, "target.1"), []byte("target.1"), 0644)
	if err != nil {
		t.Fatal(err)
	}
	err = os.Symlink("target.0", filepath.Join(dir, "symlink"))
	if err != nil {
		t.Fatal(err)
	}

	func() {
		hashFS, err := hashfs.New(ctx, opts)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			err := hashFS.Close(ctx)
			if err != nil {
				t.Errorf("close=%v", err)
			}
		}()
		err = hashFS.WaitReady(ctx)
		if err != nil {
			t.Fatalf("WaitReady=%v; want nil", err)
		}
		fi, err := hashFS.Stat(ctx, dir, "symlink")
		if err != nil {
			t.Fatalf("Stat(%q)=%v", "symlink", err)
		}
		if fi.Mode()&fs.ModeSymlink != fs.ModeSymlink {
			t.Errorf("mode=%v; want symlink", fi.Mode())
		}
		if fi.Target() != "target.0" {
			t.Errorf("target=%q; want=%q", fi.Target(), "target.0")
		}
		// make dirty to write state file
		err = hashFS.WriteFile(ctx, dir, "stamp", nil, false, time.Now(), []byte("dummy-cmdhash"), nil)
		if err != nil {
			t.Errorf("WriteFile(...)=%v; want nil error", err)
		}
	}()
	st, err := hashfs.Load(ctx, opts)
	if err != nil {
		t.Fatalf("load %v", err)
	}
	m := hashfs.StateMap(st)
	e, ok := m[filepath.ToSlash(filepath.Join(dir, "symlink"))]
	if !ok {
		t.Errorf("no symlnk: %v", m)
	}
	if e.Target != "target.0" {
		t.Errorf("symlink target=%q; want=%q", e.Target, "target.0")
	}

	t.Logf("-- modify symlink")
	err = os.Remove(filepath.Join(dir, "symlink"))
	if err != nil {
		t.Fatal(err)
	}
	err = os.Symlink("target.1", filepath.Join(dir, "symlink"))
	if err != nil {
		t.Fatal(err)
	}
	func() {
		hashFS, err := hashfs.New(ctx, opts)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			err := hashFS.Close(ctx)
			if err != nil {
				t.Errorf("close=%v", err)
			}
		}()
		err = hashFS.WaitReady(ctx)
		if err != nil {
			t.Fatalf("WaitReady=%v; want nil", err)
		}
		fi, err := hashFS.Stat(ctx, dir, "symlink")
		if err != nil {
			t.Fatalf("Stat(%q)=%v", "symlink", err)
		}
		if fi.Mode()&fs.ModeSymlink != fs.ModeSymlink {
			t.Errorf("mode=%v; want symlink", fi.Mode())
		}
		if fi.Target() != "target.1" {
			t.Errorf("target=%q; want=%q", fi.Target(), "target.1")
		}
	}()
}

func createBenchmarkState(tb testing.TB, dir string) *pb.State {
	for i := range 60000 {
		err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%d.txt", i)), nil, 0644)
		if err != nil {
			tb.Fatal(err)
		}
	}
	ctx := tb.Context()
	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		tb.Fatal(err)
	}
	defer hashFS.Close(ctx)
	err = hashFS.WaitReady(ctx)
	if err != nil {
		tb.Fatalf("WaitReady=%v; want nil", err)
	}

	fsys := hashFS.FileSystem(ctx, dir)
	err = fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		return err
	})
	if err != nil {
		tb.Fatal(err)
	}
	st := hashFS.State(ctx)
	return st
}

func BenchmarkSetState(b *testing.B) {
	dir := b.TempDir()
	st := createBenchmarkState(b, dir)
	ctx := b.Context()

	b.ReportAllocs()

	for b.Loop() {
		hashFS, err := hashfs.New(ctx, hashfs.Option{})
		if err != nil {
			b.Fatal(err)
		}
		err = hashFS.SetState(ctx, st)
		if err != nil {
			hashFS.Close(ctx)
			b.Fatal(err)
		}
		hashFS.Close(ctx)
	}
}

func BenchmarkLoadState(b *testing.B) {
	dir := b.TempDir()
	ctx := b.Context()
	st := createBenchmarkState(b, dir)

	opts := hashfs.Option{
		StateFile:     filepath.Join(dir, ".siso_fs_state"),
		CompressZstd:  false,
		CompressLevel: 3,
	}

	err := hashfs.Save(ctx, st, opts)
	if err != nil {
		b.Fatal(err)
	}
	origState := st
	b.ReportAllocs()

	for b.Loop() {
		st, err := hashfs.Load(ctx, opts)
		if err != nil {
			b.Fatal(err)
		}
		if len(st.Entries) != len(origState.Entries) {
			b.Fatalf("mismatch entries got=%d want=%d", len(st.Entries), len(origState.Entries))
		}
	}
}
