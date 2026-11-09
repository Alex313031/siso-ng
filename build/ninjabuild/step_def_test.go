// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjabuild

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"go.chromium.org/build/hashigo/digest"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/path"
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

	got := s.expandLabels(ctx, []path.Path{
		"foo/bar",
		"component:component",
	})
	want := []path.Path{
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
			targetPaths: make([]path.Path, state.NumNodes()),
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
	got := path.Strings(s.Inputs(ctx))
	want := []string{"out/Default/target2", "source1", "out/Default/target3", "source0"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Inputs: diff -want +got:\n%s", diff)
	}
	got = path.Strings(s.ExpandedInputs(ctx))
	sort.Strings(got)
	sort.Strings(want)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ExpandedInputs: diff -want +got:\n%s", diff)
	}
}

func TestTriggerInputs_phony(t *testing.T) {
	ctx := t.Context()
	state := ninjautil.NewState()
	p := ninjautil.NewManifestParser(state)
	dir := t.TempDir()
	fname := filepath.Join(dir, "build.ninja")
	err := os.WriteFile(fname, []byte(`
rule __rule
  command = ....
build phony_empty: phony
build phony_nested_empty: phony
build phony_leaf: phony ../../source2 phony_nested_empty
build phony_mid1: phony ../../tool1 phony_leaf
build phony_mid2: phony ../../source2 ../../tool2 phony_leaf
build phony_order_only: phony ../../source3
build target1: __rule ../../source1 | ../../tool1 phony_mid1 phony_mid2 phony_empty || phony_order_only ../../source4
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
	defer hashFS.Close(ctx)

	graph := &Graph{
		visited: make(map[*ninjautil.Edge]*build.Edge),
		globals: &globals{
			nstate:      state,
			path:        build.NewPath(dir, "out/Default"),
			hashFS:      hashFS,
			stepConfig:  &StepConfig{},
			targetPaths: make([]path.Path, state.NumNodes()),
			edgeRules:   make([]edgeRuleHolder, state.NumNodes()),
		},
	}
	err = graph.globals.stepConfig.Init(ctx)
	if err != nil {
		t.Fatal(err)
	}
	node, ok := state.LookupNodeByPath("target1")
	if !ok {
		t.Fatalf("target1 not found in build.ninja")
	}
	edge, ok := node.InEdge()
	if !ok {
		t.Fatalf("target1 has no edge")
	}
	s := graph.newStepDef(ctx, edge, nil)
	s.EnsureRule(ctx)

	got := path.Strings(s.TriggerInputs(ctx))
	want := []string{
		"source1",
		"tool1",
		"out/Default/phony_mid1",
		"out/Default/phony_mid2",
		"out/Default/phony_empty",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("TriggerInputs: diff -want +got:\n%s", diff)
	}

	got = path.Strings(s.ExpandedTriggerInputs(ctx))
	want = []string{
		"source1",
		"tool1",
		"source2",
		"tool2",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ExpandedTriggerInputs: diff -want +got:\n%s", diff)
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
			targetPaths: make([]path.Path, state.NumNodes()),
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
	got := path.Strings(s.Inputs(ctx))
	want := []string{"source1", "out/Default/foo.stamp", "out/Default/foo.a", "source0"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Inputs: diff -want +got:\n%s", diff)
	}
	got = path.Strings(s.ExpandedInputs(ctx))
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
			targetPaths: make([]path.Path, state.NumNodes()),
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
	got := path.Strings(s.Inputs(ctx))
	want := []string{"out/Default/protoc"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Inputs: diff -want +got:\n%s", diff)
	}
	got = path.Strings(s.ExpandedInputs(ctx))
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
						IndirectInputs: &PathFilter{
							Includes: []string{"*.h"},
						},
					},
				},
			},
			targetPaths: make([]path.Path, state.NumNodes()),
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
	got := path.Strings(s.Inputs(ctx))
	want := []string{"source1.cc", "out/Default/target2.h", "out/Default/target3.h"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Inputs: diff -want +got:\n%s", diff)
	}
	got = path.Strings(s.ExpandedInputs(ctx))
	want = []string{"source1.cc", "out/Default/target2.h", "out/Default/target3.h", "out/Default/target4.h"}
	sort.Strings(got)
	sort.Strings(want)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ExpandedInputs: diff -want +got:\n%s", diff)
	}
}

func TestStepDefBinding_RestatContent(t *testing.T) {
	ctx := t.Context()
	state := ninjautil.NewState()
	p := ninjautil.NewManifestParser(state)
	dir := t.TempDir()
	fname := filepath.Join(dir, "build.ninja")
	err := os.WriteFile(fname, []byte(`
rule rule_restat_content
  command = echo ${in} > ${out}
  restat = 1
  restat_content = 1

rule rule_plain
  command = echo ${in} > ${out}
  restat = 1

rule rule_unset
  command = echo ${in} > ${out}
  restat = 1

build out0: rule_unset in1
build out1: rule_plain in1
build out2: rule_plain in1
  restat_content = false
build out3: rule_plain in1
  restat_content = 0
build out4: rule_restat_content in1
build out5: rule_restat_content in1
  restat_content = false
build out6: rule_restat_content in1
  restat_content = 0
build out7: rule_restat_content in1
`), 0644)
	if err != nil {
		t.Fatal(err)
	}
	err = p.Load(ctx, fname)
	if err != nil {
		t.Fatal(err)
	}

	boolPtr := func(b bool) *bool { return &b }

	graph := &Graph{
		visited: make(map[*ninjautil.Edge]*build.Edge),
		globals: &globals{
			nstate: state,
			path:   build.NewPath(dir, "out/Default"),
			stepConfig: &StepConfig{
				Rules: []*StepRule{
					{
						Name:          "starlark_disable_out7",
						ActionOuts:    []string{"./out7"},
						RestatContent: boolPtr(false),
					},
					{
						Name:          "starlark_enable_plain",
						ActionName:    "rule_plain",
						RestatContent: boolPtr(true),
					},
					{
						Name:       "default_rule",
						ActionName: "rule_restat_content",
					},
				},
			},
			targetPaths: make([]path.Path, state.NumNodes()),
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

	tests := []struct {
		target string
		want   string
	}{
		// Both Starlark and Ninja unset -> ""
		{target: "out0", want: ""},
		// Starlark enables restat_content, Ninja unset -> "true"
		{target: "out1", want: "true"},
		// Starlark enables restat_content, Ninja sets restat_content = false -> "true" (Starlark takes precedence)
		{target: "out2", want: "true"},
		// Starlark enables restat_content, Ninja sets restat_content = 0 -> "true" (Starlark takes precedence)
		{target: "out3", want: "true"},
		// Starlark unset, Ninja rule sets restat_content = 1 -> "true"
		{target: "out4", want: "true"},
		// Starlark unset, Ninja rule sets restat_content = 1, edge sets false -> "false"
		{target: "out5", want: "false"},
		// Starlark unset, Ninja rule sets restat_content = 1, edge sets 0 -> "false"
		{target: "out6", want: "false"},
		// Ninja rule sets restat_content = 1, Starlark rule sets restat_content = false -> "false"
		{target: "out7", want: "false"},
	}

	for _, tc := range tests {
		s := newStepDef(tc.target)
		if got := s.Binding("restat_content"); got != tc.want {
			t.Errorf("%s: Binding(\"restat_content\") = %q; want %q", tc.target, got, tc.want)
		}
	}
}

func TestStepDefSandbox_Disabled(t *testing.T) {
	ctx := t.Context()
	state := ninjautil.NewState()
	p := ninjautil.NewManifestParser(state)
	dir := t.TempDir()
	fname := filepath.Join(dir, "build.ninja")
	err := os.WriteFile(fname, []byte(`
rule rule_plain
  command = touch ${out}

rule rule_ninja_disabled
  command = touch ${out}
  sandbox_disabled = true

rule rule_starlark_disabled
  command = touch ${out}

build out0: rule_plain
build out1: rule_ninja_disabled
build out2: rule_starlark_disabled
`), 0644)
	if err != nil {
		t.Fatal(err)
	}
	err = p.Load(ctx, fname)
	if err != nil {
		t.Fatal(err)
	}

	graph := &Graph{
		visited: make(map[*ninjautil.Edge]*build.Edge),
		globals: &globals{
			nstate: state,
			path:   build.NewPath(dir, "out/Default"),
			stepConfig: &StepConfig{
				Sandbox: map[string]string{"type": "test"},
				Rules: []*StepRule{
					{
						Name:            "starlark_disable_sandbox",
						ActionName:      "rule_starlark_disabled",
						SandboxDisabled: true,
					},
				},
			},
			targetPaths: make([]path.Path, state.NumNodes()),
			edgeRules:   make([]edgeRuleHolder, state.NumNodes()),
		},
	}
	err = graph.globals.stepConfig.Init(ctx)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		target      string
		wantSandbox bool
	}{
		// Neither Starlark nor Ninja disables the sandbox.
		{target: "out0", wantSandbox: true},
		// Ninja rule sets sandbox_disabled = true.
		{target: "out1", wantSandbox: false},
		// Starlark rule sets sandbox_disabled.
		{target: "out2", wantSandbox: false},
	}
	for _, tc := range tests {
		node, ok := state.LookupNodeByPath(tc.target)
		if !ok {
			t.Fatalf("target %q not found in build.ninja", tc.target)
		}
		edge, ok := node.InEdge()
		if !ok {
			t.Fatalf("target %q has no edge", tc.target)
		}
		s := graph.newStepDef(ctx, edge, nil)
		s.EnsureRule(ctx)
		if got := s.Sandbox() != nil; got != tc.wantSandbox {
			t.Errorf("%s: Sandbox() != nil is %t; want %t", tc.target, got, tc.wantSandbox)
		}
	}
}

func setupShellStepDefTest(ctx context.Context, t *testing.T) (*ninjautil.State, *build.Path, *hashfs.HashFS) {
	t.Helper()
	dir := t.TempDir()
	workspaceRoot := dir
	outDir := "out/Default"
	ninjaDir := filepath.Join(dir, outDir)
	err := os.MkdirAll(ninjaDir, 0755)
	if err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(dir, "bin")
	err = os.MkdirAll(binDir, 0755)
	if err != nil {
		t.Fatal(err)
	}
	shellPath := filepath.Join(binDir, "sh")
	err = os.WriteFile(shellPath, []byte("#!/bin/sh\nexit 0\n"), 0755)
	if err != nil {
		t.Fatal(err)
	}

	buildNinja := filepath.Join(ninjaDir, "build.ninja")
	err = os.WriteFile(buildNinja, []byte(`
rule nontrivial
  command = cat in.txt > out_nontrivial.txt
rule trivial
  command = cp in.txt out_trivial.txt

build out_nontrivial.txt: nontrivial in.txt
build out_trivial.txt: trivial in.txt
`), 0644)
	if err != nil {
		t.Fatal(err)
	}

	bpath := build.NewPath(workspaceRoot, outDir)
	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := hashFS.Close(context.WithoutCancel(ctx)); err != nil {
			t.Errorf("hashFS.Close=%v", err)
		}
	})

	nstate := ninjautil.NewState()
	p := ninjautil.NewManifestParser(nstate)
	err = p.Load(ctx, buildNinja)
	if err != nil {
		t.Fatal(err)
	}
	return nstate, bpath, hashFS
}

func TestStepDef_DefaultShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("These tests don't work on windows")
	}
	ctx := t.Context()
	nstate, bpath, hashFS := setupShellStepDefTest(ctx, t)
	stepConfig := &StepConfig{}
	depsLog := &DepsLog{}
	graph, err := NewGraph(ctx, "build.ninja", nstate, nil, bpath, hashFS, stepConfig, depsLog)
	if err != nil {
		t.Fatal(err)
	}

	nodeNontrivial, _ := nstate.LookupNodeByPath("out_nontrivial.txt")
	edgeNontrivial, _ := nodeNontrivial.InEdge()
	stepNontrivial := graph.newStepDef(ctx, edgeNontrivial, nil)

	args := stepNontrivial.Args(ctx)
	if diff := cmp.Diff([]string{"/bin/sh", "-c", "cat in.txt > out_nontrivial.txt"}, args); diff != "" {
		t.Errorf("Args without hermetic shell diff -want +got:\n%s", diff)
	}
	for _, in := range stepNontrivial.Inputs(ctx) {
		if strings.Contains(string(in), "sh") {
			t.Errorf("Inputs without hermetic shell should not contain shell, got %v", in)
		}
	}
	for _, in := range stepNontrivial.TriggerInputs(ctx) {
		if strings.Contains(string(in), "sh") {
			t.Errorf("TriggerInputs without hermetic shell should not contain shell, got %v", in)
		}
	}

	nodeTrivial, _ := nstate.LookupNodeByPath("out_trivial.txt")
	edgeTrivial, _ := nodeTrivial.InEdge()
	stepTrivial := graph.newStepDef(ctx, edgeTrivial, nil)
	if diff := cmp.Diff([]string{"cp", "in.txt", "out_trivial.txt"}, stepTrivial.Args(ctx)); diff != "" {
		t.Errorf("trivial Args diff -want +got:\n%s", diff)
	}
}

func TestStepDef_HermeticShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("These tests don't work on windows")
	}
	ctx := t.Context()
	nstate, bpath, hashFS := setupShellStepDefTest(ctx, t)
	stepConfig := &StepConfig{
		Shell: "bin/sh",
	}
	depsLog := &DepsLog{}
	graph, err := NewGraph(ctx, "build.ninja", nstate, nil, bpath, hashFS, stepConfig, depsLog)
	if err != nil {
		t.Fatal(err)
	}

	nodeNontrivial, _ := nstate.LookupNodeByPath("out_nontrivial.txt")
	edgeNontrivial, _ := nodeNontrivial.InEdge()
	stepNontrivial := graph.newStepDef(ctx, edgeNontrivial, nil)

	args := stepNontrivial.Args(ctx)
	if diff := cmp.Diff([]string{"../../bin/sh", "-c", "cat in.txt > out_nontrivial.txt"}, args); diff != "" {
		t.Errorf("Args with hermetic shell diff -want +got:\n%s", diff)
	}

	wantShellInput := path.Path("bin/sh")
	if !slices.Contains(stepNontrivial.Inputs(ctx), wantShellInput) {
		t.Errorf("Inputs with hermetic shell should contain %s, got %v", wantShellInput, stepNontrivial.Inputs(ctx))
	}
	if !slices.Contains(stepNontrivial.TriggerInputs(ctx), wantShellInput) {
		t.Errorf("TriggerInputs with hermetic shell should contain %s, got %v", wantShellInput, stepNontrivial.TriggerInputs(ctx))
	}
	if !slices.Contains(stepNontrivial.ExpandedTriggerInputs(ctx), wantShellInput) {
		t.Errorf("ExpandedTriggerInputs with hermetic shell should contain %s, got %v", wantShellInput, stepNontrivial.ExpandedTriggerInputs(ctx))
	}
	if !slices.Contains(stepNontrivial.ToolInputs(ctx), wantShellInput) {
		t.Errorf("ToolInputs with hermetic shell should contain %s, got %v", wantShellInput, stepNontrivial.ToolInputs(ctx))
	}
	if !slices.Contains(stepNontrivial.ExpandedInputs(ctx), wantShellInput) {
		t.Errorf("ExpandedInputs with hermetic shell should contain %s, got %v", wantShellInput, stepNontrivial.ExpandedInputs(ctx))
	}

	nodeTrivial, _ := nstate.LookupNodeByPath("out_trivial.txt")
	edgeTrivial, _ := nodeTrivial.InEdge()
	stepTrivial := graph.newStepDef(ctx, edgeTrivial, nil)
	if diff := cmp.Diff([]string{"cp", "in.txt", "out_trivial.txt"}, stepTrivial.Args(ctx)); diff != "" {
		t.Errorf("trivial Args diff -want +got:\n%s", diff)
	}
	if slices.Contains(stepTrivial.Inputs(ctx), wantShellInput) {
		t.Errorf("trivial Inputs should NOT contain %s, got %v", wantShellInput, stepTrivial.Inputs(ctx))
	}
	if slices.Contains(stepTrivial.TriggerInputs(ctx), wantShellInput) {
		t.Errorf("trivial TriggerInputs should NOT contain %s, got %v", wantShellInput, stepTrivial.TriggerInputs(ctx))
	}
}

func TestStepDef_HermeticShell_InputDeps(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("These tests don't work on windows")
	}
	ctx := t.Context()
	nstate, bpath, hashFS := setupShellStepDefTest(ctx, t)
	libDir := filepath.Join(bpath.WorkspaceRoot, "lib")
	err := os.MkdirAll(libDir, 0755)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(filepath.Join(libDir, "libc.so"), []byte("/* libc */"), 0644)
	if err != nil {
		t.Fatal(err)
	}

	stepConfig := &StepConfig{
		Shell: "bin/sh",
		InputDeps: map[string][]string{
			"bin/sh": {
				"lib/libc.so",
			},
		},
	}
	depsLog := &DepsLog{}
	graph, err := NewGraph(ctx, "build.ninja", nstate, nil, bpath, hashFS, stepConfig, depsLog)
	if err != nil {
		t.Fatal(err)
	}

	nodeNontrivial, _ := nstate.LookupNodeByPath("out_nontrivial.txt")
	edgeNontrivial, _ := nodeNontrivial.InEdge()
	stepNontrivial := graph.newStepDef(ctx, edgeNontrivial, nil)

	wantShellInput := path.Path("bin/sh")
	wantDepInput := path.Path("lib/libc.so")
	for _, want := range []path.Path{wantShellInput, wantDepInput} {
		if !slices.Contains(stepNontrivial.Inputs(ctx), want) {
			t.Errorf("Inputs with hermetic shell should contain %s, got %v", want, stepNontrivial.Inputs(ctx))
		}
		if !slices.Contains(stepNontrivial.TriggerInputs(ctx), want) {
			t.Errorf("TriggerInputs with hermetic shell should contain %s, got %v", want, stepNontrivial.TriggerInputs(ctx))
		}
		if !slices.Contains(stepNontrivial.ExpandedTriggerInputs(ctx), want) {
			t.Errorf("ExpandedTriggerInputs with hermetic shell should contain %s, got %v", want, stepNontrivial.ExpandedTriggerInputs(ctx))
		}
		if !slices.Contains(stepNontrivial.ToolInputs(ctx), want) {
			t.Errorf("ToolInputs with hermetic shell should contain %s, got %v", want, stepNontrivial.ToolInputs(ctx))
		}
		if !slices.Contains(stepNontrivial.ExpandedInputs(ctx), want) {
			t.Errorf("ExpandedInputs with hermetic shell should contain %s, got %v", want, stepNontrivial.ExpandedInputs(ctx))
		}
	}

	nodeTrivial, _ := nstate.LookupNodeByPath("out_trivial.txt")
	edgeTrivial, _ := nodeTrivial.InEdge()
	stepTrivial := graph.newStepDef(ctx, edgeTrivial, nil)
	for _, notWant := range []path.Path{wantShellInput, wantDepInput} {
		if slices.Contains(stepTrivial.Inputs(ctx), notWant) {
			t.Errorf("trivial Inputs should NOT contain %s, got %v", notWant, stepTrivial.Inputs(ctx))
		}
		if slices.Contains(stepTrivial.TriggerInputs(ctx), notWant) {
			t.Errorf("trivial TriggerInputs should NOT contain %s, got %v", notWant, stepTrivial.TriggerInputs(ctx))
		}
	}
}

func TestStepDef_DepInputs_RecompactDroppedPath(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	ninjaDir := filepath.Join(dir, "out/Default")
	if err := os.MkdirAll(ninjaDir, 0755); err != nil {
		t.Fatal(err)
	}
	buildNinja := filepath.Join(ninjaDir, "build.ninja")
	if err := os.WriteFile(buildNinja, []byte(`
rule cc
  command = cc -c ${in} -o ${out}
  deps = gcc
build gen.h: cc gen.cc
build out.o: cc out.cc gen.h
build other.o: cc other.cc
`), 0644); err != nil {
		t.Fatal(err)
	}
	nstate := ninjautil.NewState()
	if err := ninjautil.NewManifestParser(nstate).Load(ctx, buildNinja); err != nil {
		t.Fatal(err)
	}
	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := hashFS.Close(context.WithoutCancel(ctx)); err != nil {
			t.Errorf("hashFS.Close=%v", err)
		}
	})
	depsLogFile := filepath.Join(ninjaDir, ".siso_deps")
	createNewDepsLogFile(ctx, depsLogFile)
	depsLog, err := NewDepsLog(ctx, depsLogFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := depsLog.Close(); err != nil {
			t.Errorf("depsLog.Close=%v", err)
		}
	})
	if _, err := depsLog.Record(ctx, DepsLogKey{Target: "out.o", Mtime: time.Unix(1, 0)}, []string{"out.cc", "gen.h"}); err != nil {
		t.Fatal(err)
	}
	if _, err := depsLog.Record(ctx, DepsLogKey{Target: "other.o", Mtime: time.Unix(2, 0)}, []string{"other.cc"}); err != nil {
		t.Fatal(err)
	}
	// Rebuild out.o without gen.h, then recompact .siso_deps.
	if _, err := depsLog.Record(ctx, DepsLogKey{Target: "out.o", Mtime: time.Unix(3, 0)}, []string{"out.cc"}); err != nil {
		t.Fatal(err)
	}
	depsLog.Reset()
	depsLog.depsLog.needsRecompact = true
	if err := depsLog.depsLog.recompactIfNeeded(ctx); err != nil {
		t.Fatal(err)
	}

	bpath := build.NewPath(dir, "out/Default")
	graph, err := NewGraph(ctx, "build.ninja", nstate, nil, bpath, hashFS, &StepConfig{}, depsLog)
	if err != nil {
		t.Fatal(err)
	}
	nodeGenH, ok := nstate.LookupNodeByPath("gen.h")
	if !ok {
		t.Fatal("gen.h not found")
	}
	edgeGenH, ok := nodeGenH.InEdge()
	if !ok {
		t.Fatal("gen.h has no in-edge")
	}
	stepGenH := graph.newStepDef(ctx, edgeGenH, nil)
	if _, err := stepGenH.DepInputs(ctx); !errors.Is(err, ErrNoDepsLog) {
		t.Errorf("stepGenH.DepInputs(ctx)=_, %v; want error wrapping %v", err, ErrNoDepsLog)
	}
}

func TestStepDef_DepInputs_RecordSnapshotIsolation(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	ninjaDir := filepath.Join(dir, "out/Default")
	if err := os.MkdirAll(ninjaDir, 0755); err != nil {
		t.Fatal(err)
	}
	buildNinja := filepath.Join(ninjaDir, "build.ninja")
	if err := os.WriteFile(buildNinja, []byte(`
rule cc
  command = cc -c ${in} -o ${out}
  deps = gcc
build out.o: cc out.cc
`), 0644); err != nil {
		t.Fatal(err)
	}
	outFile := filepath.Join(ninjaDir, "out.o")
	if err := os.WriteFile(outFile, nil, 0644); err != nil {
		t.Fatal(err)
	}
	nstate := ninjautil.NewState()
	if err := ninjautil.NewManifestParser(nstate).Load(ctx, buildNinja); err != nil {
		t.Fatal(err)
	}
	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := hashFS.Close(context.WithoutCancel(ctx)); err != nil {
			t.Errorf("hashFS.Close=%v", err)
		}
	})
	fi, err := hashFS.Stat(ctx, dir, "out/Default/out.o")
	if err != nil {
		t.Fatal(err)
	}
	mtime := fi.ModTime()
	depsLog, err := NewDepsLog(ctx, filepath.Join(ninjaDir, ".siso_deps"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := depsLog.Close(); err != nil {
			t.Errorf("depsLog.Close=%v", err)
		}
	})
	if _, err := depsLog.Record(ctx, DepsLogKey{Target: "out.o", Mtime: mtime}, []string{"out.cc", "foo.h", "bar.h"}); err != nil {
		t.Fatal(err)
	}
	depsLog.Reset()

	bpath := build.NewPath(dir, "out/Default")
	graph, err := NewGraph(ctx, "build.ninja", nstate, nil, bpath, hashFS, &StepConfig{}, depsLog)
	if err != nil {
		t.Fatal(err)
	}
	nodeOut, ok := nstate.LookupNodeByPath("out.o")
	if !ok {
		t.Fatal("out.o not found")
	}
	edgeOut, ok := nodeOut.InEdge()
	if !ok {
		t.Fatal("out.o has no in-edge")
	}
	writerStep := graph.newStepDef(ctx, edgeOut, nil)
	if _, err := writerStep.RecordDeps(ctx, "out.o", mtime, digest.Digest{}, []string{"out.cc", "foo.h"}); err != nil {
		t.Fatal(err)
	}
	// Before depsLog.Reset(), a StepDef reading DepInputs must still see the committed snapshot.
	readerStep := graph.newStepDef(ctx, edgeOut, nil)
	seq, err := readerStep.DepInputs(ctx)
	if err != nil {
		t.Fatalf("readerStep.DepInputs(ctx)=_, %v; want nil err", err)
	}
	got := path.Strings(slices.Collect(seq))
	want := []string{"out/Default/out.cc", "out/Default/foo.h", "out/Default/bar.h"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("before Reset: DepInputs diff -want +got:\n%s", diff)
	}
}
