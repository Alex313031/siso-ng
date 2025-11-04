// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjabuild

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/google/go-cmp/cmp"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/toolsupport/ninjautil"
)

func TestStepExpandLabels(t *testing.T) {
	ctx := t.Context()
	g := &globals{
		path: build.NewPath("/b/w", "out/Default"),
		stepConfig: &StepConfig{
			InputDeps: map[string][]string{
				"component:component": {
					"component/a:a",
					"component/b",
				},
				"component/a:a": {
					"component/a/1",
					"component/a/2",
				},
			},
		},
	}
	s := &StepDef{
		globals: g,
	}

	got := s.expandLabels(ctx, []string{
		"foo/bar",
		"component:component",
	})
	want := []string{
		"foo/bar",
		"component/b",
		"component/a/1",
		"component/a/2",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("s.expandLabels(...): diff -want +got:\n%s", diff)
	}
}

func TestExpandedInputs_no_expansion(t *testing.T) {
	ctx := t.Context()
	state := ninjautil.NewState()
	p := ninjautil.NewManifestParser(state)
	dir := t.TempDir()
	fname := filepath.Join(dir, "build.ninja")
	err := os.WriteFile(fname, []byte(`
rule __rule
  command = ....
build target3: __rule ../../source3
build target2: __rule ../../source2
build target1: __rule target2 | ../../source1 || target3
`), 0644)
	if err != nil {
		t.Fatal(err)
	}
	err = p.Load(ctx, fname)
	if err != nil {
		t.Fatal(err)
	}

	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}

	setupFile := func(fname string) {
		fullpath := filepath.Join(dir, fname)
		err := os.MkdirAll(filepath.Dir(fullpath), 0755)
		if err != nil {
			t.Fatalf("MkdirAll(%q)=%v", fname, err)
		}
		err = os.WriteFile(fullpath, nil, 0644)
		if err != nil {
			t.Fatalf("WriteFile(%q)=%v", fname, err)
		}
	}
	setupFile("source3")
	setupFile("source2")
	setupFile("source1")
	setupFile("source0")

	graph := &Graph{
		visited: make(map[*ninjautil.Edge]*build.Edge),
		globals: &globals{
			nstate: state,
			path:   build.NewPath(dir, "out/Default"),
			hashFS: hashFS,
			stepConfig: &StepConfig{
				Rules: []*StepRule{
					{
						Name:       "rule1",
						ActionName: "__rule",
						ActionOuts: []string{"./target1"},
						Inputs:     []string{"source0"},
					},
				},
			},
			targetPaths: make([]string, state.NumNodes()),
			edgeRules:   make([]edgeRuleHolder, state.NumNodes()),
		},
	}
	err = graph.globals.stepConfig.Init(ctx)
	if err != nil {
		t.Fatal(err)
	}
	newStepDef := func(target string) *StepDef {
		node, ok := state.LookupNodeByPath(target)
		if !ok {
			t.Fatalf("target %q not found in build.ninja", target)
		}
		edge, ok := node.InEdge()
		if !ok {
			t.Fatalf("target %q has no edge", target)
		}
		s := graph.newStepDef(ctx, edge, nil)
		s.EnsureRule(ctx)
		return s
	}
	for _, target := range []string{
		"target3",
		"target2",
	} {
		setupFile(filepath.Join("out/Default", target))
		if newStepDef(target) == nil {
			t.Fatalf("stepDef for %q is nil?", target)
		}
	}
	s := newStepDef("target1")
	got := s.Inputs(ctx)
	want := []string{"out/Default/target2", "source1", "out/Default/target3", "source0"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Inputs: diff -want +got:\n%s", diff)
	}
	got = s.ExpandedInputs(ctx)
	sort.Strings(got)
	sort.Strings(want)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ExpandedInputs: diff -want +got:\n%s", diff)
	}
}

