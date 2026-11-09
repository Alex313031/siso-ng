// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjabuild

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"go.chromium.org/build/hashigo/digest"

	"go.chromium.org/build/siso/blob"
	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/build/buildconfig"
	"go.chromium.org/build/siso/execute"
	"go.chromium.org/build/siso/hashfs"
	pb "go.chromium.org/build/siso/hashfs/proto"
	"go.chromium.org/build/siso/path"
	"go.chromium.org/build/siso/toolsupport/ninjautil"
)

// TestLoad_RejectsNestedDirOutput verifies Load errors when one step produces
// an output nested under another step's directory output. Only a single
// producer of a whole directory tree is supported, not a merge of producers.
func TestLoad_RejectsNestedDirOutput(t *testing.T) {
	for _, tc := range []struct {
		name     string
		manifest string
	}{
		{
			name: "different_producer",
			// gendir produces the whole tree gen/; gen produces a file nested
			// inside it. A second producer inside the tree is unsupported.
			manifest: `
rule gendir
  command = mkdir -p ${out}
rule gen
  command = touch ${out}

build gen/: gendir
build gen/foo.txt: gen
build all: phony gen/ gen/foo.txt
`,
		},
		{
			name: "same_producer",
			// One step declares gen/ and a file inside it. The directory output
			// already owns its whole tree, so a member must not be declared as
			// its own output even by the same step.
			manifest: `
rule gendir
  command = mkdir -p ${out} && touch ${out}foo.txt

build gen/ gen/foo.txt: gendir
build all: phony gen/
`,
		},
		{
			name: "nested_dir_output",
			// a nested directory output is itself an output inside gen/.
			manifest: `
rule gendir
  command = mkdir -p ${out}

build gen/: gendir
build gen/sub/: gendir
build all: phony gen/ gen/sub/
`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, "out/siso"), 0755); err != nil {
				t.Fatal(err)
			}
			t.Chdir(filepath.Join(dir, "out/siso"))
			path := build.NewPath(dir, "out/siso")
			if err := os.WriteFile(filepath.Join(dir, "out/siso/build.ninja"), []byte(tc.manifest), 0644); err != nil {
				t.Fatal(err)
			}
			_, err := Load(ctx, "build.ninja", path)
			if err == nil {
				t.Fatal("Load succeeded; want error for an output nested under a directory output")
			}
			if !strings.Contains(err.Error(), "directory output") {
				t.Errorf("Load error = %v; want it to flag the directory-output overlap", err)
			}
		})
	}
}

// TestLoad_AllowsValidDirOutputs verifies Load accepts the supported directory
// output shapes, so the nesting check is not over-broad: a consumer depends on
// the whole tree, and a sibling sharing a path prefix is not "inside" the tree.
func TestLoad_AllowsValidDirOutputs(t *testing.T) {
	for _, tc := range []struct {
		name     string
		manifest string
	}{
		{
			name: "consumer_depends_on_whole_tree",
			// gendir owns gen/; cp consumes the whole tree, not a member.
			manifest: `
rule gendir
  command = mkdir -p ${out}
rule cp
  command = cp -r ${in} ${out}

build gen/: gendir
build out.txt: cp gen/
build all: phony out.txt
`,
		},
		{
			name: "sibling_shares_prefix",
			// generated.txt shares the "gen" prefix but is not under gen/.
			manifest: `
rule gendir
  command = mkdir -p ${out}
rule gen
  command = touch ${out}

build gen/: gendir
build generated.txt: gen
build all: phony gen/ generated.txt
`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, "out/siso"), 0755); err != nil {
				t.Fatal(err)
			}
			t.Chdir(filepath.Join(dir, "out/siso"))
			path := build.NewPath(dir, "out/siso")
			if err := os.WriteFile(filepath.Join(dir, "out/siso/build.ninja"), []byte(tc.manifest), 0644); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(ctx, "build.ninja", path); err != nil {
				t.Errorf("Load() = %v; want success for a valid directory-output graph", err)
			}
		})
	}
}

