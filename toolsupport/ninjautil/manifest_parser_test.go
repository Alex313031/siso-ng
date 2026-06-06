// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjautil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestParser_Empty(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	err := os.WriteFile(filepath.Join(dir, "input"), nil, 0644)
	if err != nil {
		t.Fatal(err)
	}

	state := NewState()
	p := NewManifestParser(state)
	p.SetWd(dir)
	err = p.Load(ctx, "input")
	if err != nil {
		t.Errorf("Load %v", err)
	}
}

func TestParser_Rules(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	err := os.WriteFile(filepath.Join(dir, "input"), []byte(`
rule cat
  command = cat ${in} > ${out}
rule date
  command = date > $out
build result: cat in_1.cc in-2.O
`), 0644)
	if err != nil {
		t.Fatal(err)
	}

	state := NewState()
	p := NewManifestParser(state)
	p.SetWd(dir)
	err = p.Load(ctx, "input")
	if err != nil {
		t.Fatalf("Load %v", err)
	}
	node, ok := state.LookupNodeByPath("result")
	if !ok {
		t.Fatalf("missing result")
	}
	edge, ok := node.InEdge()
	if !ok {
		t.Fatalf("no inEdge for result")
	}
	if edge.RuleName() != "cat" {
		t.Errorf("RuleName=%q; want=%q", edge.RuleName(), "cat")
	}
	cmd := edge.RawBinding("command")
	want := "cat ${in} > ${out}"
	if cmd != want {
		t.Errorf("rule cat command=%q; want=%q", cmd, want)
	}
}

func TestParser_EscapedPath(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	err := os.WriteFile(filepath.Join(dir, "build.ninja"), []byte(`
rule echo
  command = echo $in > $out
build $:all: phony out
build out: echo foo$ bar $
 bar$:baz
`), 0644)
	if err != nil {
		t.Fatal(err)
	}
	state := NewState()
	p := NewManifestParser(state)
	p.SetWd(dir)
	err = p.Load(ctx, "build.ninja")
	if err != nil {
		t.Fatalf("Load %v", err)
	}
	node, ok := state.LookupNodeByPath(":all")
	if !ok {
		t.Fatalf("missing :all")
	}
	edge, ok := node.InEdge()
	if !ok {
		t.Fatalf("no inEdge for :all")
	}
	if edge.RuleName() != "phony" {
		t.Errorf("RuleName=%q; want=%q", edge.RuleName(), "phony")
	}
	ins := edge.Inputs()
	if len(ins) != 1 {
		t.Fatalf("ins=%d; want=1", len(ins))
	}
	if ins[0].Path() != "out" {
		t.Errorf("ins[0]=%q; want=%q", ins[0].Path(), "out")
	}
	edge, ok = ins[0].InEdge()
	if !ok {
		t.Fatalf("no inEdge for %q", ins[0].Path())
	}
	if edge.RuleName() != "echo" {
		t.Errorf("RuleName=%q; want=%q", edge.RuleName(), "echo")
	}
	ins = edge.Inputs()
	if len(ins) != 2 {
		t.Fatalf("ins=%d; want=2", len(ins))
	}
	if ins[0].Path() != "foo bar" {
		t.Errorf("ins[0]=%q; want=%q", ins[0].Path(), "foo bar")
	}
	if ins[1].Path() != "bar:baz" {
		t.Errorf("ins[1]=%q; want=%q", ins[1].Path(), "bar:baz")
	}
}

