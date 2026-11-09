// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package buildconfig

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func TestFilegroupGlob(t *testing.T) {
	dir := t.TempDir()
	setupFiles(t, dir, map[string]string{
		"base/base.h":        "",
		"base/OWNERS":        "",
		"base/debug/debug.h": "",
		"build/linux/debian_bullseye_amd64-sysroot/usr/lib/gcc/x86_64-linux-gnu/10/crtbegin.o": "",
		"build/linux/debian_bullseye_amd64-sysroot/usr/lib/gcc/x86_64-linux-gnu/10/libasan.so": "",
		"build/linux/debian_bullseye_amd64-sysroot/usr/lib/gcc/x86_64-linux-gnu/10/libgcc.a":   "",
	})
	fsys := os.DirFS(dir)

	t.Run("Match", func(t *testing.T) {
		tests := []struct {
			name      string
			globSpec  globSpec
			wantFiles []string
		}{
			{
				name: "base",
				globSpec: globSpec{
					dir:      "base",
					includes: []string{"*.h"},
				},
				wantFiles: []string{
					"base/base.h",
					"base/debug/debug.h",
				},
			},
			{
				name: "libgcc",
				globSpec: globSpec{
					dir:      "build/linux/debian_bullseye_amd64-sysroot/usr/lib/gcc/x86_64-linux-gnu",
					includes: []string{"*.o", "*.so", "*.a"},
				},
				wantFiles: []string{
					"build/linux/debian_bullseye_amd64-sysroot/usr/lib/gcc/x86_64-linux-gnu/10/crtbegin.o",
					"build/linux/debian_bullseye_amd64-sysroot/usr/lib/gcc/x86_64-linux-gnu/10/libasan.so",
					"build/linux/debian_bullseye_amd64-sysroot/usr/lib/gcc/x86_64-linux-gnu/10/libgcc.a",
				},
			},
			{
				name: "exclude",
				globSpec: globSpec{
					dir:      "build/linux/debian_bullseye_amd64-sysroot/usr/lib/gcc/x86_64-linux-gnu",
					includes: []string{"*"},
					excludes: []string{"*.a"},
				},
				wantFiles: []string{
					"build/linux/debian_bullseye_amd64-sysroot/usr/lib/gcc/x86_64-linux-gnu/10/crtbegin.o",
					"build/linux/debian_bullseye_amd64-sysroot/usr/lib/gcc/x86_64-linux-gnu/10/libasan.so",
				},
			},
			{
				name: "include-with-slash",
				globSpec: globSpec{
					dir:      "base",
					includes: []string{"debug/*.h"},
				},
				wantFiles: []string{
					"base/debug/debug.h",
				},
			},
			{
				name: "exclude-with-slash",
				globSpec: globSpec{
					dir:      "base",
					includes: []string{"*.h"},
					excludes: []string{"debug/*"},
				},
				wantFiles: []string{
					"base/base.h",
				},
			},
			{
				name: "no-match",
				globSpec: globSpec{
					dir:      "base",
					includes: []string{"*.cc"},
				},
				wantFiles: nil,
			},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				ctx := t.Context()
				got, err := tc.globSpec.Update(ctx, fsys, filegroup{})
				if err != nil {
					t.Fatalf("globSpec.Update(...) unexpected error: %v", err)
				}
				if got.etag != tc.globSpec.hash() {
					t.Errorf("globSpec.Update(...) etag = %q; want %q", got.etag, tc.globSpec.hash())
				}
				if diff := cmp.Diff(tc.wantFiles, got.files); diff != "" {
					t.Errorf("globSpec.Update(...) files -want +got:\n%s", diff)
				}
			})
		}
	})

	t.Run("CacheReuse", func(t *testing.T) {
		spec := globSpec{
			dir:      "base",
			includes: []string{"*.h"},
		}
		cache := filegroup{
			etag: spec.hash(),
			files: []string{
				"base/base.h",
				"base/debug/debug.h",
				"base/version.h",
			},
		}
		got, err := spec.Update(t.Context(), fsys, cache)
		if err != nil {
			t.Fatalf("globSpec.Update(...) unexpected error: %v", err)
		}
		if diff := cmp.Diff(cache, got, cmp.AllowUnexported(filegroup{})); diff != "" {
			t.Errorf("globSpec.Update(...) cache diff -want +got:\n%s", diff)
		}
	})

	t.Run("AbsDir", func(t *testing.T) {
		// On Windows, filepath.IsAbs("/base") returns false because it lacks a drive letter,
		// causing it to fail fs.ValidPath and return early.
		if runtime.GOOS == "windows" {
			t.Skip("filepath.IsAbs('/base') returns false on Windows")
		}
		spec := globSpec{
			dir:      "/base",
			includes: []string{"*.h"},
		}
		got, err := spec.Update(t.Context(), fsys, filegroup{})
		if err != nil {
			t.Fatalf("globSpec.Update(...) unexpected error: %v", err)
		}
		wantFiles := []string{
			"/base/base.h",
			"/base/debug/debug.h",
		}
		if diff := cmp.Diff(wantFiles, got.files); diff != "" {
			t.Errorf("globSpec.Update(...) files -want +got:\n%s", diff)
		}
	})

	t.Run("InvalidDirOutsideWorkspace", func(t *testing.T) {
		spec := globSpec{
			dir:      "../base",
			includes: []string{"*.h"},
		}
		got, err := spec.Update(t.Context(), fsys, filegroup{})
		if err != nil {
			t.Fatalf("globSpec.Update(...) unexpected error: %v", err)
		}
		if len(got.files) != 0 {
			t.Errorf("globSpec.Update(...) got files %v; want empty", got.files)
		}
	})

	t.Run("NonExistentDir", func(t *testing.T) {
		spec := globSpec{
			dir:      "nonexistent",
			includes: []string{"*.h"},
		}
		_, err := spec.Update(t.Context(), fsys, filegroup{})
		if err == nil {
			t.Errorf("globSpec.Update(...) got nil error; want non-nil error")
		}
	})
}

func setupFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for k, v := range files {
		fname := filepath.Join(dir, k)
		err := os.MkdirAll(filepath.Dir(fname), 0755)
		if err != nil {
			t.Fatal(err)
		}
		err = os.WriteFile(fname, []byte(v), 0644)
		if err != nil {
			t.Fatal(err)
		}
		// make sure mtime is updated.
		now := time.Now()
		err = os.Chtimes(fname, time.Time{}, now)
		if err != nil {
			t.Fatal(err)
		}
	}
}