// TestOutermostPaths_Cleandead exercises execute.OutermostPaths on cleandead inputs: a directory output must carry its whole subtree as one unit.
func TestOutermostPaths_Cleandead(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []path.Path
		want []path.Path
	}{
		{
			name: "directory_contents_not_judged_individually",
			in:   []path.Path{"gen/foo", "gen/foo/a.o", "gen/foo/sub/b.o"},
			want: []path.Path{"gen/foo"},
		},
		{
			// A sibling file output sorts between the directory root and its
			// contents ('.' 0x2E < '/' 0x2F); the skip prefix must not reset
			// and expose gen/foo/a.o to individual judgement.
			name: "interleaved_sibling_does_not_expose_dir_contents",
			in:   []path.Path{"gen/foo", "gen/foo.stamp", "gen/foo/a.o", "gen/foo/sub/b.o"},
			want: []path.Path{"gen/foo", "gen/foo.stamp"},
		},
		{
			name: "independent_siblings_all_judged",
			in:   []path.Path{"a.o", "b.o", "c.o"},
			want: []path.Path{"a.o", "b.o", "c.o"},
		},
		{
			name: "two_directory_outputs_side_by_side",
			in:   []path.Path{"gen/a", "gen/a/x", "gen/b", "gen/b/y"},
			want: []path.Path{"gen/a", "gen/b"},
		},
		{
			name: "sibling_with_char_above_slash_sorts_after_subtree",
			in:   []path.Path{"gen/foo", "gen/foo/a.o", "gen/foo0"},
			want: []path.Path{"gen/foo", "gen/foo0"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := execute.OutermostPaths(tc.in)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("OutermostPaths(%v) (-want +got):\n%s", tc.in, diff)
			}
		})
	}
}

