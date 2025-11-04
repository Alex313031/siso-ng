// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	"go.chromium.org/build/siso/execute"
	"go.chromium.org/build/siso/hashfs"
)

func TestDepsExpandInputs(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()

	hfs, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}
	defer hfs.Close(ctx)

	setupFile := func(fname string) {
		t.Helper()
		fullpath := filepath.Join(dir, fname)
		err := os.MkdirAll(filepath.Dir(fullpath), 0755)
		if err != nil {
			t.Fatal(err)
		}
		err = os.WriteFile(fullpath, nil, 0644)
		if err != nil {
			t.Fatal(err)
		}
	}
	setupFile("components/file1")
	setupFile("components/file2")

	b := &Builder{
		hashFS: hfs,
		path:   NewPath(dir, "out/siso"),
	}
	step := &Step{
		def: &fakeStepDef{
			expandedInputs: func(context.Context) []string {
				return []string{
					"components/file1",
					"components/file2",
				}
			},
		},
		cmd: &execute.Cmd{
			Inputs: []string{
				"components:label",
				"non-existing-file",
			},
		},
	}
	depsExpandInputs(ctx, b, step)

	// expanded and no label and no existing file
	want := []string{
		"components/file1",
		"components/file2",
	}
	if diff := cmp.Diff(step.cmd.Inputs, want); diff != "" {
		t.Errorf("depsExpandInputs: diff -want +got:\n%s", diff)
	}
}