func TestParser_Binding_flags(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	err := os.WriteFile(filepath.Join(dir, "build.ninja"), []byte(`
ninja_required_version = 1.7.2
asmflags = -fPIC
defines = -DDCHECK_ALWAYS_ON=1
include_dirs = -I../.. -Igen

rule asm
  command = ../../third_party/llvm-build/Release+Asserts/bin/clang -MMD -MF ${out}.d ${defines} ${include_dirs} ${asmflags} -c ${in} -o ${out}
  depfile = obj/${source_name_part}.o.d
  deps = gcc
  description = ASM ${out}

build obj/armv8-linux.o: asm ../../armv8-linux.S
  source_name_part = armv8-linux
`), 0644)
	if err != nil {
		t.Fatal(err)
	}
	state := NewState()
	p := NewManifestParser(state)
	p.SetWd(dir)
	err = p.Load(ctx, "build.ninja")
	if err != nil {
		t.Fatalf("Load %v", err)
	}
	node, ok := state.LookupNodeByPath("obj/armv8-linux.o")
	if !ok {
		t.Fatalf("missing obj/armv8-linux.o")
	}
	edge, ok := node.InEdge()
	if !ok {
		t.Fatalf("no inEdge for obj/armv8-linux.o")
	}
	if edge.RuleName() != "asm" {
		t.Errorf("RuleName=%q; want=%q", edge.RuleName(), "asm")
	}
	if got, want := edge.Binding("command"), "../../third_party/llvm-build/Release+Asserts/bin/clang -MMD -MF obj/armv8-linux.o.d -DDCHECK_ALWAYS_ON=1 -I../.. -Igen -fPIC -c ../../armv8-linux.S -o obj/armv8-linux.o"; got != want {
		t.Errorf("command=%q; want=%q", got, want)
	}
	if got, want := edge.UnescapedBinding("depfile"), "obj/armv8-linux.o.d"; got != want {
		t.Errorf("depfile=%q; want=%q", got, want)
	}
	if got, want := edge.Binding("deps"), "gcc"; got != want {
		t.Errorf("deps=%q; want=%q", got, want)
	}
	if got, want := edge.Binding("description"), "ASM obj/armv8-linux.o"; got != want {
		t.Errorf("description=%q; want=%q", got, want)
	}
}

func TestParser_Binding_rsp(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	err := os.WriteFile(filepath.Join(dir, "build.ninja"), []byte(`
rule gen_buildflags
  rspfile = ${out}.rsp
  rspfile_content = -flags DCHECK_IS_CONFIGURABLE=false
  command = python3 ../../build/write_buildflag_header.py --output ${out} --rulename //base$:debugging_buildflags --gen-dir gen --definitions ${rspfile}
  restat = 1

build gen/base/debug/debugging_buildflags.h $
 : gen_buildflags $
  | $
    ../../build/write_buildflag_header.py
`), 0644)
	if err != nil {
		t.Fatal(err)
	}
	state := NewState()
	p := NewManifestParser(state)
	p.SetWd(dir)
	err = p.Load(ctx, "build.ninja")
	if err != nil {
		t.Fatalf("Load %v", err)
	}
	node, ok := state.LookupNodeByPath("gen/base/debug/debugging_buildflags.h")
	if !ok {
		t.Fatalf("missing gen/base/debug/debugging_buildflags.h")
	}
	edge, ok := node.InEdge()
	if !ok {
		t.Fatalf("no inEdge for gen/base/debug/debugging_buildflags.h")
	}
	if edge.RuleName() != "gen_buildflags" {
		t.Errorf("RuleName=%q; want=%q", edge.RuleName(), "gen_buildflags")
	}
	if got, want := edge.Binding("rspfile"), "gen/base/debug/debugging_buildflags.h.rsp"; got != want {
		t.Errorf("rspfile=%q; want=%q", got, want)
	}
	if got, want := edge.Binding("rspfile_content"), "-flags DCHECK_IS_CONFIGURABLE=false"; got != want {
		t.Errorf("rspcontent=%q; want=%q", got, want)
	}
	if got, want := edge.Binding("command"), "python3 ../../build/write_buildflag_header.py --output gen/base/debug/debugging_buildflags.h --rulename //base:debugging_buildflags --gen-dir gen --definitions gen/base/debug/debugging_buildflags.h.rsp"; got != want {
		t.Errorf("command=%q; want=%q", got, want)
	}
}