// TestCleanDead_LeavesUndeletableDeadOutput verifies dead-output cleanup is
// best-effort: a dead file whose removal fails (e.g. held open by another
// process on Windows, or an unwritable parent here) is left behind with a
// warning, while other dead files are still removed and the build proceeds.
func TestCleanDead_LeavesUndeletableDeadOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("read-only directories do not block child removal on windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory write permissions")
	}
	ctx := t.Context()
	dir := t.TempDir()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "build/config/siso"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "build/config/siso/main.star"), []byte(`
load("@builtin//struct.star", "module")

def init(ctx):
  return module(
    "config",
    step_config = "{}",
    filegroups = {},
    handlers = {},
  )
`), 0644); err != nil {
		t.Fatal(err)
	}
	config, err := buildconfig.New(ctx, "@config//main.star", map[string]string{}, map[string]fs.FS{
		"config":           os.DirFS(filepath.Join(dir, "build/config/siso")),
		"config_overrides": os.DirFS(filepath.Join(dir, ".siso_remote")),
	})
	if err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(dir, "out/siso")
	if err := os.MkdirAll(filepath.Join(outDir, "gen"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(outDir)
	path := build.NewPath(dir, "out/siso")
	if err := os.WriteFile(filepath.Join(outDir, "build.ninja"), []byte(`
rule touch
  command = touch ${out}

build foo.o: touch foo.cc
`), 0644); err != nil {
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

	// Two dead generated files from a previous build: one in the read-only
	// out dir root (removal fails), one in a writable subdir (removal works).
	var entries []*pb.Entry
	for name, content := range map[string]string{
		"dead.o":      "dead",
		"gen/dead2.o": "dead2",
	} {
		fname := filepath.Join(outDir, name)
		if err := os.WriteFile(fname, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Lstat(fname)
		if err != nil {
			t.Fatal(err)
		}
		d := blob.FromBytes(digest.SHA256, name, []byte(content)).Digest()
		entries = append(entries, &pb.Entry{
			Id:      &pb.FileID{ModTime: fi.ModTime().UnixNano()},
			Name:    filepath.ToSlash(fname),
			Digest:  &pb.Digest{Hash: d.Hash, SizeBytes: d.SizeBytes},
			CmdHash: []byte("cmdhash"),
		})
	}
	if err := hashFS.SetState(ctx, &pb.State{Entries: entries}); err != nil {
		t.Fatal(err)
	}

	depsLog, err := NewDepsLog(ctx, ".siso_deps")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := depsLog.Close(); err != nil {
			t.Error(err)
		}
	}()
	stepConfig, err := NewStepConfig(ctx, digest.SHA256, config, path, "build.ninja", ".")
	if err != nil {
		t.Fatal(err)
	}
	nstate, err := Load(ctx, "build.ninja", path)
	if err != nil {
		t.Fatal(err)
	}
	g, err := NewGraph(ctx, "build.ninja", nstate, config, path, hashFS, stepConfig, depsLog)
	if err != nil {
		t.Fatal(err)
	}

	// Make dead.o undeletable via its read-only parent.
	if err := os.Chmod(outDir, 0555); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chmod(outDir, 0755); err != nil {
			t.Error(err)
		}
	}()

	n, total, err := g.CleanDead(ctx)
	if err != nil {
		t.Fatalf("CleanDead=%d, %d, %v; want nil error (an undeletable dead output must not fail the build)", n, total, err)
	}
	if got, want := n, 1; got != want {
		t.Errorf("CleanDead removed %d; want %d", got, want)
	}
	if _, err := os.Lstat(filepath.Join(outDir, "dead.o")); err != nil {
		t.Errorf("Lstat(dead.o)=%v; want the undeletable dead file left in place", err)
	}
	if _, err := os.Lstat(filepath.Join(outDir, "gen/dead2.o")); !os.IsNotExist(err) {
		t.Errorf("Lstat(gen/dead2.o) err=%v; want not-exist (the deletable dead file is still removed)", err)
	}
	// The undeletable file must survive in saved state, so it re-enters
	// PreviouslyGeneratedFiles and the next build's clean-dead retries it.
	m := hashfs.StateMap(hashFS.DigestFunction(), hashFS.State(ctx))
	if _, ok := m[filepath.ToSlash(filepath.Join(outDir, "dead.o"))]; !ok {
		t.Errorf("dead.o missing from saved state; the next build's clean-dead will never retry removing it")
	}
	if _, ok := m[filepath.ToSlash(filepath.Join(outDir, "gen/dead2.o"))]; ok {
		t.Errorf("gen/dead2.o still in saved state; want dropped (it was removed)")
	}
}

func TestTargets(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	err := os.MkdirAll(filepath.Join(dir, "build/config/siso"), 0755)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(filepath.Join(dir, "build/config/siso/main.star"), []byte(`
load("@builtin//struct.star", "module")

def init(ctx):
  return module(
    "config",
    step_config = "{}",
    filegroups = {},
    handlers = {},
  )
`), 0644)
	if err != nil {
		t.Fatal(err)
	}
	config, err := buildconfig.New(ctx, "@config//main.star", map[string]string{}, map[string]fs.FS{
		"config":           os.DirFS(filepath.Join(dir, "build/config/siso")),
		"config_overrides": os.DirFS(filepath.Join(dir, ".siso_remote")),
	})
	if err != nil {
		t.Fatal(err)
	}
	err = os.MkdirAll(filepath.Join(dir, "out/siso"), 0755)
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(filepath.Join(dir, "out/siso"))
	path := build.NewPath(dir, "out/siso")
	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(filepath.Join(dir, "out/siso/build.ninja"), []byte(`
rule cxx
  command =  ../../third_party/llvm-build/Release+Asserts/bin/clang++ -MMD -MF ${out}.d ${defines} ${include_dirs} ${cflags} ${cflags_cc} -c ${in} -o ${out}
  description = CXX ${out}
  depfile = ${out}.d
  deps = gcc

rule link
  command = "python3" "../../build/toolchain/gcc_link_wrapper.py" --output="${output_dir}/${target_output_name}${output_extension}" -- ../../third_party/llvm-build/Release+Asserts/bin/clang++ ${ldflags} -o "${output_dir}/${target_output_name}${output_extension}" -Wl,--start-group @"${output_dir}/${target_output_name}${output_extension}.rsp" ${solibs} -Wl,--end-group  ${libs} ${rlibs}
  description = LINK ${output_dir}/${target_output_name}${output_extension}
  rspfile = ${output_dir}/${target_output_name}${output_extension}.rsp
  rspfile_content = ${in}

build foo.o: cxx foo.cc
build bar.o: cxx bar.cc
build exe: link foo.o bar.o
build all: phony exe
`), 0644)
	if err != nil {
		t.Fatal(err)
	}
	depsLog, err := NewDepsLog(ctx, ".siso_deps")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		err := depsLog.Close()
		if err != nil {
			t.Error(err)
		}
	}()
	stepConfig, err := NewStepConfig(ctx, digest.SHA256, config, path, "build.ninja", ".")
	if err != nil {
		t.Fatal(err)
	}

	nstate, err := Load(ctx, "build.ninja", path)
	if err != nil {
		t.Fatal(err)
	}

	g, err := NewGraph(ctx, "build.ninja", nstate, config, path, hashFS, stepConfig, depsLog)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		args []string
		want []string
	}{
		{
			args: nil,
			want: []string{"out/siso/all"},
		},
		{
			args: []string{"all"},
			want: []string{"out/siso/all"},
		},
		{
			args: []string{"foo.o"},
			want: []string{"out/siso/foo.o"},
		},
		{
			args: []string{"foo.cc^"},
			want: []string{"out/siso/foo.o"},
		},
	} {
		targets, err := g.Targets(ctx, tc.args...)
		if err != nil {
			t.Errorf("g.Targets(ctx, %q)=%v, %v; want nil err", tc.args, targets, err)
			continue
		}
		var got []string
		for _, target := range targets {
			p, err := g.TargetPath(ctx, target)
			if err != nil {
				t.Errorf("g.TargetPath(%v)=%v, %v; want nil err", target, p, err)
			}
			got = append(got, p)
		}
		if diff := cmp.Diff(tc.want, got); diff != "" {
			t.Errorf("g.Targets(ctx, %q) diff -want +got:\n%s", tc.args, diff)
		}
	}
}