func TestExpandedInputs_replace_accumulate(t *testing.T) {
	ctx := t.Context()
	state := ninjautil.NewState()
	p := ninjautil.NewManifestParser(state)
	dir := t.TempDir()
	fname := filepath.Join(dir, "build.ninja")
	err := os.WriteFile(fname, []byte(`
rule __rule
  command = ....
rule stamp
  command = ....
rule archive
  command = ....

build foo.a: archive ../../source4
build bar.stamp: stamp ../../source3
build foo.stamp: stamp ../../source2 bar.stamp
build target1: __rule ../../source1 foo.stamp foo.a
`), 0644)
	if err != nil {
		t.Fatal(err)
	}
	err = p.Load(ctx, fname)
	if err != nil {
		t.Fatal(err)
	}

	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}

	setupFile := func(fname string) {
		fullpath := filepath.Join(dir, fname)
		err := os.MkdirAll(filepath.Dir(fullpath), 0755)
		if err != nil {
			t.Fatalf("MkdirAll(%q)=%v", fname, err)
		}
		err = os.WriteFile(fullpath, nil, 0644)
		if err != nil {
			t.Fatalf("WriteFile(%q)=%v", fname, err)
		}
	}
	setupFile("source4")
	setupFile("source3")
	setupFile("source2")
	setupFile("source1")
	setupFile("source0")

	graph := &Graph{
		visited: make(map[*ninjautil.Edge]*build.Edge),
		globals: &globals{
			nstate: state,
			path:   build.NewPath(dir, "out/Default"),
			hashFS: hashFS,
			stepConfig: &StepConfig{
				Rules: []*StepRule{
					{
						Name:       "rule1",
						ActionName: "__rule",
						ActionOuts: []string{"./target1"},
						Inputs:     []string{"source0"},
					},
					{
						Name:       "stamp",
						ActionName: "stamp",
						Replace:    true,
					},
					{
						Name:       "archive",
						ActionName: "archive",
						Accumulate: true,
					},
				},
			},
			targetPaths: make([]string, state.NumNodes()),
			edgeRules:   make([]edgeRuleHolder, state.NumNodes()),
		},
	}
	err = graph.globals.stepConfig.Init(ctx)
	if err != nil {
		t.Fatal(err)
	}
	newStepDef := func(target string) *StepDef {
		node, ok := state.LookupNodeByPath(target)
		if !ok {
			t.Fatalf("target %q not found in build.ninja", target)
		}
		edge, ok := node.InEdge()
		if !ok {
			t.Fatalf("target %q has no edge", target)
		}
		s := graph.newStepDef(ctx, edge, nil)
		s.EnsureRule(ctx)
		return s
	}
	for _, target := range []string{
		"foo.a",
		"bar.stamp",
		"foo.stamp",
	} {
		setupFile(filepath.Join("out/Default", target))
		if newStepDef(target) == nil {
			t.Fatalf("stepDef for %q is nil?", target)
		}
	}
	s := newStepDef("target1")
	got := s.Inputs(ctx)
	want := []string{"source1", "out/Default/foo.stamp", "out/Default/foo.a", "source0"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Inputs: diff -want +got:\n%s", diff)
	}
	got = s.ExpandedInputs(ctx)
	want = []string{"source1", "source2", "out/Default/foo.a", "source3", "source4", "source0"}
	sort.Strings(got)
	sort.Strings(want)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ExpandedInputs: diff -want +got:\n%s", diff)
	}
}

func TestExpandedInputs_solibs(t *testing.T) {
	ctx := t.Context()
	state := ninjautil.NewState()
	p := ninjautil.NewManifestParser(state)
	dir := t.TempDir()
	fname := filepath.Join(dir, "build.ninja")
	err := os.WriteFile(fname, []byte(`
rule solink
  command = ...

build ./libc++.so ./libc++.so.TOC: solink

rule link
  command = ...

build ./protoc: link | ./libc++.so.TOC
   solibs = ./libc++.so

rule __rule
   command = ...

build foo.h: __rule | ./protoc
`), 0644)
	if err != nil {
		t.Fatal(err)
	}
	err = p.Load(ctx, fname)
	if err != nil {
		t.Fatal(err)
	}

	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}

	graph := &Graph{
		visited: make(map[*ninjautil.Edge]*build.Edge),
		globals: &globals{
			nstate:      state,
			path:        build.NewPath(dir, "out/Default"),
			hashFS:      hashFS,
			stepConfig:  &StepConfig{},
			targetPaths: make([]string, state.NumNodes()),
			edgeRules:   make([]edgeRuleHolder, state.NumNodes()),
		},
	}
	newStepDef := func(target string) *StepDef {
		node, ok := state.LookupNodeByPath(target)
		if !ok {
			t.Fatalf("target %q not found in build.ninja", target)
		}
		edge, ok := node.InEdge()
		if !ok {
			t.Fatalf("target %q has no edge", target)
		}
		return graph.newStepDef(ctx, edge, nil)
	}

	for _, target := range []string{
		"libc++.so.TOC",
		"libc++.so",
		"protoc",
	} {
		fullPath := filepath.Join(dir, "out/Default", target)
		err := os.MkdirAll(filepath.Dir(fullPath), 0755)
		if err != nil {
			t.Fatalf("MkdirAll(%q)=%v", filepath.Dir(fullPath), err)
		}
		err = os.WriteFile(fullPath, nil, 0644)
		if err != nil {
			t.Fatalf("WriteFile(%q)=%v", fullPath, err)
		}
		if newStepDef(target) == nil {
			t.Fatalf("stepDef for %q is nil?", target)
		}
	}
	s := newStepDef("foo.h")
	got := s.Inputs(ctx)
	want := []string{"out/Default/protoc"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Inputs: diff -want +got:\n%s", diff)
	}
	got = s.ExpandedInputs(ctx)
	want = []string{
		"out/Default/protoc",
		"out/Default/libc++.so",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ExpandedInputs: diff -want +got:\n%s", diff)
	}
}

