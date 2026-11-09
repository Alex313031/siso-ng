// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package buildconfig

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"go.starlark.net/starlark"
	"go.starlark.net/syntax"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/hashfs"
)

func TestParseFilegroups(t *testing.T) {
	tests := []struct {
		name         string
		starlarkCode string
		want         map[string]globSpec
	}{
		{
			name: "implicit-dir",
			starlarkCode: `{
				"base:headers": {
					"type": "glob",
					"includes": ["*.h"],
				},
			}`,
			want: map[string]globSpec{
				"base:headers": {
					dir:      "base",
					includes: []string{"*.h"},
				},
			},
		},
		{
			name: "explicit-dir-and-excludes",
			starlarkCode: `{
				"my_label": {
					"type": "glob",
					"dir": "custom/dir",
					"includes": ["*.o", "*.a"],
					"excludes": ["*_test.o"],
				},
			}`,
			want: map[string]globSpec{
				"my_label": {
					dir:      "custom/dir",
					includes: []string{"*.o", "*.a"},
					excludes: []string{"*_test.o"},
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			thread := &starlark.Thread{Name: "test"}
			v, err := starlark.EvalOptions(&syntax.FileOptions{}, thread, "<test>", tc.starlarkCode, nil)
			if err != nil {
				t.Fatalf("starlark.EvalOptions failed: %v", err)
			}
			fg, err := parseFilegroups(v)
			if err != nil {
				t.Fatalf("parseFilegroups() unexpected error: %v", err)
			}
			got := make(map[string]globSpec)
			for k, updater := range fg {
				gs, ok := updater.(globSpec)
				if !ok {
					t.Fatalf("unexpected updater type: %T", updater)
				}
				got[k] = gs
			}
			if diff := cmp.Diff(tc.want, got, cmp.AllowUnexported(globSpec{})); diff != "" {
				t.Errorf("parseFilegroups() diff (-want +got):\n%s", diff)
			}
		})
	}
}

func TestParseFilegroups_Error(t *testing.T) {
	tests := []struct {
		name         string
		starlarkCode string
	}{
		{
			name: "unsupported-type",
			starlarkCode: `{
				"label": {
					"type": "unknown",
				},
			}`,
		},
		{
			name: "missing-includes",
			starlarkCode: `{
				"label": {
					"type": "glob",
				},
			}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			thread := &starlark.Thread{Name: "test"}
			v, err := starlark.EvalOptions(&syntax.FileOptions{}, thread, "<test>", tc.starlarkCode, nil)
			if err != nil {
				t.Fatalf("starlark.EvalOptions failed: %v", err)
			}
			_, err = parseFilegroups(v)
			if err == nil {
				t.Errorf("parseFilegroups() got nil error; want non-nil error")
			}
		})
	}
}

func TestUpdateFilegroups(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	setupFiles(t, dir, map[string]string{
		"base/base.h": "",
	})

	hfs, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatalf("hashfs.New: %v", err)
	}
	defer hfs.Close(ctx)

	cfg := &Config{
		filegroups: map[string]filegroupUpdater{
			"base:headers": globSpec{
				dir:      "base",
				includes: []string{"*.h"},
			},
			"nonexistent:headers": globSpec{
				dir:      "nonexistent",
				includes: []string{"*.h"},
			},
		},
	}

	bpath := build.NewPath(dir, "out/Default")
	fg, err := cfg.UpdateFilegroups(ctx, hfs, bpath, Filegroups{})
	if err != nil {
		t.Fatalf("UpdateFilegroups: %v", err)
	}

	wantFiles := map[string][]string{
		"base:headers": {"base/base.h"},
	}
	if diff := cmp.Diff(wantFiles, fg.Filegroups); diff != "" {
		t.Errorf("UpdateFilegroups Filegroups diff (-want +got):\n%s", diff)
	}
}