func TestNewStepConfig_RejectUnknownKeys(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	err := os.MkdirAll(filepath.Join(dir, "build/config/siso"), 0755)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(filepath.Join(dir, "build/config/siso/main.star"), []byte(`
load("@builtin//struct.star", "module")

strict_config = True

def init(ctx):
  return module(
    "config",
    step_config = '{"unknown_key_test": "value"}',
    filegroups = {},
    handlers = {},
  )
`), 0644)
	if err != nil {
		t.Fatal(err)
	}
	config, err := buildconfig.New(ctx, "@config//main.star", map[string]string{}, map[string]fs.FS{
		"config":           os.DirFS(filepath.Join(dir, "build/config/siso")),
		"config_overrides": os.DirFS(filepath.Join(dir, ".siso_remote")),
	})
	if err != nil {
		t.Fatal(err)
	}
	err = os.MkdirAll(filepath.Join(dir, "out/siso"), 0755)
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(filepath.Join(dir, "out/siso"))
	path := build.NewPath(dir, "out/siso")

	err = os.WriteFile(filepath.Join(dir, "out/siso/build.ninja"), []byte(`
build all: phony
`), 0644)
	if err != nil {
		t.Fatal(err)
	}

	_, err = NewStepConfig(ctx, digest.SHA256, config, path, "build.ninja", ".")
	if err == nil {
		t.Error("NewStepConfig succeeded with unknown keys, but should have failed")
	}
}