func TestParser_Binding_buildscope(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	err := os.WriteFile(filepath.Join(dir, "build.ninja"), []byte(`
pool build_toolchain_action_pool
  depth = 128

rule nocompile
  command = python3 ../../tools/nocompile/wrapper.py ../../third_party/llvm-build/Release+Asserts/bin/clang++ ${in} obj/base/${source_name_part}.o obj/base/${source_name_part}.o.d -- ${cflags} ${cflags_cc} ${defines} ${include_dirs} -MMD -MF obj/base/${source_name_part}.o.d -MT obj/base/${source_name_part}.o -x c++
  description = ACTION //base:base_nocompile_tests(//build/toolchain/linux:clang_x64)
  pool = build_toolchain_action_pool
  restat = 1

build obj/base/nocompile.o: nocompile ../../base/test/nocompile.nc
  defines =
  include_dirs =
  cflags =
  cflags_cc =
  source_name_part = nocompile
  defines = -DDCHECK_ALWAYS_ON=1
  include_dirs = -I../.. -Igen
  cflags = -Wall
  cflags_cc = -std=c++20
  depfile = obj/base/nocompile.o.d
`), 0644)
	if err != nil {
		t.Fatal(err)
	}
	state := NewState()
	p := NewManifestParser(state)
	p.SetWd(dir)
	err = p.Load(ctx, "build.ninja")
	if err != nil {
		t.Fatalf("Load %v", err)
	}
	node, ok := state.LookupNodeByPath("obj/base/nocompile.o")
	if !ok {
		t.Fatalf("missing obj/base/nocompile.o")
	}
	edge, ok := node.InEdge()
	if !ok {
		t.Fatalf("no inEdge for obj/base/nocompile.o")
	}
	if got, want := edge.Binding("command"), "python3 ../../tools/nocompile/wrapper.py ../../third_party/llvm-build/Release+Asserts/bin/clang++ ../../base/test/nocompile.nc obj/base/nocompile.o obj/base/nocompile.o.d -- -Wall -std=c++20 -DDCHECK_ALWAYS_ON=1 -I../.. -Igen -MMD -MF obj/base/nocompile.o.d -MT obj/base/nocompile.o -x c++"; got != want {
		t.Errorf("command=%q; want=%q", got, want)
	}
}

func TestParser_Binding_Recursive(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	err := os.WriteFile(filepath.Join(dir, "build.ninja"), []byte(`
cflags_cc = /Fpobj/generated_api_types_cc.pch /Yubuild/precompile.h

rule cxx
  command = ..\..\third_party\llvm-build\Release+Asserts\bin\clang-cl.exe /c ${in} /Fo${out} /W4 ${cflags_cc} /Fd"obj/api/generated_api_types_cc.pdb"
  # ${cflags_cc} should be "/Fpobj/generated_api_types_cc.pch /Yubuild/precompile.h /Ycbuild/precompile.h"

build obj/api/generated_api_types/precompile.cc.obj: cxx ../../build/precompile.cc || phony/input_deps
  cflags_cc = ${cflags_cc} /Ycbuild/precompile.h
`), 0644)
	if err != nil {
		t.Fatal(err)
	}
	state := NewState()
	p := NewManifestParser(state)
	p.SetWd(dir)
	err = p.Load(ctx, "build.ninja")
	if err != nil {
		t.Fatalf("Load %v", err)
	}
	node, ok := state.LookupNodeByPath("obj/api/generated_api_types/precompile.cc.obj")
	if !ok {
		t.Fatal("missing obj/api/generated_api_types/precompile.cc.obj")
	}
	edge, ok := node.InEdge()
	if !ok {
		t.Fatal("no inEdge for obj/api/generated_api_types/precompile.cc.obj")
	}
	if got, want := edge.Binding("command"), `..\..\third_party\llvm-build\Release+Asserts\bin\clang-cl.exe /c ../../build/precompile.cc /Foobj/api/generated_api_types/precompile.cc.obj /W4 /Fpobj/generated_api_types_cc.pch /Yubuild/precompile.h /Ycbuild/precompile.h /Fd"obj/api/generated_api_types_cc.pdb"`; got != want {
		t.Errorf("command=%q; want=%q", got, want)
	}
}

func TestParser_Dupbuild_Error(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	err := os.WriteFile(filepath.Join(dir, "build.ninja"), []byte(`
rule cat
  command = cat $in > $out
build b: cat a
build b: cat c
`), 0644)
	if err != nil {
		t.Fatal(err)
	}
	state := NewState()
	p := NewManifestParser(state)
	p.SetWd(dir)
	err = p.Load(ctx, "build.ninja")
	if _, ok := errors.AsType[multipleRulesError](err); !ok {
		t.Errorf("p.Load() got: %v; want: %v", err, multipleRulesError{})
	}
}

