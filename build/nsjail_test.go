// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"maps"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestNewNSJailExecutor_DefaultDirs(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("NSJail is only supported on linux")
	}
	wsRoot := t.TempDir()
	b := &Builder{
		path: NewPath(wsRoot, "out/siso"),
	}
	for _, tc := range []struct {
		name             string
		extraConfig      map[string]string
		wantPublicDirs   []string
		wantWritableDirs []string
	}{
		{
			name:             "default_when_unset",
			extraConfig:      nil,
			wantPublicDirs:   nil,
			wantWritableDirs: nil,
		},
		{
			name: "only_default_readable_dirs_set_replaces_both_default_lists",
			extraConfig: map[string]string{
				"default_readable_dirs": "/custom/bin:/custom/lib",
			},
			wantPublicDirs:   []string{"/custom/bin", "/custom/lib"},
			wantWritableDirs: []string{},
		},
		{
			name: "only_default_writable_dirs_set_replaces_both_default_lists",
			extraConfig: map[string]string{
				"default_writable_dirs": "/custom/rw1:/custom/rw2",
			},
			wantPublicDirs:   []string{},
			wantWritableDirs: []string{"/custom/rw1", "/custom/rw2"},
		},
		{
			name: "both_default_readable_dirs_and_default_writable_dirs_set",
			extraConfig: map[string]string{
				"default_readable_dirs": "/custom/bin:rel/read",
				"default_writable_dirs": "/custom/rw:rel/write",
			},
			wantPublicDirs:   []string{"/custom/bin", filepath.Join(wsRoot, "rel/read")},
			wantWritableDirs: []string{"/custom/rw", filepath.Join(wsRoot, "rel/write")},
		},
		{
			name: "explicit_empty_default_readable_dirs_clears_defaults",
			extraConfig: map[string]string{
				"default_readable_dirs": "",
			},
			wantPublicDirs:   []string{},
			wantWritableDirs: []string{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := map[string]string{
				"nsjail_path":    "/bin/true",
				"nsjail_workdir": ".nsjail_workdir",
				"nsjail_outdir":  "out/siso",
			}
			maps.Copy(cfg, tc.extraConfig)
			exec, err := newNSJailExecutor(t.Context(), b, nil, cfg)
			if err != nil {
				t.Fatalf("newNSJailExecutor: %v", err)
			}
			if diff := cmp.Diff(tc.wantPublicDirs, exec.req.PublicDirs); diff != "" {
				t.Errorf("PublicDirs mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.wantWritableDirs, exec.req.WritableDirs); diff != "" {
				t.Errorf("WritableDirs mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