func TestInputDeps_InputsLabel(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	err := os.MkdirAll(filepath.Join(dir, "build/config/siso"), 0755)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(filepath.Join(dir, "build/config/siso/main.star"), []byte(`
load("@builtin//struct.star", "module")

def init(ctx):
  return module(
    "config",
    step_config = '{"input_deps": {"out/siso/ndk/sysroot:headers": ["out/siso/ndk_headers_phony:inputs"]}}',
    filegroups = {},
    handlers = {},
  )
`), 0644)
	if err != nil {
		t.Fatal(err)
	}
	config, err := buildconfig.New(ctx, "@config//main.star", map[string]string{}, map[string]fs.FS{
		"config":           os.DirFS(filepath.Join(dir, "build/config/siso")),
		"config_overrides": os.DirFS(filepath.Join(dir, ".siso_remote")),
	})
	if err != nil {
		t.Fatal(err)
	}
	err = os.MkdirAll(filepath.Join(dir, "out/siso"), 0755)
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(filepath.Join(dir, "out/siso"))
	path := build.NewPath(dir, "out/siso")
	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := hashFS.Close(context.WithoutCancel(ctx)); err != nil {
			t.Errorf("hashFS.Close=%v", err)
		}
	})

	err = os.WriteFile(filepath.Join(dir, "out/siso/build.ninja"), []byte(`
rule cp
  command = cp ${in} ${out}

build ndk/sysroot/usr/include/a.h: cp ../../ndk/a.h
build ndk/sysroot/usr/include/b.h: cp ../../ndk/b.h
build ndk/sysroot/usr/include/c.h: cp ../../ndk/c.h
build ndk_headers_sub1: phony ndk/sysroot/usr/include/a.h
build ndk_headers_sub2: phony ndk/sysroot/usr/include/b.h
build ndk_headers_phony: phony ndk_headers_sub1 ndk_headers_sub2 ndk/sysroot/usr/include/c.h
build all: phony ndk_headers_phony
`), 0644)
	if err != nil {
		t.Fatal(err)
	}
	depsLog, err := NewDepsLog(ctx, ".siso_deps")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := depsLog.Close(); err != nil {
			t.Error(err)
		}
	}()
	stepConfig, err := NewStepConfig(ctx, digest.SHA256, config, path, "build.ninja", ".")
	if err != nil {
		t.Fatal(err)
	}
	nstate, err := Load(ctx, "build.ninja", path)
	if err != nil {
		t.Fatal(err)
	}
	g, err := NewGraph(ctx, "build.ninja", nstate, config, path, hashFS, stepConfig, depsLog)
	if err != nil {
		t.Fatal(err)
	}

	got := g.InputDeps(ctx)["out/siso/ndk_headers_phony:inputs"]
	want := []string{
		"out/siso/ndk/sysroot/usr/include/a.h",
		"out/siso/ndk/sysroot/usr/include/b.h",
		"out/siso/ndk/sysroot/usr/include/c.h",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("InputDeps[\"out/siso/ndk_headers_phony:inputs\"] diff -want +got:\n%s", diff)
	}
}

func setupFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for k, v := range files {
		fname := filepath.Join(dir, k)
		err := os.MkdirAll(filepath.Dir(fname), 0755)
		if err != nil {
			t.Fatal(err)
		}
		mode := fs.FileMode(0644)
		if strings.HasPrefix(k, "bin/") || strings.HasPrefix(v, "#!") {
			mode = 0755
		}
		err = os.WriteFile(fname, []byte(v), mode)
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestGraph_ShellFromStepConfig(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("These tests don't work on windows")
	}
	ctx := t.Context()
	dir := t.TempDir()
	setupFiles(t, dir, map[string]string{
		"bin/sh":   "#!/bin/sh\nexit 0\n",
		"bin/bash": "#!/bin/bash\nexit 0\n",
		"build/config/siso/main.star": `
load("@builtin//encoding.star", "json")
load("@builtin//struct.star", "module")

def init(ctx):
  step_config = {
    "shell": "bin/sh",
  }
  return module(
    "config",
    step_config = json.encode(step_config),
    filegroups = {},
    handlers = {},
  )
`,
		"out/siso/build.ninja": `
rule nontrivial
  command = cat ${in} > ${out} && echo "nontrivial" >> ${out}

build out.txt: nontrivial in.txt
`,
	})

	config, err := buildconfig.New(ctx, "@config//main.star", map[string]string{}, map[string]fs.FS{
		"config":           os.DirFS(filepath.Join(dir, "build/config/siso")),
		"config_overrides": os.DirFS(filepath.Join(dir, ".siso_remote")),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(filepath.Join(dir, "out/siso"))
	path := build.NewPath(dir, "out/siso")
	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := hashFS.Close(context.WithoutCancel(ctx)); err != nil {
			t.Errorf("hashFS.Close=%v", err)
		}
	})

	depsLog, err := NewDepsLog(ctx, ".siso_deps")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := depsLog.Close(); err != nil {
			t.Error(err)
		}
	}()
	stepConfig, err := NewStepConfig(ctx, digest.SHA256, config, path, "build.ninja", ".")
	if err != nil {
		t.Fatal(err)
	}
	if stepConfig.Shell != "bin/sh" {
		t.Errorf("stepConfig.Shell = %q, want \"bin/sh\"", stepConfig.Shell)
	}

	nstate, err := Load(ctx, "build.ninja", path)
	if err != nil {
		t.Fatal(err)
	}
	g, err := NewGraph(ctx, "build.ninja", nstate, config, path, hashFS, stepConfig, depsLog)
	if err != nil {
		t.Fatal(err)
	}
	if g.Shell() != "bin/sh" {
		t.Errorf("g.Shell() = %q, want \"bin/sh\"", g.Shell())
	}

	node, _ := nstate.LookupNodeByPath("out.txt")
	edge, _ := node.InEdge()
	stepDef := g.newStepDef(ctx, edge, nil)

	args := stepDef.Args(ctx)
	if len(args) < 1 || args[0] != "../../bin/sh" {
		t.Errorf("stepDef.Args(ctx)[0] = %q, want \"../../bin/sh\"", args)
	}
	if !slices.Contains(stepDef.Inputs(ctx), "bin/sh") {
		t.Errorf("stepDef.Inputs(ctx) should contain bin/sh, got %v", stepDef.Inputs(ctx))
	}
}

