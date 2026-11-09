// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package e2etests

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/build/ninjabuild"
	"go.chromium.org/build/siso/hashfs"
)

func TestBuild_MissingDepsFatalNoFallback(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	ctx := t.Context()
	dir := tempDir(t)

	runNinjaTest := func(t *testing.T, target string, w io.Writer) (build.Stats, error) {
		t.Helper()
		build.SetExperimentForTest("no-fallback")
		defer func() {
			build.SetExperimentForTest("")
		}()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile: ".siso_fs_state",
		})
		defer cleanup()
		opt.OutputLogWriter = w
		opt.MissingDeps = build.MissingDepsFatal
		return ninjabuild.Run(ctx, graph, opt, []string{target}, ninjabuild.RunNinjaOpts{})
	}

	setupFiles(t, dir, "TestBuild_CheckDeps", nil)
	var sisoOutput bytes.Buffer
	t.Logf("-- first build bar.h")
	stats, err := runNinjaTest(t, "bar.h", &sisoOutput)
	if err != nil {
		t.Fatalf("ninja err: %v", err)
	}
	if stats.Done != stats.Total || stats.Total != 1 {
		t.Errorf("done=%d total=%d; want done=total=1", stats.Done, stats.Total)
	}

	t.Logf("-- second build all with -missing_deps=fatal and no-fallback")
	sisoOutput.Reset()
	_, err = runNinjaTest(t, "all", &sisoOutput)
	if err == nil {
		t.Fatalf("ninja err: got nil, want error")
	}

	if _, ok := errors.AsType[build.DepsError](err); !ok {
		t.Errorf("got error %T: %v, want build.DepsError", err, err)
	}
	if _, ok := errors.AsType[build.TooManyFallbackError](err); ok {
		t.Errorf("unexpected TooManyFallbackError: %v", err)
	}
}

func TestBuild_MissingDepsMode(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}

	runTest := func(ctx context.Context, t *testing.T, mode build.MissingDepsMode) (string, error) {
		t.Helper()
		dir := tempDir(t)
		setupFiles(t, dir, "TestBuild_CheckDeps", nil)

		runNinja := func(target string, w io.Writer) error {
			opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
				StateFile: ".siso_fs_state",
			})
			defer cleanup()
			opt.OutputLogWriter = w
			opt.MissingDeps = mode
			_, err := ninjabuild.Run(ctx, graph, opt, []string{target}, ninjabuild.RunNinjaOpts{})
			return err
		}

		var sisoOutput bytes.Buffer
		err := runNinja("bar.h", &sisoOutput)
		if err != nil {
			t.Fatalf("first build bar.h failed: %v", err)
		}
		sisoOutput.Reset()
		err = runNinja("all", &sisoOutput)
		return sisoOutput.String(), err
	}

	badDepWarnMsg := `missing deps warn: deps inputs have no dependencies from "./obj/foo.o" to ["bar.h"]`
	badDepErrorMsg := `missing deps error: deps inputs have no dependencies from "./obj/foo.o" to ["bar.h"]`

	for _, tc := range []struct {
		name       string
		mode       build.MissingDepsMode
		checkErr   func(t *testing.T, err error)
		wantOutput string
		wantNoWarn bool
	}{
		{
			name: "ignore",
			mode: build.MissingDepsIgnore,
			checkErr: func(t *testing.T, err error) {
				t.Helper()
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			},
			wantNoWarn: true,
		},
		{
			name: "warn",
			mode: build.MissingDepsWarn,
			checkErr: func(t *testing.T, err error) {
				t.Helper()
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			},
			wantOutput: badDepWarnMsg,
		},
		{
			name: "error",
			mode: build.MissingDepsError,
			checkErr: func(t *testing.T, err error) {
				t.Helper()
				if !errors.Is(err, build.ErrMissingDepsViolation) {
					t.Errorf("got error %v, want errors.Is %v", err, build.ErrMissingDepsViolation)
				}
			},
			wantOutput: badDepErrorMsg,
		},
		{
			name: "fatal",
			mode: build.MissingDepsFatal,
			checkErr: func(t *testing.T, err error) {
				t.Helper()
				if _, ok := errors.AsType[build.DepsError](err); !ok {
					t.Errorf("got error %T: %v, want build.DepsError", err, err)
				}
				if !errors.Is(err, build.ErrMissingDepsViolation) {
					t.Errorf("got error %v, want errors.Is %v", err, build.ErrMissingDepsViolation)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runTest(t.Context(), t, tc.mode)
			tc.checkErr(t, err)
			if tc.wantNoWarn && strings.Contains(out, badDepWarnMsg) {
				t.Errorf("output contains bad dep warning, want none:\n%s", out)
			}
			if tc.wantOutput != "" && !strings.Contains(out, tc.wantOutput) {
				t.Errorf("output does not contain %q in mode %s:\n%s", tc.wantOutput, tc.name, out)
			}
		})
	}
}
