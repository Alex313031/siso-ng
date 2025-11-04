// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/hashfs"
)

// Test schedule for abs path correctly. b/354792946
func TestBuild_Local_AbsPath(t *testing.T) {
	ctx := t.Context()
	topdir := tempDir(t)

	dir := filepath.Join(topdir, "src")
	err := os.MkdirAll(dir, 0755)
	if err != nil {
		t.Fatal(err)
	}
	buildercacheDir := filepath.Join(topdir, "buildercache")
	buildercacheDir, err = filepath.Abs(buildercacheDir)
	if err != nil {
		t.Fatal(err)
	}
	err = os.MkdirAll(buildercacheDir, 0755)
	if err != nil {
		t.Fatal(err)
	}
	buildercacheDir = filepath.ToSlash(buildercacheDir)

	ninja := func(t *testing.T) (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile: ".siso_fs_state",
		})
		defer cleanup()
		return runNinja(ctx, "build.ninja", graph, opt, nil, runNinjaOpts{})
	}

	writeFile := func(t *testing.T, fname, content string) {
		t.Helper()
		err := os.MkdirAll(filepath.Dir(fname), 0755)
		if err != nil {
			t.Fatal(err)
		}
		err = os.WriteFile(fname, []byte(content), 0644)
		if err != nil {
			t.Fatal(err)
		}
	}

	writeFile(t, filepath.Join(dir, "build/config/siso/main.star"), `
load("@builtin//encoding.star", "json")
load("@builtin//struct.star", "module")

def __stamp(ctx, cmd):
    ctx.actions.write(cmd.outputs[0])
    ctx.actions.exit(exit_status = 0)

__handlers = {
    "stamp": __stamp,
}

def init(ctx):
    step_config = {
        "rules": [
            {
                "name": "simple/stamp",
                "action": "stamp",
                "handler": "stamp",
                "replace": True,
            },
        ],
    }
    return module(
        "config",
        step_config = json.encode(step_config),
        filegroups = {},
        handlers = __handlers,
    )
`)
	writeFile(t, filepath.Join(dir, "out/siso/build.ninja"), fmt.Sprintf(`
rule stamp
  command = touch ${out}

build gen/foo.inputdeps.stamp: stamp %s/foo.in

build all: phony gen/foo.inputdeps.stamp

build build.ninja: phony
`,
		// need to escape ":" (eps. on windows)
		strings.ReplaceAll(buildercacheDir, ":", "$:")))

	writeFile(t, filepath.Join(buildercacheDir, "foo.in"), "foo input")

	stats, err := ninja(t)
	if err != nil {
		t.Errorf("ninja %v; want nil err", err)
	}
	if stats.NoExec != 1 || stats.Done != stats.Total {
		t.Errorf("noexec=%d done=%d total=%d; want noexec=1 done=total: %#v", stats.NoExec, stats.Done, stats.Total, stats)
	}
}

func TestBuild_Local_Inputs(t *testing.T) {
	ctx := t.Context()
	dir := tempDir(t)

	ninja := func(t *testing.T) (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile: ".siso_fs_state",
		})
		defer cleanup()
		return runNinja(ctx, "build.ninja", graph, opt, nil, runNinjaOpts{})
	}
	fname := filepath.ToSlash(filepath.Join(dir, "test/input2"))
	hashfs.SetNoLazyForTest(fname)
	defer hashfs.SetNoLazyForTest()

	setupFiles(t, dir, t.Name(), nil)
	t.Logf("-- first build")
	stats, err := ninja(t)
	if err != nil {
		t.Fatalf("ninja err: %v", err)
	}
	if stats.Done != stats.Total || stats.Local != 2 || stats.Total != 3 {
		t.Errorf("done=%d total=%d local=%d; want done=total=3 local=2: %#v", stats.Done, stats.Total, stats.Local, stats)
	}

	t.Logf("-- check input file is recorded in missing_digests")
	st, err := hashfs.Load(ctx, hashfs.Option{StateFile: filepath.Join(dir, "out/siso/.siso_fs_state")})
	if err != nil {
		t.Fatalf("hashfs load err: %v", err)
	}
	if !slices.Equal(st.MissingDigests, []string{fname}) {
		t.Errorf("missing_digests=%q; want=%q", st.MissingDigests, []string{fname})
	}

	t.Logf("-- confirm no-op")
	stats, err = ninja(t)
	if err != nil {
		t.Fatalf("ninja err: %v", err)
	}
	if stats.Done != stats.Total || stats.Local != 0 || stats.Skipped != 3 || stats.Total != 3 {
		t.Errorf("done=%d total=%d skipped=%d local=%d; want done=total=skipped=3 local=0: %#v", stats.Done, stats.Total, stats.Skipped, stats.Local, stats)
	}

	touchFile(t, dir, "test/input2")
	t.Logf("-- second build")
	stats, err = ninja(t)
	if err != nil {
		t.Fatalf("ninja err: %v", err)
	}
	if stats.Done != stats.Total || stats.Local != 1 || stats.Skipped != 2 || stats.Total != 3 {
		t.Errorf("done=%d total=%d skipped=%d local=%d; want done=total=3 skipped=2 local=1: %#v", stats.Done, stats.Total, stats.Skipped, stats.Local, stats)
	}

}
