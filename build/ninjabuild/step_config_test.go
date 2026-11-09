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
	"go.chromium.org/build/siso/path"
	"go.chromium.org/build/siso/toolsupport/ninjautil"
)

func TestStepConfigUpdateFilegroups_NilInputDeps(t *testing.T) {
	ctx := t.Context()
	sc := &StepConfig{}
	if err := sc.Init(ctx); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	err := sc.UpdateFilegroups(ctx, map[string][]string{
		"includes:headers": {"includes/foo.h", "includes/bar.h"},
	})
	if err != nil {
		t.Fatalf("UpdateFilegroups failed: %v", err)
	}
	if got, want := len(sc.InputDeps), 1; got != want {
		t.Fatalf("len(InputDeps) = %d, want %d", got, want)
	}
	if diff := cmp.Diff([]string{"includes/foo.h", "includes/bar.h"}, sc.InputDeps["includes:headers"]); diff != "" {
		t.Errorf("InputDeps[\"includes:headers\"] diff -want +got:\n%s", diff)
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

	got := sc.ExpandInputs(ctx, p, hashFS, []path.Path{
		"foo/bar",
		"out/Default/gen/out",
		"component:component",
		"extra/file",
	})

	want := []path.Path{
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

func TestStepConfigLookup_NinjaProperties(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	path := build.NewPath(dir, "out/siso")
	err := os.MkdirAll(filepath.Join(dir, "out/siso"), 0755)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(filepath.Join(dir, "out/siso/build.ninja"), []byte(`
rule cxx
  command = g++ -c ${in} -o ${out}
  remote_enabled = true
  remote_platform_ref = custom_ref
  remote_timeout = 2m

rule link
  command = ld ${in} -o ${out}
  remote_enabled = false

rule other
  command = echo ${in} > ${out}
  remote_enabled = true
  remote_platform_ref = custom_ref
  remote_timeout = 5m

rule cxx_simple
  command = g++ -c ${in} -o ${out}

build obj/foo.o: cxx ../../foo.cc
build bin/app: link obj/foo.o
build out/out.txt: other

build obj/bar.o: cxx_simple ../../bar.cc
  remote_enabled = true
  remote_platform_ref = custom_ref
  remote_timeout = 3m
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

	sc := StepConfig{
		Platforms: map[string]map[string]string{
			"default": {
				"container-image": "default-image",
			},
			"custom_ref": {
				"container-image": "custom-image",
			},
		},
		Rules: []*StepRule{
			{
				Name:       "cxx_rule",
				ActionName: "cxx",
				Remote:     false,
			},
			{
				Name:        "link_rule",
				ActionName:  "link",
				Remote:      true,
				PlatformRef: "default",
			},
		},
	}
	err = sc.Init(ctx)
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("Test case 1: cxx_rule. Starlark rule has Remote = false, but ninja file has remote_enabled = true, remote_platform_ref = custom_ref, remote_timeout = 2m.")
	{
		node, ok := state.LookupNodeByPath("obj/foo.o")
		if !ok {
			t.Fatal("obj/foo.o not found")
		}
		edge, ok := node.InEdge()
		if !ok {
			t.Fatal("no inEdge for obj/foo.o")
		}
		rule, ok := sc.Lookup(ctx, path, edge)
		if !ok {
			t.Errorf("Lookup for obj/foo.o failed")
		}
		if !rule.Remote {
			t.Errorf("obj/foo.o rule.Remote = false, want true")
		}
		if rule.Timeout != "2m" {
			t.Errorf("obj/foo.o rule.Timeout = %q, want %q", rule.Timeout, "2m")
		}
		if rule.Platform["container-image"] != "custom-image" {
			t.Errorf("obj/foo.o rule.Platform[\"container-image\"] = %q, want %q", rule.Platform["container-image"], "custom-image")
		}
	}

	t.Logf("Test case 2: link_rule. Starlark rule has Remote = true, but ninja file has remote_enabled = false.")
	{
		node, ok := state.LookupNodeByPath("bin/app")
		if !ok {
			t.Fatal("bin/app not found")
		}
		edge, ok := node.InEdge()
		if !ok {
			t.Fatal("no inEdge for bin/app")
		}
		rule, ok := sc.Lookup(ctx, path, edge)
		if !ok {
			t.Errorf("Lookup for bin/app failed")
		}
		if rule.Remote {
			t.Errorf("bin/app rule.Remote = true, want false")
		}
		if len(rule.Platform) != 0 {
			t.Errorf("bin/app rule.Platform = %v, want empty", rule.Platform)
		}
	}

	t.Logf("Test case 3: other rule. No matching starlark rule, but ninja file has remote_enabled = true, remote_platform_ref = custom_ref, remote_timeout = 5m.")
	{
		node, ok := state.LookupNodeByPath("out/out.txt")
		if !ok {
			t.Fatal("out/out.txt not found")
		}
		edge, ok := node.InEdge()
		if !ok {
			t.Fatal("no inEdge for out/out.txt")
		}
		rule, ok := sc.Lookup(ctx, path, edge)
		if !ok {
			t.Errorf("Lookup for out/out.txt failed")
		}
		if !rule.Remote {
			t.Errorf("out/out.txt rule.Remote = false, want true")
		}
		if rule.Timeout != "5m" {
			t.Errorf("out/out.txt rule.Timeout = %q, want %q", rule.Timeout, "5m")
		}
		if rule.Platform["container-image"] != "custom-image" {
			t.Errorf("out/out.txt rule.Platform[\"container-image\"] = %q, want %q", rule.Platform["container-image"], "custom-image")
		}
	}

	t.Logf("Test case 4: cxx_simple_rule. No starlark config, and ninja has remote_enabled = true, remote_platform_ref = custom_ref, remote_timeout = 3m on the build statement.")
	{
		node, ok := state.LookupNodeByPath("obj/bar.o")
		if !ok {
			t.Fatal("obj/bar.o not found")
		}
		edge, ok := node.InEdge()
		if !ok {
			t.Fatal("no inEdge for obj/bar.o")
		}
		rule, ok := sc.Lookup(ctx, path, edge)
		if !ok {
			t.Errorf("Lookup for obj/bar.o failed")
		}
		if !rule.Remote {
			t.Errorf("obj/bar.o rule.Remote = false, want true")
		}
		if rule.Timeout != "3m" {
			t.Errorf("obj/bar.o rule.Timeout = %q, want %q", rule.Timeout, "3m")
		}
		if rule.Platform["container-image"] != "custom-image" {
			t.Errorf("obj/bar.o rule.Platform[\"container-image\"] = %q, want %q", rule.Platform["container-image"], "custom-image")
		}
	}
}

func TestStepConfigLookup_NinjaOutputLocal(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	path := build.NewPath(dir, "out/siso")
	err := os.MkdirAll(filepath.Join(dir, "out/siso"), 0755)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(filepath.Join(dir, "out/siso/build.ninja"), []byte(`
rule metalava
  command = metalava ${in} -o ${out}
  output_local = true

rule javac
  command = javac ${in} -o ${out}

rule stamp
  command = touch ${out}

rule other
  command = echo ${in} > ${out}
  remote_enabled = true

rule other_local
  command = echo ${in} > ${out}

build gen/foo_api.txt: metalava ../../foo.java
build gen/bar.jar: javac ../../bar.java
  output_local = false
build gen/baz.jar: javac ../../baz.java
build gen/foo.stamp: stamp ../../foo.java
  output_local = true
build gen/out.txt: other
  output_local = true
build gen/local.txt: other_local
  output_local = true
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

	sc := StepConfig{
		Platforms: map[string]map[string]string{
			"default": {
				"container-image": "default-image",
			},
		},
		Rules: []*StepRule{
			{
				Name:       "metalava_rule",
				ActionName: "metalava",
				Remote:     true,
			},
			{
				Name:        "javac_rule",
				ActionName:  "javac",
				Remote:      true,
				OutputLocal: true,
			},
			{
				Name:       "stamp_rule",
				ActionName: "stamp",
			},
		},
	}
	err = sc.Init(ctx)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name            string
		desc            string
		out             string
		wantOK          bool
		wantOutputLocal bool
	}{
		{
			name:            "ninja_rule_true",
			desc:            "starlark rule without output_local, ninja rule has output_local = true",
			out:             "gen/foo_api.txt",
			wantOK:          true,
			wantOutputLocal: true,
		},
		{
			name:            "ninja_build_false_overrides_starlark",
			desc:            "starlark rule has output_local, ninja build has output_local = false",
			out:             "gen/bar.jar",
			wantOK:          true,
			wantOutputLocal: false,
		},
		{
			name:            "starlark_without_binding",
			desc:            "starlark rule has output_local, no ninja binding",
			out:             "gen/baz.jar",
			wantOK:          true,
			wantOutputLocal: true,
		},
		{
			name:            "ninja_build_true_on_local_rule",
			desc:            "local starlark rule, ninja build has output_local = true",
			out:             "gen/foo.stamp",
			wantOK:          true,
			wantOutputLocal: true,
		},
		{
			name:            "ninja_build_true_with_remote_enabled",
			desc:            "no starlark rule, ninja has remote_enabled = true and output_local = true",
			out:             "gen/out.txt",
			wantOK:          true,
			wantOutputLocal: true,
		},
		{
			name:            "ninja_build_true_only",
			desc:            "no starlark rule, ninja has only output_local = true",
			out:             "gen/local.txt",
			wantOK:          false,
			wantOutputLocal: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node, ok := state.LookupNodeByPath(tc.out)
			if !ok {
				t.Fatalf("%s not found", tc.out)
			}
			edge, ok := node.InEdge()
			if !ok {
				t.Fatalf("no inEdge for %s", tc.out)
			}
			rule, ok := sc.Lookup(ctx, path, edge)
			if ok != tc.wantOK || rule.OutputLocal != tc.wantOutputLocal {
				t.Errorf("%s: Lookup(ctx, path, edge)=rule.OutputLocal=%t, %t; want %t, %t", tc.desc, rule.OutputLocal, ok, tc.wantOutputLocal, tc.wantOK)
			}
		})
	}
}

func TestStepConfigInit_PlatformRefValidation(t *testing.T) {
	ctx := t.Context()

	// Case 1: valid config
	sc := &StepConfig{
		Platforms: map[string]map[string]string{
			"default": {"OS": "linux"},
		},
		Rules: []*StepRule{
			{
				Name:          "cxx",
				CommandPrefix: "g++",
				PlatformRef:   "default",
			},
		},
	}
	if err := sc.Init(ctx); err != nil {
		t.Errorf("Init failed with valid platform_ref: %v", err)
	}

	// Case 2: invalid platform_ref in rule
	scInvalidRule := &StepConfig{
		Rules: []*StepRule{
			{
				Name:          "cxx",
				CommandPrefix: "g++",
				PlatformRef:   "missing_platform",
			},
		},
	}
	if err := scInvalidRule.Init(ctx); err == nil {
		t.Error("Init succeeded with invalid platform_ref in rule, but should have failed")
	}

	// Case 3: invalid platform_ref in outputs_map
	scInvalidOutputsMap := &StepConfig{
		Rules: []*StepRule{
			{
				Name:          "cxx",
				CommandPrefix: "g++",
				OutputsMap: map[string]StepDeps{
					"foo.o": {
						PlatformRef: "missing_platform",
					},
				},
			},
		},
	}
	if err := scInvalidOutputsMap.Init(ctx); err == nil {
		t.Error("Init succeeded with invalid platform_ref in outputs_map, but should have failed")
	}
}

func TestStepConfigLookup_StrictRemote(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	path := build.NewPath(dir, "out/siso")
	err := os.MkdirAll(filepath.Join(dir, "out/siso"), 0755)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(filepath.Join(dir, "out/siso/build.ninja"), []byte(`
rule cxx
  command = g++ -c ${in} -o ${out}

build obj/foo.o: cxx ../../foo.cc
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

	sc := StepConfig{
		Rules: []*StepRule{
			{
				Name:         "cxx_rule",
				ActionName:   "cxx",
				Remote:       true,
				StrictRemote: true,
			},
		},
	}
	err = sc.Init(ctx)
	if err != nil {
		t.Fatal(err)
	}

	node, ok := state.LookupNodeByPath("obj/foo.o")
	if !ok {
		t.Fatal("obj/foo.o not found")
	}
	edge, ok := node.InEdge()
	if !ok {
		t.Fatal("no inEdge for obj/foo.o")
	}
	rule, ok := sc.Lookup(ctx, path, edge)
	if !ok {
		t.Errorf("Lookup for obj/foo.o failed")
	}
	if !rule.StrictRemote {
		t.Errorf("obj/foo.o rule.StrictRemote = false, want true")
	}
}

func TestStepConfigInit_Env(t *testing.T) {
	for _, tc := range []struct {
		name    string
		env     *build.EnvConfig
		wantErr bool
	}{
		{
			name: "nil_env",
		},
		{
			name: "excludes_omits_and_substitutions",
			env: &build.EnvConfig{
				Excludes:      []string{"RBE_metrics_project"},
				CacheKeyOmits: []string{"HOME", "USER", "HOSTNAME", "TOP", "CIPD_PROXY_URL"},
				CacheKeySubstitutions: map[string]string{
					"/path/to/workspace": "CWD",
				},
				CacheKeyTrimPrefixes: []string{"CWD/"},
			},
		},
		{
			name: "inherits_with_omits_and_substitutions",
			env: &build.EnvConfig{
				Inherits:      []string{"FOO", "BAR", "HOME"},
				CacheKeyOmits: []string{"HOME"},
				CacheKeySubstitutions: map[string]string{
					"/path/to/workspace": "CWD",
				},
				CacheKeyTrimPrefixes: []string{"CWD/"},
			},
		},
		{
			name: "error_both_inherits_and_excludes",
			env: &build.EnvConfig{
				Inherits: []string{"FOO"},
				Excludes: []string{"HOME"},
			},
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			sc := StepConfig{Env: tc.env}
			err := sc.Init(ctx)
			if gotErr := err != nil; gotErr != tc.wantErr {
				t.Errorf("sc.Init(ctx) = %v; gotErr %t, wantErr %t", err, gotErr, tc.wantErr)
			}
		})
	}
}
