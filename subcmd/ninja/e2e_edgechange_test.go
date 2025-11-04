// Copyright 2025 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/hashfs"
)

func TestBuild_EdgeChange(t *testing.T) {
	ctx := t.Context()
	dir := tempDir(t)

	ninja := func(t *testing.T) (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile: ".siso_fs_state",
		})
		defer cleanup()
		return runNinja(ctx, "build.ninja", graph, opt, []string{"out"}, runNinjaOpts{})
	}

	setupFiles(t, dir, t.Name(), nil)
	fi, err := os.Stat(filepath.Join(dir, "in"))
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(filepath.Join(dir, "out/siso/build.ninja"), []byte(`
rule action
  command = python3 ../../action.py --input ${in} --output ${out}

build out: action ../../in | ../../in2
`), 0644)
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("-- first build")
	stats, err := ninja(t)
	if err != nil {
		t.Fatalf("ninja %v", err)
	}

	if stats.Done != stats.Total || stats.Total != 1 {
		t.Errorf("done=%d total=%d; want done=1 total=1; %#v", stats.Done, stats.Total, stats)
	}

	want := "in\nin2\n"
	got, err := os.ReadFile(filepath.Join(dir, "out/siso/out"))
	if err != nil {
		t.Fatalf("failed to read out: %v", err)
	}
	if strings.ReplaceAll(string(got), "\r", "") != want {
		t.Errorf("out: got=%q; want=%q", got, want)
	}

	t.Logf("-- confirm no-op")
	stats, err = ninja(t)
	if err != nil {
		t.Fatalf("ninja %v", err)
	}
	if stats.Done != stats.Total || stats.Skipped != stats.Total || stats.Total != 1 {
		t.Errorf("done=%d total=%d skipped=%d; want done=1 total=1 skipped=1; %#v", stats.Done, stats.Total, stats.Skipped, stats)
	}

	t.Logf("-- remove in2 from inputs")
	err = os.Remove(filepath.Join(dir, "in2"))
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(filepath.Join(dir, "out/siso/build.ninja"), []byte(`
rule action
  command = python3 ../../action.py --input ${in} --output ${out}

build out: action ../../in
`), 0644)
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("-- second build")
	stats, err = ninja(t)
	if err != nil {
		t.Fatalf("ninja %v", err)
	}

	if stats.Done != stats.Total || stats.Total != 1 || stats.Skipped != 0 {
		t.Errorf("done=%d total=%d skipped=%d; want done=1 total=1 skipped=0; %#v", stats.Done, stats.Total, stats.Skipped, stats)
	}

	want = "in\n"
	got, err = os.ReadFile(filepath.Join(dir, "out/siso/out"))
	if err != nil {
		t.Fatalf("failed to read out: %v", err)
	}
	if strings.ReplaceAll(string(got), "\r", "") != want {
		t.Errorf("out: got=%q; want=%q", got, want)
	}

	t.Logf("-- add in3 to inputs")
	err = os.WriteFile(filepath.Join(dir, "in3"), []byte("in3\n"), 0644)
	if err != nil {
		t.Fatal(err)
	}
	// set same mtime with in, to make in3 is older than out,
	// so step won't run as mtime order. {in,in3} < out.
	// but step will run as we add in3 to dependencies.
	err = os.Chtimes(filepath.Join(dir, "in3"), time.Time{}, fi.ModTime())
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(filepath.Join(dir, "out/siso/build.ninja"), []byte(`
rule action
  command = python3 ../../action.py --input ${in} --output ${out}

build out: action ../../in | ../../in3
`), 0644)
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("-- third build")
	stats, err = ninja(t)
	if err != nil {
		t.Fatalf("ninja %v", err)
	}

	if stats.Done != stats.Total || stats.Total != 1 || stats.Skipped != 0 {
		t.Errorf("done=%d total=%d skipped=%d; want done=1 total=1 skipped=0; %#v", stats.Done, stats.Total, stats.Skipped, stats)
	}

	want = "in\nin3\n"
	got, err = os.ReadFile(filepath.Join(dir, "out/siso/out"))
	if err != nil {
		t.Fatalf("failed to read out: %v", err)
	}
	if strings.ReplaceAll(string(got), "\r", "") != want {
		t.Errorf("out: got=%q; want=%q", got, want)
	}
}
