// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"
)

// readDirFailFS wraps an fs.FS and forces ReadDir on a specific path to fail, simulating a mid-walk failure.
type readDirFailFS struct {
	inner    fs.FS
	failOn   string
	failWith error
}

func (f readDirFailFS) Open(name string) (fs.File, error) { return f.inner.Open(name) }

func (f readDirFailFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if name == f.failOn {
		return nil, f.failWith
	}
	return fs.ReadDir(f.inner, name)
}

func (f readDirFailFS) Stat(name string) (fs.FileInfo, error) { return fs.Stat(f.inner, name) }

// TestDirEffectiveMtime_WalkErrorSurfaces verifies dirEffectiveMtime surfaces (does not swallow) an error when the walk fails partway through.
func TestDirEffectiveMtime_WalkErrorSurfaces(t *testing.T) {
	now := time.Now()
	inner := fstest.MapFS{
		"testdir":           {Mode: fs.ModeDir | 0755, ModTime: now},
		"testdir/a.txt":     {Data: []byte("a"), ModTime: now},
		"testdir/sub":       {Mode: fs.ModeDir | 0755, ModTime: now},
		"testdir/sub/b.txt": {Data: []byte("b"), ModTime: now},
	}
	wantErr := errors.New("simulated readdir failure")
	fsys := readDirFailFS{inner: inner, failOn: "testdir/sub", failWith: wantErr}

	_, err := dirEffectiveMtime(fsys, "testdir")
	if err == nil {
		t.Fatal("dirEffectiveMtime: nil err; want walk error surfaced, not swallowed")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("dirEffectiveMtime err=%v; want wraps %v", err, wantErr)
	}
}

func TestDirEffectiveMtime(t *testing.T) {
	root := t.TempDir()

	dir := filepath.Join(root, "testdir")
	subdir := filepath.Join(dir, "sub")
	err := os.MkdirAll(subdir, 0755)
	if err != nil {
		t.Fatal(err)
	}

	baseTime := time.Now().Truncate(time.Second)
	files := map[string]time.Time{
		"a.txt":     baseTime.Add(-2 * time.Second),
		"b.txt":     baseTime.Add(-1 * time.Second),
		"sub/c.txt": baseTime, // newest file
	}
	for name, mtime := range files {
		p := filepath.Join(dir, name)
		err := os.WriteFile(p, []byte("content"), 0644)
		if err != nil {
			t.Fatal(err)
		}
		err = os.Chtimes(p, mtime, mtime)
		if err != nil {
			t.Fatal(err)
		}
	}

	// Pin the dir mtimes older than every file so the result is driven by the
	// newest file, not the wall-clock mtime MkdirAll stamped on the dirs.
	oldDir := baseTime.Add(-time.Hour)
	for _, d := range []string{subdir, dir} {
		if err := os.Chtimes(d, oldDir, oldDir); err != nil {
			t.Fatal(err)
		}
	}

	got, err := dirEffectiveMtime(os.DirFS(root), "testdir")
	if err != nil {
		t.Fatalf("dirEffectiveMtime: %v", err)
	}

	// The newest file (sub/c.txt) is at baseTime and the directories are
	// older, so the effective mtime must equal baseTime exactly.
	if !got.Equal(baseTime) {
		t.Errorf("dirEffectiveMtime = %v, want %v (newest file mtime)", got, baseTime)
	}
}

func TestDirEffectiveMtime_Empty(t *testing.T) {
	root := t.TempDir()

	dir := filepath.Join(root, "emptydir")
	err := os.MkdirAll(dir, 0755)
	if err != nil {
		t.Fatal(err)
	}
	// Pin the dir's mtime so the assertion checks an exact value, not just "not zero".
	want := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(dir, want, want); err != nil {
		t.Fatal(err)
	}

	got, err := dirEffectiveMtime(os.DirFS(root), "emptydir")
	if err != nil {
		t.Fatalf("dirEffectiveMtime: %v", err)
	}

	// An empty dir has no members, so the effective mtime is the dir's own.
	if !got.Equal(want) {
		t.Errorf("dirEffectiveMtime = %v, want %v (the dir's own mtime)", got, want)
	}
}