func TestParser_ConcurrentSubninja(t *testing.T) {
	origLoaderConcurrency := loaderConcurrency
	loaderConcurrency = 8
	defer func() { loaderConcurrency = origLoaderConcurrency }()
	ctx := t.Context()
	dir := t.TempDir()

	state := NewState()
	p := NewManifestParser(state)
	p.SetWd(dir)

	write := func(fname, content string) {
		t.Helper()
		fname = filepath.Join(dir, fname)
		err := os.MkdirAll(filepath.Dir(fname), 0755)
		if err != nil {
			t.Fatal(err)
		}
		err = os.WriteFile(fname, []byte(content), 0644)
		if err != nil {
			t.Fatal(err)
		}
	}

	write("build.ninja", `
subninja a/build.ninja
subninja b/build.ninja
subninja c/build.ninja
subninja d/build.ninja
subninja e/build.ninja
subninja f/build.ninja
subninja g/build.ninja
subninja h/build.ninja
subninja i/build.ninja
`)

	for _, d := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"} {
		write(fmt.Sprintf("%s/build.ninja", d), fmt.Sprintf(`
subninja %[1]s/a/build.ninja
subninja %[1]s/a/build.ninja
subninja %[1]s/b/build.ninja
subninja %[1]s/c/build.ninja
subninja %[1]s/d/build.ninja
subninja %[1]s/e/build.ninja
subninja %[1]s/f/build.ninja
subninja %[1]s/g/build.ninja
subninja %[1]s/h/build.ninja
subninja %[1]s/i/build.ninja
`, d))
		write(fmt.Sprintf("%s/a/build.ninja", d), "")
		write(fmt.Sprintf("%s/b/build.ninja", d), "")
		write(fmt.Sprintf("%s/c/build.ninja", d), "")
		write(fmt.Sprintf("%s/d/build.ninja", d), "")
		write(fmt.Sprintf("%s/e/build.ninja", d), "")
		write(fmt.Sprintf("%s/f/build.ninja", d), "")
		write(fmt.Sprintf("%s/g/build.ninja", d), "")
		write(fmt.Sprintf("%s/h/build.ninja", d), "")
		write(fmt.Sprintf("%s/i/build.ninja", d), "")
	}

	err := p.Load(ctx, "build.ninja")
	if err != nil {
		t.Fatal(err)
	}
}

func TestParser_Validation(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	err := os.WriteFile(filepath.Join(dir, "input"), []byte(`
rule cat
   command = cat $in > $out
build foo: cat bar |@ baz baz2
`), 0644)
	if err != nil {
		t.Fatal(err)
	}
	state := NewState()
	p := NewManifestParser(state)
	p.SetWd(dir)
	err = p.Load(ctx, "input")
	if err != nil {
		t.Errorf("Load %v", err)
	}
	node, ok := state.LookupNodeByPath("foo")
	if !ok {
		t.Fatalf("foo not found")
	}
	edge, ok := node.InEdge()
	if !ok {
		t.Fatalf("no inEdge of foo")
	}
	validations := edge.Validations()
	var got []string
	for _, v := range validations {
		got = append(got, v.Path())
	}
	want := []string{"baz", "baz2"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("validations for foo: -want +got:\n%s", diff)
	}
}

func TestParser_eval_path(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	err := os.WriteFile(filepath.Join(dir, "build.ninja"), []byte(`
root=.

rule configure
  command = ${configure_env}python3 $root/configure.py $configure_args
  generator = 1
build build.ninja: configure | $root/configure.py $root/misc/ninja_syntax.py
`), 0644)
	if err != nil {
		t.Fatal(err)
	}
	state := NewState()
	p := NewManifestParser(state)
	p.SetWd(dir)
	err = p.Load(ctx, "build.ninja")
	if err != nil {
		t.Errorf("Load %v", err)
	}
	node, ok := state.LookupNodeByPath("build.ninja")
	if !ok {
		t.Fatalf("build.ninja not found")
	}
	edge, ok := node.InEdge()
	if !ok {
		t.Fatalf("no inEdge of build.ninja")
	}
	inputs := edge.Inputs()
	var got []string
	for _, in := range inputs {
		got = append(got, in.Path())
	}
	want := []string{"configure.py", "misc/ninja_syntax.py"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("inputs of build.ninja: diff -want +got:\n%s", diff)
	}
}

