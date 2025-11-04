// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjabuild

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/google/go-cmp/cmp"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/toolsupport/ninjautil"
)

func TestIndirectInputsFilter(t *testing.T) {
	for _, tc := range []struct {
		name       string
		ii         *IndirectInputs
		matches    []string
		nonmatches []string
	}{
		{
			name: "starAllMatch",
			ii: &IndirectInputs{
				Includes: []string{"*"},
			},
			matches: []string{
				"foo",
				"foo/bar",
			},
		},
		{
			name: "suffixMatch",
			ii: &IndirectInputs{
				Includes: []string{"*.json", "*.ts"},
			},
			matches: []string{
				"foo.json",
				"foo/bar.json",
				"foo.ts",
				"foo/bar.ts",
			},
			nonmatches: []string{
				"foo.js",
				"foo/bar.js",
				"foo.json/bar",
			},
		},
		{
			name: "baseMatch",
			ii: &IndirectInputs{
				Includes: []string{"foo-?.txt"},
			},
			matches: []string{
				"foo-1.txt",
				"bar/foo-1.txt",
			},
			nonmatches: []string{
				"foo-11.txt",
				"foo-1.txt/bar",
			},
		},
		{
			name: "pathMatch",
			ii: &IndirectInputs{
				Includes: []string{"foo/bar-?.txt"},
			},
			matches: []string{
				"foo/bar-1.txt",
				"foo/bar-2.txt",
			},
			nonmatches: []string{
				"foo/bar-11.txt",
				"x/foo/bar-1.txt",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			f := tc.ii.filter(ctx)
			for _, p := range tc.matches {
				if !f(ctx, p, false) {
					t.Errorf("f(ctx, %q, false)=false; want=true", p)
				}
			}
			for _, p := range tc.nonmatches {
				if f(ctx, p, false) {
					t.Errorf("f(ctx, %q, false)=true; want=false", p)
				}
			}
		})
	}
}

func TestStepConfigExpandInputs(t *testing.T) {
	ctx := t.Context()
	tdir := t.TempDir()
	err := os.MkdirAll(tdir, 0755)
	if err != nil {
		t.Fatal(err)
	}
	for _, fname := range []string{
		"out/Default/gen/out",
		"out/Default/gen/libfoo.so",
		"foo/bar",
		"foo/baz",
		"base/base.h",
		"component/a/1",
		"component/a/2",
		"component/b",
	} {
		pathname := filepath.Join(tdir, fname)
		err := os.MkdirAll(filepath.Dir(pathname), 0755)
		if err != nil {
			t.Fatal(err)
		}
		err = os.WriteFile(pathname, nil, 0644)
		if err != nil {
			t.Fatal(err)
		}
	}

	p := build.NewPath(tdir, "out/Default")
	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatalf("hashfs.New(...)=_, %v; want _, nil", err)
	}
	sc := StepConfig{
		InputDeps: map[string][]string{
			"./gen/out": {
				"./gen/libfoo.so",
			},
			"foo/bar": {
				"foo/baz",
				"base/base.h",
				"base/extra",
			},
			"component:component": {
				"component/a:a",
				"component/b",
			},
			"component/a:a": {
				"component/a/1",
				"component/a/2",
			},
		},
	}

	got := sc.ExpandInputs(ctx, p, hashFS, []string{
		"foo/bar",
		"out/Default/gen/out",
		"component:component",
		"extra/file",
	})

	want := []string{
		"base/base.h",
		"component/a/1",
		"component/a/2",
		"component/b",
		"foo/bar",
		"foo/baz",
		"out/Default/gen/libfoo.so",
		"out/Default/gen/out",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("sc.ExpandInputs(...); diff -want +got:\n%s", diff)
	}
}

func TestStepConfigLookup_WinPath(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip(`this test only works for windows: require filepath.IsAbs("c:/") == true`)
		return
	}
	ctx := t.Context()
	dir := t.TempDir()
	path := build.NewPath(dir, "out/siso")
	err := os.MkdirAll(filepath.Join(dir, "out/siso"), 0755)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(filepath.Join(dir, "out/siso/build.ninja"), []byte(`
rule cxx
  command = c:/b/s/w/ir/cipd_bin_packages/cpython3/bin/python3.exe ../../build/toolchain/clang_code_coverage_wrapper.py clang-cl.exe -c ${in} -o ${out}

build obj/foo.o: cxx ../../foo.cc
build all: phony obj/foo.o

build build.ninja: phony
`), 0644)
	if err != nil {
		t.Fatal(err)
	}
	state := ninjautil.NewState()
	p := ninjautil.NewManifestParser(state)
	err = p.Load(ctx, filepath.Join(dir, "out/siso/build.ninja"))
	if err != nil {
		t.Fatal(err)
	}

	node, ok := state.LookupNodeByPath("obj/foo.o")
	if !ok {
		t.Errorf("obj/foo.o not found in build.ninja")
	}
	edge, ok := node.InEdge()
	if !ok {
		t.Errorf("no inEdge for obj/foo.o")
	}

	sc := StepConfig{
		Rules: []*StepRule{
			{
				Name:          "clang-coverage/cxx",
				CommandPrefix: "python3.exe ../../build/toolchain/clang_code_coverage_wrapper.py",
				Remote:        true,
			},
		},
	}
	rule, ok := sc.Lookup(ctx, path, edge)
	if !ok || !rule.Remote {
		t.Errorf("Lookup(ctx, path, edge)=%v, %v; want (rule.Remote, true)", rule, ok)
	}
}