func TestDirEffectiveMtime_NestedUpdate(t *testing.T) {
	root := t.TempDir()

	dir := filepath.Join(root, "testdir")
	subdir := filepath.Join(dir, "a", "b")
	err := os.MkdirAll(subdir, 0755)
	if err != nil {
		t.Fatal(err)
	}

	oldTime := time.Now().Add(-10 * time.Second).Truncate(time.Second)
	deepFile := filepath.Join(subdir, "deep.txt")
	err = os.WriteFile(deepFile, []byte("deep"), 0644)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{deepFile, subdir, filepath.Join(dir, "a"), dir} {
		err = os.Chtimes(p, oldTime, oldTime)
		if err != nil {
			t.Fatal(err)
		}
	}

	mtime1, err := dirEffectiveMtime(os.DirFS(root), "testdir")
	if err != nil {
		t.Fatalf("dirEffectiveMtime: %v", err)
	}

	newTime := time.Now().Truncate(time.Second)
	err = os.WriteFile(deepFile, []byte("updated"), 0644)
	if err != nil {
		t.Fatal(err)
	}
	err = os.Chtimes(deepFile, newTime, newTime)
	if err != nil {
		t.Fatal(err)
	}

	mtime2, err := dirEffectiveMtime(os.DirFS(root), "testdir")
	if err != nil {
		t.Fatalf("dirEffectiveMtime: %v", err)
	}

	if !mtime2.After(mtime1) {
		t.Errorf("mtime after update (%v) should be after mtime before update (%v)", mtime2, mtime1)
	}
}

func TestDirEffectiveMtime_FileDeletion(t *testing.T) {
	root := t.TempDir()

	dir := filepath.Join(root, "testdir")
	err := os.MkdirAll(dir, 0755)
	if err != nil {
		t.Fatal(err)
	}

	oldTime := time.Now().Add(-10 * time.Second).Truncate(time.Second)
	file := filepath.Join(dir, "a.txt")
	err = os.WriteFile(file, []byte("content"), 0644)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{file, dir} {
		err = os.Chtimes(p, oldTime, oldTime)
		if err != nil {
			t.Fatal(err)
		}
	}

	mtime1, err := dirEffectiveMtime(os.DirFS(root), "testdir")
	if err != nil {
		t.Fatalf("dirEffectiveMtime: %v", err)
	}

	// Deleting a file updates the parent directory's mtime.
	err = os.Remove(file)
	if err != nil {
		t.Fatal(err)
	}

	mtime2, err := dirEffectiveMtime(os.DirFS(root), "testdir")
	if err != nil {
		t.Fatalf("dirEffectiveMtime: %v", err)
	}

	if !mtime2.After(mtime1) {
		t.Errorf("mtime after deletion (%v) should be after mtime before deletion (%v)", mtime2, mtime1)
	}
}

func TestDirEffectiveMtime_NonExistent(t *testing.T) {
	root := t.TempDir()

	_, err := dirEffectiveMtime(os.DirFS(root), "nonexistent")
	if err == nil {
		t.Fatal("dirEffectiveMtime should return error for non-existent directory")
	}
}

// mapTree builds a synthetic directory tree with n regular files spread
// across dirsPer subdirectories, each file with a distinct mtime.
func mapTree(n, dirsPer int, base time.Time) fstest.MapFS {
	fsys := fstest.MapFS{"root": &fstest.MapFile{Mode: fs.ModeDir | 0755, ModTime: base}}
	for i := range n {
		dirPath := fmt.Sprintf("root/sub%04d", i%dirsPer)
		if _, ok := fsys[dirPath]; !ok {
			fsys[dirPath] = &fstest.MapFile{Mode: fs.ModeDir | 0755, ModTime: base}
		}
		fsys[fmt.Sprintf("%s/f%07d.txt", dirPath, i)] = &fstest.MapFile{
			Data:    []byte("x"),
			ModTime: base.Add(time.Duration(i) * time.Millisecond),
		}
	}
	return fsys
}

// BenchmarkDirEffectiveMtime measures how the per-directory tree walk scales with file count, guarding the directory-target up-to-date path against regressions.
func BenchmarkDirEffectiveMtime(b *testing.B) {
	base := time.Now()
	for _, n := range []int{1_000, 10_000, 100_000} {
		fsys := mapTree(n, 200, base)
		b.Run(fmt.Sprintf("files=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := dirEffectiveMtime(fsys, "root"); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