func TestParser_eval_path_in_build_binding(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	err := os.WriteFile(filepath.Join(dir, "build.ninja"), []byte(`
rule bootstrap
  command = cd "$$(dirname "${builder}")" && touch ${out}

build out/soong/build.ninja: bootstrap $
   | out/soong/build.glob_results $
   ${builder}
 description = analyzing Android.bp files and generating ninja file
 builder = out/soong/host/linux-x86/bin/soong_build
`), 0644)
	if err != nil {
		t.Fatal(err)
	}
	state := NewState()
	p := NewManifestParser(state)
	p.SetWd(dir)
	err = p.Load(ctx, "build.ninja")
	if err != nil {
		t.Errorf("Load %v", err)
	}
	node, ok := state.LookupNodeByPath("out/soong/build.ninja")
	if !ok {
		t.Fatalf("out/soong/build.ninja not found")
	}
	edge, ok := node.InEdge()
	if !ok {
		t.Fatalf("no inEdge of out/soong/build/build.ninja")
	}
	inputs := edge.Inputs()
	var got []string
	for _, in := range inputs {
		got = append(got, in.Path())
	}
	want := []string{"out/soong/build.glob_results", "out/soong/host/linux-x86/bin/soong_build"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("inputs of out/soong/build.ninja: diff -want +got:\n%s", diff)
	}
	command := edge.Binding("command")
	wantCommand := `cd "$(dirname "out/soong/host/linux-x86/bin/soong_build")" && touch out/soong/build.ninja`
	if command != wantCommand {
		t.Errorf("command=%q; want=%q", command, wantCommand)
	}
}

func TestParser_simplevar(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	err := os.WriteFile(filepath.Join(dir, "build.ninja"), []byte(`
root = .
rule re2c
  command = re2c -b -i --no-generation-date --no-version -o $out $in
  description = RE2C $out
build $root/src/depfile_parser.cc: re2c $root/src/depfile_parser.in.cc
`), 0644)
	if err != nil {
		t.Fatal(err)
	}
	state := NewState()
	p := NewManifestParser(state)
	p.SetWd(dir)
	err = p.Load(ctx, "build.ninja")
	if err != nil {
		t.Errorf("Load %v", err)
	}
	node, ok := state.LookupNodeByPath("src/depfile_parser.cc")
	if !ok {
		t.Fatalf("src/depfile_parser.cc not found")
	}
	edge, ok := node.InEdge()
	if !ok {
		t.Fatalf("no inEdge of src/depfile_parser.cc")
	}
	desc := edge.Binding("description")
	want := "RE2C src/depfile_parser.cc"
	if desc != want {
		t.Errorf("description=%q; want=%q", desc, want)
	}
	command := edge.Binding("command")
	want = "re2c -b -i --no-generation-date --no-version -o src/depfile_parser.cc src/depfile_parser.in.cc"
	if command != want {
		t.Errorf("command=%q; want=%q", command, want)
	}
}

func TestParser_space_in_binding(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	err := os.WriteFile(filepath.Join(dir, "build.ninja"), []byte(`
two_words_with_one_space = foo $
    bar
one_words_with_no_space = foo$
    bar

rule foo
    description = $two_words_with_one_space - $one_words_with_no_space

build out: foo in
`), 0644)
	if err != nil {
		t.Fatal(err)
	}
	state := NewState()
	p := NewManifestParser(state)
	p.SetWd(dir)
	err = p.Load(ctx, "build.ninja")
	if err != nil {
		t.Errorf("Load %v", err)
	}
	node, ok := state.LookupNodeByPath("out")
	if !ok {
		t.Fatalf("out not found")
	}
	edge, ok := node.InEdge()
	if !ok {
		t.Fatalf("no inEddge of out")
	}
	desc := edge.Binding("description")
	want := "foo bar - foobar"
	if desc != want {
		t.Errorf("description=%q; want=%q", desc, want)
	}
}

func TestParser_whitespace_in_command(t *testing.T) {
	// https://github.com/ninja-build/ninja/issues/952
	// b/430748593
	ctx := t.Context()
	dir := t.TempDir()
	err := os.WriteFile(filepath.Join(dir, "build.ninja"), []byte(`
rule echo_tab
  command = echo foo$
	bar && touch ${out}

rule echo_space
  command = echo foo$
    bar && touch ${out}

build out: echo_tab
build out2: echo_space
`), 0644)
	if err != nil {
		t.Fatal(err)
	}
	state := NewState()
	p := NewManifestParser(state)
	p.SetWd(dir)
	err = p.Load(ctx, "build.ninja")
	if err != nil {
		t.Errorf("Load %v", err)
	}
	node, ok := state.LookupNodeByPath("out")
	if !ok {
		t.Fatalf("out not found")
	}
	edge, ok := node.InEdge()
	if !ok {
		t.Fatalf("no inEdge of out")
	}
	command := edge.Binding("command")
	want := "echo foo\tbar && touch out"
	if command != want {
		t.Errorf("out command=%q; want=%q", command, want)
	}

	node, ok = state.LookupNodeByPath("out2")
	if !ok {
		t.Fatalf("out2 not found")
	}
	edge, ok = node.InEdge()
	if !ok {
		t.Fatalf("no inEdge of out2")
	}
	command = edge.Binding("command")
	want = "echo foobar && touch out2"
	if command != want {
		t.Errorf("out2 command=%q; want=%q", command, want)
	}
}

