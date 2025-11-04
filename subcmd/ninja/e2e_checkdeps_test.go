// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/hashfs"
)

func TestBuild_CheckDeps(t *testing.T) {
	ctx := t.Context()
	dir := tempDir(t)

	ninja := func(t *testing.T, target string, w io.Writer) (build.Stats, error) {
		t.Helper()
		build.SetExperimentForTest("check-deps")
		defer func() {
			build.SetExperimentForTest("")
		}()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile: ".siso_fs_state",
		})
		defer cleanup()
		opt.OutputLogWriter = w
		return runNinja(ctx, "build.ninja", graph, opt, []string{target}, runNinjaOpts{})
	}

	setupFiles(t, dir, t.Name(), nil)
	var sisoOutput bytes.Buffer
	t.Logf("-- first build bar.h")
	stats, err := ninja(t, "bar.h", &sisoOutput)
	if err != nil {
		t.Fatalf("ninja err: %v", err)
	}
	if stats.Done != stats.Total || stats.Total != 1 {
		t.Errorf("done=%d total=%d; want done=total=1", stats.Done, stats.Total)
	}

	t.Logf("-- second build all")
	stats, err = ninja(t, "all", &sisoOutput)
	if err != nil {
		t.Fatalf("ninja err: %v", err)
	}
	if stats.Done != stats.Total || stats.Total != 5 {
		t.Errorf("done=%d total=%d; want done=total=4", stats.Done, stats.Total)
	}

	t.Logf("siso_output:\n%s", sisoOutput.String())

	want := `deps error: deps inputs have no dependencies from "./obj/foo.o" to ["bar.h"]`
	if !strings.Contains(sisoOutput.String(), want) {
		t.Errorf("unexpected siso_output\ngot:\n%s\nwant contains\n%s", sisoOutput.String(), want)
	}
}
