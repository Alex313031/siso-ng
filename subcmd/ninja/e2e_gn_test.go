// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"testing"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/hashfs"
)

// Test rebuild build.ninja (gn gen) behavior.
func TestBuild_GNGen(t *testing.T) {
	ctx := t.Context()
	dir := tempDir(t)

	run := func(args ...string) error {
		cmd := exec.CommandContext(ctx, "python3", args...)
		cmd.Dir = dir
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}

	runBuild := func(t *testing.T, s string) (build.Stats, []build.StepMetric, error) {
		t.Helper()
		t.Logf("build - %s", s)
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile: ".siso_fs_state",
		})
		defer cleanup()

		manifestOpt := opt
		manifestOpt.Clobber = false
		manifestOpt.RebuildManifest = "build.ninja"
		manifestBuild, err := build.New(ctx, graph, manifestOpt)
		if err != nil {
			t.Fatal(err)
		}
		err = manifestBuild.Build(ctx, "rebuild manifest", "build.ninja")
		manifestBuild.Close()
		stats := manifestBuild.Stats()
		if errors.Is(err, build.ErrManifestModified) {
			return stats, nil, err
		}
		if err != nil {
			t.Fatal(err)
		}

		var metricsBuffer syncBuffer
		opt.MetricsJSONWriter = &metricsBuffer

		b, err := build.New(ctx, graph, opt)
		if err != nil {
			t.Fatal(err)
		}
		defer b.Close()
		err = b.Build(ctx, "build", "all")
		stats = b.Stats()
		var metrics []build.StepMetric
		dec := json.NewDecoder(bytes.NewReader(metricsBuffer.buf.Bytes()))
		for dec.More() {
			var m build.StepMetric
			derr := dec.Decode(&m)
			if derr != nil {
				t.Errorf("decode %v", derr)
			}
			metrics = append(metrics, m)
		}
		return stats, metrics, err
	}

	testName := t.Name()
	const nsteps = 2

	t.Run("rebuild", func(t *testing.T) {
		setupFiles(t, dir, testName, nil)
		err := run("buildtools/gn.py", "gen", "out/siso")
		if err != nil {
			t.Fatalf("gn gen failed: %v", err)
		}

		stats, _, err := runBuild(t, "first")
		if err != nil {
			t.Fatalf("first build=%v; want nil err", err)
		}
		if stats.Total != nsteps {
			t.Errorf("first build Total=%d want=%d", stats.Total, nsteps)
		}
		if stats.Done != nsteps {
			t.Errorf("first build Done=%d want=%d", stats.Done, nsteps)
		}
		if stats.Skipped != 1 {
			t.Errorf("first build Skipped=%d want=1", stats.Skipped)
		}
		stats, _, err = runBuild(t, "null")
		if err != nil {
			t.Fatalf("null build=%v; want nil err", err)
		}
		if stats.Total != nsteps {
			t.Errorf("null build Total=%d want=%d", stats.Total, nsteps)
		}
		if stats.Done != nsteps {
			t.Errorf("null build Done=%d want=%d", stats.Done, nsteps)
		}
		if stats.Skipped != nsteps {
			t.Errorf("null build Skipped=%d want=%d", stats.Skipped, nsteps)
		}

		modifyFile(t, dir, "BUILD.gn", func(buf []byte) []byte {
			return append(buf, []byte("!!!")...)
		})

		stats, _, err = runBuild(t, "incremental-regen")
		if !errors.Is(err, build.ErrManifestModified) {
			t.Fatalf("regen build=%v; want %v", err, build.ErrManifestModified)
		}

		stats, _, err = runBuild(t, "incremental-after-regen")
		if err != nil {
			t.Fatalf("incremental build after regen %v, want nil err", err)
		}
		if stats.Total != nsteps {
			t.Errorf("incremental build Total=%d want=%d", stats.Total, nsteps)
		}
		if stats.Done != nsteps {
			t.Errorf("incremental build Done=%d want=%d", stats.Done, nsteps)
		}
		if stats.Skipped != nsteps {
			t.Errorf("incremental build Skipped=%d want=%d", stats.Skipped, nsteps)
		}
	})

	t.Run("clean", func(t *testing.T) {
		setupFiles(t, dir, testName, nil)
		err := run("buildtools/gn.py", "gen", "out/siso")
		if err != nil {
			t.Fatalf("gn gen failed: %v", err)
		}

		err = run("buildtools/gn.py", "clean", "out/siso")
		if err != nil {
			t.Fatalf("gn clean failed: %v", err)
		}

		stats, _, err := runBuild(t, "clean-regen")
		if !errors.Is(err, build.ErrManifestModified) {
			t.Fatalf("clean build=%v; want %v", err, build.ErrManifestModified)
		}

		stats, _, err = runBuild(t, "clean-after-regen")
		if err != nil {
			t.Fatalf("clean build after regen %v, want nil err", err)
		}
		if stats.Total != nsteps {
			t.Errorf("clean build Total=%d want=%d", stats.Total, nsteps)
		}
		if stats.Done != nsteps {
			t.Errorf("clean build Done=%d want=%d", stats.Done, nsteps)
		}
		if stats.Skipped != 1 {
			t.Errorf("clean build Skipped=%d want=1", stats.Skipped)
		}
	})
	t.Run("failure_regen", func(t *testing.T) {
		ninja := func(lastFailedTargets []string) (build.Stats, error) {
			opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
				StateFile: ".siso_fs_state",
			})
			defer cleanup()
			opt.LastFailureTargets = lastFailedTargets
			return runNinja(ctx, "build.ninja", graph, opt, nil, runNinjaOpts{})
		}
		setupFiles(t, dir, testName, nil)
		err := run("buildtools/gn.py", "gen", "out/siso")
		if err != nil {
			t.Fatalf("gn gen failed: %v", err)
		}

		stats, err := ninja(nil)
		if err != nil {
			t.Fatalf("first build  %v, want nil err", err)
		}

		modifyFile(t, dir, "BUILD.gn", func(buf []byte) []byte {
			return append(buf, []byte("!!!")...)
		})

		stats, err = ninja([]string{"base.stamp"})
		if err != nil {
			t.Errorf("build err: %v", err)
		}
		if stats.Total != nsteps {
			t.Errorf("fix build Total=%d want=%d", stats.Total, nsteps)
		}
		if stats.Done != nsteps {
			t.Errorf("fix build Done=%d want=%d", stats.Done, nsteps)
		}
		if stats.Skipped != nsteps {
			t.Errorf("fix build Skipped=%d want=%d", stats.Skipped, nsteps)
		}
	})

	t.Run("failure_missing_targets", func(t *testing.T) {
		ninja := func(lastFailedTargets []string) (build.Stats, error) {
			opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
				StateFile: ".siso_fs_state",
			})
			defer cleanup()
			opt.LastFailureTargets = lastFailedTargets
			return runNinja(ctx, "build.ninja", graph, opt, nil, runNinjaOpts{})
		}
		setupFiles(t, dir, testName, nil)
		err := run("buildtools/gn.py", "gen", "out/siso")
		if err != nil {
			t.Fatalf("gn gen failed: %v", err)
		}

		stats, err := ninja(nil)
		if err != nil {
			t.Fatalf("first build  %v, want nil err", err)
		}
		modifyFile(t, dir, "BUILD.gn", func(buf []byte) []byte {
			return append(buf, []byte("!!!")...)
		})

		// mark deprecated.stamp failed to simulate missing last failed targets in next build
		stats, err = ninja([]string{"deprecated.stamp"})
		if err != nil {
			t.Errorf("build err: %v", err)
		}
		if stats.Total != nsteps {
			t.Errorf("fix build Total=%d want=%d", stats.Total, nsteps)
		}
		if stats.Done != nsteps {
			t.Errorf("fix build Done=%d want=%d", stats.Done, nsteps)
		}
		if stats.Skipped != nsteps {
			t.Errorf("fix build Skipped=%d want=%d", stats.Skipped, nsteps)
		}
	})
}