func TestParser_Rule_Escapes(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	ninjaContent := `
rule $
  rule_with_space
  command = echo $in > $out
rule$
  rule_without_space
  command = echo $in > $out
rule $
$
 rule_with_double_continuation
  command = echo $in > $out
rule$
$
rule_with_double_continuation_no_space
  command = echo $in > $out
build out1: rule_with_space in
build out2: rule_without_space in
build out3: rule_with_double_continuation in
build out4: rule_with_double_continuation_no_space in
`
	err := os.WriteFile(filepath.Join(dir, "input"), []byte(ninjaContent), 0644)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(filepath.Join(dir, "in"), []byte("input"), 0644)
	if err != nil {
		t.Fatal(err)
	}

	state := NewState()
	p := NewManifestParser(state)
	p.SetWd(dir)
	err = p.Load(ctx, "input")
	if err != nil {
		t.Fatalf("Load %v", err)
	}
	for _, tc := range []struct {
		target   string
		wantRule string
	}{
		{
			target:   "out1",
			wantRule: "rule_with_space",
		},
		{
			target:   "out2",
			wantRule: "rule_without_space",
		},
		{
			target:   "out3",
			wantRule: "rule_with_double_continuation",
		},
		{
			target:   "out4",
			wantRule: "rule_with_double_continuation_no_space",
		},
	} {
		node, ok := state.LookupNodeByPath(tc.target)
		if !ok {
			t.Fatalf("missing %s", tc.target)
		}
		edge, ok := node.InEdge()
		if !ok {
			t.Fatalf("no inEdge for %s", tc.target)
		}
		if edge.RuleName() != tc.wantRule {
			t.Errorf("target %s RuleName=%q; want=%q", tc.target, edge.RuleName(), tc.wantRule)
		}
	}

	// invalid syntax should fail to parse
	for _, tc := range []struct {
		ninjaContent string
	}{
		{
			ninjaContent: "rule$ foo\n  command = echo $in > $out\n",
		},
		{
			ninjaContent: "rule $ foo\n  command = echo $in > $out\n",
		},
		{
			ninjaContent: "rule : \n  command = echo\n",
		},
		{
			ninjaContent: "rule foo:bar\n  command = echo\n",
		},
		{
			ninjaContent: "rule foo bar\n  command = echo\n",
		},
		{
			ninjaContent: "rule$\tfoo\n  command = echo\n",
		},
		{
			ninjaContent: "rule $\tfoo\n  command = echo\n",
		},
		// line continuation inside a name should fail if it is replaced by a space
		{
			ninjaContent: "rule my$\nrule\n  command = echo\n",
		},
	} {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "input"), []byte(tc.ninjaContent), 0644)
		os.WriteFile(filepath.Join(dir, "in"), []byte("input"), 0644)

		state := NewState()
		p := NewManifestParser(state)
		p.SetWd(dir)
		err = p.Load(ctx, "input")
		if err == nil {
			t.Errorf("Load %q nil; want error", tc.ninjaContent)
		}
	}
}

