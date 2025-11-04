// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package buildconfig

import (
	"os"
	"path/filepath"
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

	baseGlobSpec := globSpec{
		dir:      "base",
		includes: []string{"*.h"},
	}
	libgccGlobSpec := globSpec{
		dir:      "build/linux/debian_bullseye_amd64-sysroot/usr/lib/gcc/x86_64-linux-gnu",
		includes: []string{"*.o", "*.so", "*.a"},
	}
	excludeGlobSpec := globSpec{
		dir:      "build/linux/debian_bullseye_amd64-sysroot/usr/lib/gcc/x86_64-linux-gnu",
		includes: []string{"*"},
		excludes: []string{"*.a"},
	}

	for _, tc := range []struct {
		name     string
		globSpec globSpec
		cache    filegroup
		want     filegroup
	}{
		{
			name:     "base",
			globSpec: baseGlobSpec,
			want: filegroup{
				etag: baseGlobSpec.hash(),
				files: []string{
					"base/base.h",
					"base/debug/debug.h",
				},
			},
		},
		{
			name:     "base-reuse",
			globSpec: baseGlobSpec,
			// when hash matches, don't glob but reuse.
			cache: filegroup{
				etag: baseGlobSpec.hash(),
				files: []string{
					"base/base.h",
					"base/debug/debug.h",
					"base/version.h",
				},
			},
			want: filegroup{
				etag: baseGlobSpec.hash(),
				files: []string{
					"base/base.h",
					"base/debug/debug.h",
					"base/version.h",
				},
			},
		},
		{
			name:     "libgcc",
			globSpec: libgccGlobSpec,
			want: filegroup{
				etag: libgccGlobSpec.hash(),
				files: []string{
					"build/linux/debian_bullseye_amd64-sysroot/usr/lib/gcc/x86_64-linux-gnu/10/crtbegin.o",
					"build/linux/debian_bullseye_amd64-sysroot/usr/lib/gcc/x86_64-linux-gnu/10/libasan.so",
					"build/linux/debian_bullseye_amd64-sysroot/usr/lib/gcc/x86_64-linux-gnu/10/libgcc.a",
				},
			},
		},
		{
			name:     "exclude",
			globSpec: excludeGlobSpec,
			want: filegroup{
				etag: excludeGlobSpec.hash(),
				files: []string{
					"build/linux/debian_bullseye_amd64-sysroot/usr/lib/gcc/x86_64-linux-gnu/10/crtbegin.o",
					"build/linux/debian_bullseye_amd64-sysroot/usr/lib/gcc/x86_64-linux-gnu/10/libasan.so",
				},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			got, err := tc.globSpec.Update(ctx, fsys, tc.cache)
			if err != nil {
				t.Fatalf("globSpec.Update(...)=%v, %v; want nil err", got, err)
			}
			if diff := cmp.Diff(tc.want.etag, got.etag); diff != "" {
				t.Errorf("globSpec.Update(...) etags -want +got:\n%s", diff)
			}
			if diff := cmp.Diff(tc.want.files, got.files); diff != "" {
				t.Errorf("globSpec.Update(...) files -want +got:\n%s", diff)
			}
		})
	}
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
