// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/hashfs"
)

func TestBuild_CycleCheck(t *testing.T) {
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

	setupFiles(t, dir, t.Name(), nil)

	_, err := ninja(t)
	if err == nil {
		t.Fatalf("ninja %v; want error", err)
	}
	t.Logf("ninja %v", err)
	var cycleErr build.DependencyCycleError
	if !errors.As(err, &cycleErr) {
		t.Fatalf("err type %T; want %T", err, cycleErr)
	}
	want := build.DependencyCycleError{
		Targets: []string{"out/siso/gen/foo.txt", "out/siso/gen/foo.txt"},
	}
	if diff := cmp.Diff(want, cycleErr); diff != "" {
		t.Errorf("diff (-want +got):\n%s", diff)
	}
}