// TestParser_IncludeInLastChunk is a regression test for crrev.com/c/7692413:
// setup() used `if i < len(p.chunks)` to guard access to `p.chunks[i+1]`, but
// the condition should be `if i+1 < len(p.chunks)`. When a file with an
// include directive fit in a single chunk, the loop's only iteration (i=0)
// passed the old guard (0 < 1) and tried to access p.chunks[1], panicking with
// an index out of range.
//
// This test uses a minimal parent file whose only statement is an include
// directive, ensuring a single chunk with ninclude > 0.
func TestParser_IncludeInLastChunk(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()

	err := os.WriteFile(filepath.Join(dir, "rules.ninja"), []byte(`
rule cc
  command = cc -c ${in} -o ${out}
`), 0644)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(filepath.Join(dir, "build.ninja"), []byte(`
include rules.ninja
build obj/a.o: cc a.c
`), 0644)
	if err != nil {
		t.Fatal(err)
	}

	state := NewState()
	p := NewManifestParser(state)
	p.SetWd(dir)
	err = p.Load(ctx, "build.ninja")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	node, ok := state.LookupNodeByPath("obj/a.o")
	if !ok {
		t.Fatal("missing node for obj/a.o")
	}
	edge, ok := node.InEdge()
	if !ok {
		t.Fatal("no inEdge for obj/a.o")
	}
	if got, want := edge.RuleName(), "cc"; got != want {
		t.Errorf("RuleName=%q; want=%q", got, want)
	}
}

// TestParser_IncludeMoreStatementsThanParent is a regression test for
// crrev.com/c/7692414: includeChunks() used to index into ch.statements
// (the parent chunk) instead of cch.statements (the included chunk),
// causing an out-of-bounds panic when the included file had more statements
// than the parent chunk had remaining after the include directive.
func TestParser_IncludeMoreStatementsThanParent(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()

	// The included file has many statements (more than the parent has
	// after its include directive).
	err := os.WriteFile(filepath.Join(dir, "included.ninja"), []byte(`
rule cc
  command = cc -c ${in} -o ${out}

rule link
  command = cc ${in} -o ${out}

build obj/a.o: cc a.c
build obj/b.o: cc b.c
build obj/c.o: cc c.c
build prog: link obj/a.o obj/b.o obj/c.o
`), 0644)
	if err != nil {
		t.Fatal(err)
	}

	// The parent file has only the include directive and nothing after it,
	// so the parent chunk has fewer statements than the included file.
	err = os.WriteFile(filepath.Join(dir, "build.ninja"), []byte(`
include included.ninja
`), 0644)
	if err != nil {
		t.Fatal(err)
	}

	state := NewState()
	p := NewManifestParser(state)
	p.SetWd(dir)
	err = p.Load(ctx, "build.ninja")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	node, ok := state.LookupNodeByPath("prog")
	if !ok {
		t.Fatal("missing node for prog")
	}
	edge, ok := node.InEdge()
	if !ok {
		t.Fatal("no inEdge for prog")
	}
	if got, want := edge.RuleName(), "link"; got != want {
		t.Errorf("RuleName=%q; want=%q", got, want)
	}
	if got, want := len(edge.Inputs()), 3; got != want {
		t.Errorf("len(Inputs)=%d; want=%d", got, want)
	}
}

// TestParser_IncludeResolvesRelativeToWd verifies that include directives are
// resolved relative to the working directory set via SetWd, not the process
// working directory (regression test for crrev.com/c/7692380).
func TestParser_IncludeResolvesRelativeToWd(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()

	err := os.WriteFile(filepath.Join(dir, "included.ninja"), []byte(`
rule cc
  command = cc -c ${in} -o ${out}

build obj/a.o: cc a.c
`), 0644)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(filepath.Join(dir, "build.ninja"), []byte(`
include included.ninja
`), 0644)
	if err != nil {
		t.Fatal(err)
	}

	state := NewState()
	p := NewManifestParser(state)
	p.SetWd(dir)
	err = p.Load(ctx, "build.ninja")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	node, ok := state.LookupNodeByPath("obj/a.o")
	if !ok {
		t.Fatal("missing node for obj/a.o")
	}
	edge, ok := node.InEdge()
	if !ok {
		t.Fatal("no inEdge for obj/a.o")
	}
	if got, want := edge.RuleName(), "cc"; got != want {
		t.Errorf("RuleName=%q; want=%q", got, want)
	}
}

