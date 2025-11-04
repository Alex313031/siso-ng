// Copyright 2025 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/hashfs"
)

func TestBuild_NinjaLog(t *testing.T) {
	ctx := t.Context()
	dir := tempDir(t)

	ninja := func(t *testing.T) (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile: ".siso_fs_state",
		})
		defer cleanup()
		return runNinja(ctx, "build.ninja", graph, opt, []string{"all"}, runNinjaOpts{})
	}

	setupFiles(t, dir, t.Name(), nil)
	t.Logf("-- first build")
	stats, err := ninja(t)
	if err != nil {
		t.Fatalf("ninja %v", err)
	}
	if stats.Done != stats.Total || stats.Total != 2 {
		t.Errorf("done=%d total=%d; want done=2 total=2; %#v", stats.Done, stats.Total, stats)
	}

	buf, err := os.ReadFile(filepath.Join(dir, "out/siso/.ninja_log"))
	if err != nil {
		t.Fatalf("failed to .ninja_log: %v", err)
	}
	t.Logf(".ninja_log:\n%s", string(buf))
	lines := strings.Split(string(buf), "\n")
	if len(lines) == 0 {
		t.Fatalf("empty .ninja_log")
	}
	const ninjaLogMagic = "# ninja log v5"
	if lines[0] != ninjaLogMagic {
		t.Errorf("wrong .ninja_log magic=%q; want=%q", lines[0], ninjaLogMagic)
	}
	wantTargets := []string{
		"build.ninja.stamp",
		"build.ninja",
		"base.stamp",
	}
	if len(lines[1:]) != len(wantTargets)+1 {
		t.Errorf("wrong # of entries=%d; want=%d", len(lines[1:]), len(wantTargets))
	}
	for i, line := range lines[1 : len(lines)-1] {
		t.Logf("line=%q", line)
		fields := strings.Fields(line)
		if len(fields) != 5 {
			t.Fatalf("entry#%d wrong # of fields=%d; want=5", i, len(fields))
		}
		if fields[3] != wantTargets[i] {
			t.Errorf("entry#%d wrong target=%q; want=%q", i, fields[3], wantTargets[i])
		}
	}
	if lines[len(lines)-1] != "" {
		t.Errorf("last entry=%q; want empty", lines[len(lines)-1])
	}
}
