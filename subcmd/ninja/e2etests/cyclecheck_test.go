// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package e2etests

import (
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/build/ninjabuild"
	"go.chromium.org/build/siso/hashfs"
)

func TestBuild_CycleCheck(t *testing.T) {
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

	setupFiles(t, dir, t.Name(), nil)

	_, err := runNinjaTest(t)
	if err == nil {
		t.Fatalf("ninja %v; want error", err)
	}
	t.Logf("ninja %v", err)
	cycleErr, ok := errors.AsType[build.DependencyCycleError](err)
	if !ok {
		t.Fatalf("err type %T; want %T", err, build.DependencyCycleError{})
	}
	want := build.DependencyCycleError{
		Targets: []string{"gen/foo.txt", "gen/foo.txt"},
	}
	if diff := cmp.Diff(want, cycleErr); diff != "" {
		t.Errorf("diff (-want +got):\n%s", diff)
	}
}
