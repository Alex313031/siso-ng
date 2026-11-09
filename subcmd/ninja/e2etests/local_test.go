// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package e2etests

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/build/ninjabuild"
	"go.chromium.org/build/siso/hashfs"
)

// Test schedule for abs path correctly. b/354792946
func TestBuild_Local_AbsPath(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
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

	runNinjaTest := func(t *testing.T) (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile: ".siso_fs_state",
		})
		defer cleanup()
		return ninjabuild.Run(ctx, graph, opt, nil, ninjabuild.RunNinjaOpts{})
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

	stats, err := runNinjaTest(t)
	if err != nil {
		t.Errorf("ninja %v; want nil err", err)
	}
	if stats.NoExec != 1 || stats.Done != stats.Total {
		t.Errorf("noexec=%d done=%d total=%d; want noexec=1 done=total: %#v", stats.NoExec, stats.Done, stats.Total, stats)
	}
}

func TestBuild_Local_Inputs(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	ctx := t.Context()
	dir := tempDir(t)

	runNinjaTest := func(t *testing.T) (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile: ".siso_fs_state",
		})
		defer cleanup()
		return ninjabuild.Run(ctx, graph, opt, nil, ninjabuild.RunNinjaOpts{})
	}
	fname := filepath.ToSlash(filepath.Join(dir, "test/input2"))
	hashfs.SetNoLazyForTest(fname)
	defer hashfs.SetNoLazyForTest()

	setupFiles(t, dir, t.Name(), nil)
	t.Logf("-- first build")
	stats, err := runNinjaTest(t)
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
	// All three source inputs land in missing_digests: mark uses
	// StatMtime (no compute queue) and these python-action inputs are
	// not reached by scandeps, so no digest runs before state save.
	want := []string{
		filepath.ToSlash(filepath.Join(dir, "test/input0")),
		filepath.ToSlash(filepath.Join(dir, "test/input1")),
		fname, // test/input2, SetNoLazyForTest'd
	}
	got := slices.Clone(st.MissingDigests)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("missing_digests=%q; want=%q", got, want)
	}

	t.Logf("-- confirm no-op")
	stats, err = runNinjaTest(t)
	if err != nil {
		t.Fatalf("ninja err: %v", err)
	}
	if stats.Done != stats.Total || stats.Local != 0 || stats.Skipped != 3 || stats.Total != 3 {
		t.Errorf("done=%d total=%d skipped=%d local=%d; want done=total=skipped=3 local=0: %#v", stats.Done, stats.Total, stats.Skipped, stats.Local, stats)
	}

	touchFile(t, dir, "test/input2")
	t.Logf("-- second build")
	stats, err = runNinjaTest(t)
	if err != nil {
		t.Fatalf("ninja err: %v", err)
	}
	if stats.Done != stats.Total || stats.Local != 1 || stats.Skipped != 2 || stats.Total != 3 {
		t.Errorf("done=%d total=%d skipped=%d local=%d; want done=total=3 skipped=2 local=1: %#v", stats.Done, stats.Total, stats.Skipped, stats.Local, stats)
	}

}

// TestBuild_Local_StaleDepsTargetCache verifies that when a step with a stale
// .siso_deps entry stats a generated header before its local generator step
// runs, the local generator step updates b.targets so downstream consumers of
// the generated header still rebuild (b/575838859).
func TestBuild_Local_StaleDepsTargetCache(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	ctx := t.Context()
	dir := tempDir(t)

	setupFiles(t, dir, t.Name(), nil)

	runNinjaTest := func(t *testing.T) (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:   ".siso_fs_state",
			OutputLocal: func(context.Context, string) bool { return true },
		})
		defer cleanup()
		// Serialize steps so scheduling order is deterministic by critical-path
		// weight: a.o (weight 3) runs before gen/foo.h (weight 2), which runs
		// before b.o (weight 1).
		opt.Limits.Step = 1
		return ninjabuild.Run(ctx, graph, opt, nil, ninjabuild.RunNinjaOpts{})
	}

	t.Logf("-- first build")
	stats, err := runNinjaTest(t)
	if err != nil {
		t.Fatalf("ninja err: %v", err)
	}
	if stats.Done != stats.Total || stats.Local != 5 || stats.Total != 6 {
		t.Fatalf("done=%d total=%d local=%d; want done=total=6 local=5: %#v", stats.Done, stats.Total, stats.Local, stats)
	}

	// Simulate an incremental change where:
	// 1. a.cc drops its include of gen/foo.h and its order-only dep on gen/foo.h,
	//    leaving a stale gen/foo.h entry in .siso_deps for a.o.
	// 2. foo.in is modified so gen/foo.h is regenerated locally.
	// 3. b.o still depends on gen/foo.h and must be rebuilt.
	modifyFile(t, dir, "foo.in", func([]byte) []byte {
		return []byte("// foo v2\n")
	})
	modifyFile(t, dir, "a.cc", func([]byte) []byte {
		return []byte("// a v2 (no longer includes gen/foo.h)\n")
	})
	modifyFile(t, dir, "out/siso/build.ninja", func(buf []byte) []byte {
		return bytes.ReplaceAll(buf,
			[]byte("build a.o: cxx ../../a.cc || gen/foo.h"),
			[]byte("build a.o: cxx ../../a.cc"),
		)
	})

	t.Logf("-- second build")
	stats, err = runNinjaTest(t)
	if err != nil {
		t.Fatalf("ninja err: %v", err)
	}
	if stats.Done != stats.Total || stats.Local != 5 || stats.Total != 6 {
		t.Errorf("second build: done=%d total=%d local=%d skipped=%d; want done=total=6 local=5 skipped=1: %#v", stats.Done, stats.Total, stats.Local, stats.Skipped, stats)
	}

	t.Logf("-- confirm no-op after second build")
	stats, err = runNinjaTest(t)
	if err != nil {
		t.Fatalf("ninja err: %v", err)
	}
	if stats.Done != stats.Total || stats.Skipped != 6 || stats.Local != 0 {
		t.Errorf("confirm no-op: done=%d total=%d skipped=%d local=%d; want done=total=skipped=6 local=0: %#v", stats.Done, stats.Total, stats.Skipped, stats.Local, stats)
	}
}
