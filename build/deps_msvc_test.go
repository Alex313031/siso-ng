// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"go.chromium.org/build/siso/execute"
	"go.chromium.org/build/siso/hashfs"
)

func TestDescMSVCDepsAfterRun(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()

	hfs, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range []string{
		"out/siso/clang-cl.exe",
		"base/foo.cc",
		"base/foo.h",
		"base/bar.h",
		"v1/foo.h",
	} {
		err = hfs.WriteFile(ctx, dir, in, nil, false, time.Now(), nil, nil)
		if err != nil {
			t.Fatal(err)
		}
	}

	b := &Builder{
		path:   NewPath(dir, "out/siso"),
		hashFS: hfs,
	}
	step := &Step{
		def: fakeStepDef{},
		cmd: &execute.Cmd{
			Args: []string{"clang-cl.exe", "/showIncludes", "/TP", "../../base/foo.cc"},
			Inputs: []string{
				"out/siso/clang-cl.exe",
				"base/foo.cc",
				// These headers will be added by scandeps,
				// which may add unnecessary headers from
				// difference branch of ifdef, etc.
				"base/foo.h",
				"base/bar.h",
				"v1/foo.h",
			},
			Deps: "msvc",
		},
	}
	w := step.cmd.StdoutWriter()
	_, err = w.Write([]byte("Note: including file: ../../base/foo.h\r\n"))
	if err != nil {
		t.Fatalf("write to stdout: %v", err)
	}

	var deps depsMSVC
	got, err := deps.DepsAfterRun(ctx, b, step)
	if err != nil {
		t.Errorf("DepsAfterRun(ctx, b, step)=%q, %v; want nil err", got, err)
	}
	// v1/foo.h is workaround for b/294927170 and
	// https://github.com/llvm/llvm-project/issues/58726
	want := []string{
		"../../base/foo.h",
		"../../v1/foo.h",
		"../../base/foo.cc",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("DepsAfterRun(ctx, b, step): diff -want +got:\n%s", diff)
	}
	stdout := string(step.cmd.Stdout())
	if stdout != "" {
		t.Errorf("DepsAfterRun: stdout: %q, want empty", stdout)
	}
}