func TestParser_IncludeCycle(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()

	write := func(fname, content string) {
		t.Helper()
		fname = filepath.Join(dir, fname)
		err := os.MkdirAll(filepath.Dir(fname), 0755)
		if err != nil {
			t.Fatal(err)
		}
		err = os.WriteFile(fname, []byte(content), 0644)
		if err != nil {
			t.Fatal(err)
		}
	}

	t.Run("self_include", func(t *testing.T) {
		write("self.ninja", "include self.ninja\n")

		state := NewState()
		p := NewManifestParser(state)
		p.SetWd(dir)
		err := p.Load(ctx, "self.ninja")
		if err == nil {
			t.Fatal("expected error for self-include, got nil")
		}
		t.Logf("got expected error: %v", err)
	})

	t.Run("mutual_include", func(t *testing.T) {
		write("a.ninja", "include b.ninja\n")
		write("b.ninja", "include a.ninja\n")

		state := NewState()
		p := NewManifestParser(state)
		p.SetWd(dir)
		err := p.Load(ctx, "a.ninja")
		if err == nil {
			t.Fatal("expected error for mutual include cycle, got nil")
		}
		t.Logf("got expected error: %v", err)
	})

	t.Run("diamond_include_across_subninjas", func(t *testing.T) {
		write("build.ninja", "subninja sub_a.ninja\nsubninja sub_b.ninja\n")
		write("sub_a.ninja", "include shared.ninja\n")
		write("sub_b.ninja", "include shared.ninja\n")
		write("shared.ninja", "rule cat\n  command = cat $in > $out\n")

		state := NewState()
		p := NewManifestParser(state)
		p.SetWd(dir)
		err := p.Load(ctx, "build.ninja")
		if err != nil {
			t.Fatalf("diamond include across subninjas should not be a cycle, got: %v", err)
		}
	})

	t.Run("diamond_include_within_file", func(t *testing.T) {
		write("main.ninja", "include common.ninja\ninclude common.ninja\n")
		write("common.ninja", "myvar = hello\n")

		state := NewState()
		p := NewManifestParser(state)
		p.SetWd(dir)
		err := p.Load(ctx, "main.ninja")
		if err != nil {
			t.Fatalf("including the same file twice sequentially should not be a cycle, got: %v", err)
		}
	})

	t.Run("symlink_cycle", func(t *testing.T) {
		write("sym_target.ninja", "include sym_link.ninja\n")
		linkPath := filepath.Join(dir, "sym_link.ninja")
		os.Remove(linkPath) // remove if exists from prior test run
		err := os.Symlink(filepath.Join(dir, "sym_target.ninja"), linkPath)
		if err != nil {
			t.Skipf("symlinks not supported: %v", err)
		}

		state := NewState()
		p := NewManifestParser(state)
		p.SetWd(dir)
		err = p.Load(ctx, "sym_target.ninja")
		if err == nil {
			t.Fatal("expected error for symlink include cycle, got nil")
		}
		t.Logf("got expected error: %v", err)
	})
}

// TestParser_IncludeFilenames verifies that files loaded via include
// directives appear in State.Filenames().
func TestParser_IncludeFilenames(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()

	err := os.WriteFile(filepath.Join(dir, "rules.ninja"), []byte(`
rule cc
  command = cc -c ${in} -o ${out}
`), 0644)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(filepath.Join(dir, "build.ninja"), []byte(`
include rules.ninja
build obj/a.o: cc a.c
`), 0644)
	if err != nil {
		t.Fatal(err)
	}

	state := NewState()
	p := NewManifestParser(state)
	p.SetWd(dir)
	err = p.Load(ctx, "build.ninja")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	filenames := state.Filenames()
	slices.Sort(filenames)
	want := []string{
		filepath.Join(dir, "build.ninja"),
		filepath.Join(dir, "rules.ninja"),
	}
	if diff := cmp.Diff(want, filenames); diff != "" {
		t.Errorf("Filenames() mismatch (-want +got):\n%s", diff)
	}
}

// TestParser_LoadFailureReleasesMmaps verifies that when parse fails after
// readFile has registered one or more mmaps on State, Load tears them down
// before returning the error.
func TestParser_LoadFailureReleasesMmaps(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()

	// build.ninja parses cleanly through readFile + setup, then the
	// invalid sub.ninja triggers an error during the include's parse.
	if err := os.WriteFile(filepath.Join(dir, "build.ninja"), []byte(`
rule cc
  command = clang $in -o $out

include sub.ninja

build out: cc src.cc
`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub.ninja"), []byte(`
this is not a valid ninja statement
`), 0644); err != nil {
		t.Fatal(err)
	}

	state := NewState()
	p := NewManifestParser(state)
	p.SetWd(dir)
	if err := p.Load(ctx, "build.ninja"); err == nil {
		t.Fatal("Load: want error from invalid sub.ninja, got nil")
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	if len(state.mmaps) != 0 {
		t.Errorf("after failed Load, state.mmaps has %d mappings; want 0", len(state.mmaps))
	}
}
