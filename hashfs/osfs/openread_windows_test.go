// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

//go:build windows

package osfs

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtendedLengthPath(t *testing.T) {
	short := `C:/a/b/c.txt`
	if got, err := extendedLengthPath(short); err != nil || got != short {
		t.Errorf("extendedLengthPath(%q) = %q, %v; want %q, nil (short path unchanged)", short, got, err, short)
	}

	// A forward-slash absolute path past the threshold must come back as a
	// \\?\ extended path with backslashes, or CreateFile rejects it on hosts
	// without global long-path support.
	long := "C:/" + strings.Repeat("dir/", 80) + "leaf.txt"
	got, err := extendedLengthPath(long)
	if err != nil {
		t.Fatalf("extendedLengthPath(long) error: %v", err)
	}
	if !strings.HasPrefix(got, `\\?\`) {
		t.Errorf("extendedLengthPath(long) = %q; want a \\\\?\\ prefix", got)
	}
	if strings.ContainsRune(strings.TrimPrefix(got, `\\?\`), '/') {
		t.Errorf("extendedLengthPath(long) = %q; want no forward slashes after the prefix", got)
	}
}

// TestExtendedLengthPathRelative locks in that the long-path decision uses the
// resolved absolute length, not len(name): a short relative name from a deep
// working directory must still get the \\?\ prefix. This is deterministic
// regardless of the host's global long-path setting.
func TestExtendedLengthPathRelative(t *testing.T) {
	deep := t.TempDir()
	for len(deep) < 300 {
		deep = filepath.Join(deep, "nested_directory_component")
	}
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(deep)

	got, err := extendedLengthPath("leaf.txt")
	if err != nil {
		t.Fatalf("extendedLengthPath(relative): %v", err)
	}
	if !strings.HasPrefix(got, `\\?\`) {
		t.Errorf("extendedLengthPath(%q from deep cwd) = %q; want a \\\\?\\ prefix", "leaf.txt", got)
	}
}

// TestOpenReadDeepPath exercises the real read path (FileSource.Open ->
// openRead -> CreateFile) against a path longer than MAX_PATH.
func TestOpenReadDeepPath(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	deep := root
	for len(deep) < 300 {
		deep = filepath.Join(deep, "nested_directory_component")
	}
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatalf("MkdirAll(deep): %v", err)
	}
	fname := filepath.Join(deep, "leaf.txt")
	const body = "deep-file-body"
	if err := os.WriteFile(fname, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile(deep leaf, len=%d): %v", len(fname), err)
	}

	ofs := New(ctx, "test", Option{})
	rc, err := ofs.FileSource(filepath.ToSlash(fname), int64(len(body))).Open(ctx)
	if err != nil {
		t.Fatalf("FileSource.Open(len=%d): %v", len(fname), err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if got, want := string(got), body; got != want {
		t.Errorf("read %q; want %q", got, want)
	}
}