func TestGraph_Shell_Validation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("These tests don't work on windows")
	}
	ctx := t.Context()
	dir := t.TempDir()
	setupFiles(t, dir, map[string]string{
		"bin/sh":        "#!/bin/sh\nexit 0\n",
		"bin/bash":      "#!/bin/bash\nexit 0\n",
		"bin/zsh":       "#!/bin/zsh\nexit 0\n",
		"bin/noexec/sh": "echo non-executable\n",
		"build/config/siso/main.star": `
load("@builtin//struct.star", "module")

def init(ctx):
  return module(
    "config",
    step_config = "{}",
    filegroups = {},
    handlers = {},
  )
`,
		"out/siso/build.ninja": `
build all: phony
`,
	})
	if err := os.Chmod(filepath.Join(dir, "bin/noexec/sh"), 0644); err != nil {
		t.Fatal(err)
	}

	config, err := buildconfig.New(ctx, "@config//main.star", map[string]string{}, map[string]fs.FS{
		"config":           os.DirFS(filepath.Join(dir, "build/config/siso")),
		"config_overrides": os.DirFS(filepath.Join(dir, ".siso_remote")),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(filepath.Join(dir, "out/siso"))
	path := build.NewPath(dir, "out/siso")
	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := hashFS.Close(context.WithoutCancel(ctx)); err != nil {
			t.Errorf("hashFS.Close=%v", err)
		}
	})

	depsLog, err := NewDepsLog(ctx, ".siso_deps")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := depsLog.Close(); err != nil {
			t.Error(err)
		}
	}()
	nstate, err := Load(ctx, "build.ninja", path)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name    string
		shell   string
		wantErr error
	}{
		{
			name:    "empty",
			shell:   "",
			wantErr: nil,
		},
		{
			name:    "valid_sh",
			shell:   "bin/sh",
			wantErr: nil,
		},
		{
			name:    "valid_bash",
			shell:   "bin/bash",
			wantErr: nil,
		},
		{
			name:    "not_clean",
			shell:   "./bin/sh",
			wantErr: errShellPathNotCleanOrLocal,
		},
		{
			name:    "not_local_parent",
			shell:   "../../bin/sh",
			wantErr: errShellPathNotCleanOrLocal,
		},
		{
			name:    "not_local_abs",
			shell:   "/bin/sh",
			wantErr: errShellPathNotCleanOrLocal,
		},
		{
			name:    "invalid_basename",
			shell:   "bin/zsh",
			wantErr: errInvalidShellBasename,
		},
		{
			name:    "not_found",
			shell:   "bin/nonexistent/sh",
			wantErr: fs.ErrNotExist,
		},
		{
			name:    "not_executable",
			shell:   "bin/noexec/sh",
			wantErr: errShellNotExecutable,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stepConfig := &StepConfig{Shell: tc.shell}
			_, err := NewGraph(ctx, "build.ninja", nstate, config, path, hashFS, stepConfig, depsLog)
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("NewGraph(..., shell=%q) err = %v; want %v", tc.shell, err, tc.wantErr)
			}
		})
	}
}

func TestGraph_Shell_Windows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("This test only works on windows")
	}
	ctx := t.Context()
	dir := t.TempDir()
	path := build.NewPath(dir, "out/siso")
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
	stepConfig := &StepConfig{Shell: "bin/sh"}
	depsLog := &DepsLog{}
	_, err = NewGraph(ctx, "build.ninja", nstate, nil, path, hashFS, stepConfig, depsLog)
	if !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("NewGraph on windows with shell err = %v, want %v", err, errors.ErrUnsupported)
	}
}
