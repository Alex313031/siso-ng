// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package e2etests

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/build/ninjabuild"
	"go.chromium.org/build/siso/hashfs"
)

func TestBuild_Landlock_Basic(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	skipUnlessLandlockUsable(t)
	ctx := t.Context()
	dir := tempDir(t)

	setupFiles(t, dir, t.Name(), nil)

	hashfsOpts := hashfs.Option{
		StateFile: ".siso_fs_state",
	}

	t.Logf("-- first build")
	func() {
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfsOpts)
		defer cleanup()
		var metricsBuffer syncBuffer
		opt.MetricsJSONWriter = &metricsBuffer

		stats, err := ninjabuild.Run(ctx, graph, opt, nil, ninjabuild.RunNinjaOpts{})
		if err != nil {
			t.Fatalf("ninjabuild.Run failed: %v", err)
		}
		if stats.Done != 1 || stats.Local != 1 || stats.Total != 1 {
			t.Errorf("stats: done=%d local=%d total=%d; want done=1 local=1 total=1", stats.Done, stats.Local, stats.Total)
		}
		got := readFile(t, filepath.Join(dir, "out/siso/out.txt"))
		if got != "hello landlock\n" {
			t.Errorf("out.txt = %q; want %q", got, "hello landlock\n")
		}

		dec := json.NewDecoder(bytes.NewReader(metricsBuffer.buf.Bytes()))
		foundMetric := false
		for dec.More() {
			var m build.StepMetric
			if err := dec.Decode(&m); err != nil {
				t.Fatalf("decode metric: %v", err)
			}
			if m.StepID == "" {
				continue
			}
			if filepath.Base(m.Output()) == "out.txt" {
				foundMetric = true
				if !m.Sandbox {
					t.Errorf("m.Sandbox = false; want true")
				}
				if m.Err {
					t.Errorf("m.Err = true; want false")
				}
			}
		}
		if !foundMetric {
			t.Errorf("missing metric for out.txt")
		}
	}()

	t.Logf("-- second build (no-op)")
	func() {
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfsOpts)
		defer cleanup()

		stats, err := ninjabuild.Run(ctx, graph, opt, nil, ninjabuild.RunNinjaOpts{})
		if err != nil {
			t.Fatalf("ninjabuild.Run failed on second build: %v", err)
		}
		if stats.Skipped != 1 {
			t.Errorf("stats.Skipped = %d; want 1", stats.Skipped)
		}
	}()
}

func TestBuild_Landlock_DenyUndeclaredRead(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	skipUnlessLandlockUsable(t)
	ctx := t.Context()
	dir := tempDir(t)

	setupFiles(t, dir, t.Name(), nil)

	opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
		StateFile: ".siso_fs_state",
	})
	defer cleanup()

	t.Logf("-- attempting to read undeclared file under landlock sandbox")
	_, err := ninjabuild.Run(ctx, graph, opt, nil, ninjabuild.RunNinjaOpts{})
	if err == nil {
		t.Fatalf("ninjabuild.Run succeeded; want error due to undeclared read restriction")
	}

	if _, statErr := os.Stat(filepath.Join(dir, "out/siso/out.txt")); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("out.txt exists but should not have been created on failed action")
	}
}

func TestBuild_Landlock_DenyUndeclaredWrite(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	skipUnlessLandlockUsable(t)
	ctx := t.Context()
	dir := tempDir(t)

	setupFiles(t, dir, t.Name(), nil)

	opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
		StateFile: ".siso_fs_state",
	})
	defer cleanup()

	t.Logf("-- attempting to write outside output directory under landlock sandbox")
	_, err := ninjabuild.Run(ctx, graph, opt, nil, ninjabuild.RunNinjaOpts{})
	if err == nil {
		t.Fatalf("ninjabuild.Run succeeded; want error due to unauthorized write restriction")
	}

	unauthorizedPath := filepath.Join(dir, "unauthorized.txt")
	if _, statErr := os.Stat(unauthorizedPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("%s exists; write outside sandbox should have been blocked", unauthorizedPath)
	}
}

func TestBuild_Landlock_Depfile(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	skipUnlessLandlockUsable(t)
	ctx := t.Context()
	dir := tempDir(t)

	setupFiles(t, dir, t.Name(), nil)

	opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
		StateFile: ".siso_fs_state",
	})
	defer cleanup()

	t.Logf("-- attempting to build with landlock depfile restriction active")
	stats, err := ninjabuild.Run(ctx, graph, opt, nil, ninjabuild.RunNinjaOpts{})

	expectedErr := build.DepsError{UnsandboxedInputs: []string{"../../undeclared.h"}}
	if !errors.Is(err, expectedErr) {
		t.Errorf("got error %v, want %v", err, expectedErr)
	}

	if stats.Done != 2 || stats.Local != 1 || stats.Total != 2 {
		t.Errorf("done=%d total=%d local=%d; want done=2 total=2 local=1 %#v", stats.Done, stats.Total, stats.Local, stats)
	}
}