func TestExpandedInputs_indirect_inputs(t *testing.T) {
	ctx := t.Context()
	state := ninjautil.NewState()
	p := ninjautil.NewManifestParser(state)
	dir := t.TempDir()
	fname := filepath.Join(dir, "build.ninja")
	err := os.WriteFile(fname, []byte(`
rule __rule
  command = ....

build target4.h target4.m: __rule ../../source4.in
build target3.h: __rule ../../source3.in target4.m
build target2.h: __rule ../../source2.in
build target1: __rule ../../source1.cc target2.h target3.h
`), 0644)
	if err != nil {
		t.Fatal(err)
	}
	err = p.Load(ctx, fname)
	if err != nil {
		t.Fatal(err)
	}

	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}

	setupFile := func(fname string) {
		fullpath := filepath.Join(dir, fname)
		err := os.MkdirAll(filepath.Dir(fullpath), 0755)
		if err != nil {
			t.Fatalf("MkdirAll(%q)=%v", fname, err)
		}
		err = os.WriteFile(fullpath, nil, 0644)
		if err != nil {
			t.Fatalf("WriteFile(%q)=%v", fname, err)
		}
	}
	setupFile("source4.in")
	setupFile("source3.in")
	setupFile("source2.in")
	setupFile("source1.cc")

	graph := &Graph{
		visited: make(map[*ninjautil.Edge]*build.Edge),
		globals: &globals{
			nstate: state,
			path:   build.NewPath(dir, "out/Default"),
			hashFS: hashFS,
			stepConfig: &StepConfig{
				Rules: []*StepRule{
					{
						Name:       "rule1",
						ActionName: "__rule",
						IndirectInputs: &IndirectInputs{
							Includes: []string{"*.h"},
						},
					},
				},
			},
			targetPaths: make([]string, state.NumNodes()),
			edgeRules:   make([]edgeRuleHolder, state.NumNodes()),
		},
	}
	err = graph.globals.stepConfig.Init(ctx)
	if err != nil {
		t.Fatal(err)
	}
	newStepDef := func(target string) *StepDef {
		node, ok := state.LookupNodeByPath(target)
		if !ok {
			t.Fatalf("target %q not found in build.ninja", target)
		}
		edge, ok := node.InEdge()
		if !ok {
			t.Fatalf("target %q has no edge", target)
		}
		s := graph.newStepDef(ctx, edge, nil)
		s.EnsureRule(ctx)
		return s
	}
	for _, target := range []string{
		"target4.h",
		"target3.h",
		"target2.h",
	} {
		setupFile(filepath.Join("out/Default", target))
		if newStepDef(target) == nil {
			t.Fatalf("stepDef for %q is nil?", target)
		}
	}
	s := newStepDef("target1")
	got := s.Inputs(ctx)
	want := []string{"source1.cc", "out/Default/target2.h", "out/Default/target3.h"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Inputs: diff -want +got:\n%s", diff)
	}
	got = s.ExpandedInputs(ctx)
	want = []string{"source1.cc", "out/Default/target2.h", "out/Default/target3.h", "out/Default/target4.h"}
	sort.Strings(got)
	sort.Strings(want)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ExpandedInputs: diff -want +got:\n%s", diff)
	}
}
