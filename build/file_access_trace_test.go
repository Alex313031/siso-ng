// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"go.chromium.org/build/siso/hashfs"
)

// TestFilesDiff_DirTargetCoversDescendants verifies filesDiff treats files
// traced under a declared directory artifact as expected, not as undeclared
// extras. A directory is declared as one unit but the syscall trace reports
// each descendant; without directory awareness every descendant becomes an add
// and the directory a del, marking a pure directory-I/O step impure.
func TestFilesDiff_DirTargetCoversDescendants(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	hfs, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}
	defer hfs.Close(ctx)
	if err := hfs.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}

	// Traced paths are workdir-relative (as strace reports them); declared
	// targets are workspace-relative (as cmd.AllOutputs/AllInputs yield them).
	for _, rel := range []string{
		"out/siso/gen/extracted/a.h",
		"out/siso/gen/extracted/sub/b.h",
		"out/siso/other/stray.h",
	} {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	b := &Builder{hashFS: hfs, path: NewPath(dir, "out/siso")}
	traced := []string{
		"gen/extracted/a.h",
		"gen/extracted/sub/b.h",
	}

	// A directory output is recorded slash-stripped (cmd.OutputDirs); a
	// directory input keeps its trailing slash (cmd.Inputs). Both must cover
	// their descendants.
	for _, tc := range []struct {
		name     string
		declared []string
	}{
		{name: "output_slash_stripped", declared: []string{"out/siso/gen/extracted"}},
		{name: "input_trailing_slash", declared: []string{"out/siso/gen/extracted/"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			adds, dels, _, errs := filesDiff(ctx, b, tc.declared, nil, traced, "")
			if len(errs) != 0 {
				t.Fatalf("filesDiff errs: %v", errs)
			}
			if len(adds) != 0 {
				t.Errorf("adds = %v; want none (descendants of a declared directory are expected, not extra)", adds)
			}
			if len(dels) != 0 {
				t.Errorf("dels = %v; want none (the declared directory is covered by its traced descendants)", dels)
			}
		})
	}

	// An unrelated traced file outside any declared target must still be
	// flagged, so the directory-coverage fix does not suppress real findings.
	t.Run("unrelated_file_still_flagged", func(t *testing.T) {
		adds, _, _, errs := filesDiff(ctx, b, []string{"out/siso/gen/extracted"}, nil,
			append(slices.Clone(traced), "other/stray.h"), "")
		if len(errs) != 0 {
			t.Fatalf("filesDiff errs: %v", errs)
		}
		if !slices.Contains(adds, "out/siso/other/stray.h") {
			t.Errorf("adds = %v; want it to include out/siso/other/stray.h (a genuinely undeclared access)", adds)
		}
	})
}
