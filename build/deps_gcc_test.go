// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/google/go-cmp/cmp"

	"go.chromium.org/build/siso/execute"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/reapi/merkletree"
)

type fakeGraph struct {
	inputDeps map[string][]string
}

func (g fakeGraph) NumTargets() int { return len(g.inputDeps) }

func (fakeGraph) Targets(ctx context.Context, args ...string) ([]Target, error) {
	return nil, nil
}

func (fakeGraph) SpellcheckTarget(string) (string, error) { return "", errors.New("spellcheck error") }

func (fakeGraph) TargetPath(ctx context.Context, target Target) (string, error) {
	return "", errors.New("not implemented")
}

func (fakeGraph) Edge(ctx context.Context, target Target, next StepDef) (*Edge, error) {
	return nil, errors.New("not implemented")
}

func (g fakeGraph) InputDeps(ctx context.Context) map[string][]string {
	return g.inputDeps
}

func (fakeGraph) InputsRequiringClangScandeps(ctx context.Context) []string { return nil }

func (g fakeGraph) StepLimits(ctx context.Context) map[string]int {
	return map[string]int{}
}

func (g fakeGraph) Binding(key string) string { return "" }

func (g fakeGraph) Filenames() []string { return []string{"build.ninja"} }

func TestDepsGCCFixCmdInputs_ios(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("depsGCC is not used on windows")
		return
	}
	ctx := t.Context()
	dir := t.TempDir()
	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}

	setupDir := func(dirname string) {
		t.Helper()
		fullpath := filepath.Join(dir, dirname)
		err := os.MkdirAll(fullpath, 0755)
		if err != nil {
			t.Fatal(err)
		}
	}

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
	setupSymlink := func(fname, target string) {
		t.Helper()
		fullpath := filepath.Join(dir, fname)
		err := os.MkdirAll(filepath.Dir(fullpath), 0755)
		if err != nil {
			t.Fatal(err)
		}
		err = os.Symlink(target, fullpath)
		if err != nil {
			t.Fatal(err)
		}
	}

	setupDir("out/siso")
	setupDir("out/siso/gen")
	setupDir("out/siso/obj/base")
	setupFile("base/base.cc")
	setupFile("third_party/llvm-build/Release+Asserts/bin/clang")
	setupFile("third_party/llvm-build/Release+Asserts/bin/clang++")
	setupFile("buildtools/third_party/libc++/trunk/include/stdout.h")
	setupFile("build/mac_files/xcode_binaries/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX.sdk/somefile")
	setupSymlink("build/mac_files/xcode_binaries/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX13.3.sdk", "MacOSX.sdk")

	b := &Builder{

		path:   NewPath(dir, "out/siso"),
		hashFS: hashFS,
		graph: fakeGraph{
			inputDeps: map[string][]string{
				"build/mac_files/xcode_binaries/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX.sdk:headers": {
					"build/mac_files/xcode_binaries/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX.sdk/somefile",
				},
				"third_party/llvm-build/Release+Asserts:headers": {
					"third_party/llvm-build/Release+Asserts/bin/clang",
					"third_party/llvm-build/Release+Asserts/bin/clang++",
				},
				"buildtools/third_party/libc++/trunk/include:headers": {
					"buildtools/third_party/libc++/trunk/include/stdout.h",
				},
			},
		},
	}

	cmd := &execute.Cmd{
		Args: []string{
			"../../third_party/llvm-build/Release+Asserts/bin/clang",
			"-I.",
			"-Igen",
			"-I../..",
			"-isysroot",
			"../../build/mac_files/xcode_binaries/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX13.3.sdk",
			"-isystem",
			"../../buildtools/third_party/libc++/trunk/include",
			"-c",
			"../../base/base.cc",
			"-o",
			"obj/base/base.o",
		},
		Dir: "out/siso",
	}

	gcc := depsGCC{
		treeInput: func(ctx context.Context, dir string) (merkletree.TreeEntry, error) {
			inputDeps := b.graph.InputDeps(ctx)
			dir, _, err := b.resolveSymlinkForInputDeps(ctx, dir, ":headers", inputDeps)
			if err != nil {
				t.Logf("tree of %s: not found: %v", dir, err)
				return merkletree.TreeEntry{}, err
			}
			return merkletree.TreeEntry{Name: dir}, nil
		},
	}

	got, err := gcc.fixCmdInputs(ctx, b, cmd)
	if err != nil {
		t.Errorf("gcc.fixCmdInputs(ctx, b, cmd)=%q, %v; want nil err", got, err)
	}
	want := []string{
		".",
		"build/mac_files/xcode_binaries/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX13.3.sdk",
		"buildtools/third_party/libc++/trunk/include",
		"out/siso",
		"out/siso/gen",
		"third_party/llvm-build/Release+Asserts",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("gcc.fixCmdInputs(ctx, b, cmd): diff -want +got:\n%s", diff)
	}

	wantTrees := []merkletree.TreeEntry{
		{Name: "third_party/llvm-build/Release+Asserts"},
		{Name: "build/mac_files/xcode_binaries/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX.sdk"},
		{Name: "buildtools/third_party/libc++/trunk/include"},
	}
	if diff := cmp.Diff(wantTrees, cmd.TreeInputs); diff != "" {
		t.Errorf("cmd.TreeInputs; diff -want +got:\n%s", diff)
	}
}

func TestDepsGCCFixCmdInputs_chromeos(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("depsGCC is not used on windows")
		return
	}
	ctx := t.Context()
	dir := t.TempDir()
	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}

	setupDir := func(dirname string) {
		t.Helper()
		fullpath := filepath.Join(dir, dirname)
		err := os.MkdirAll(fullpath, 0755)
		if err != nil {
			t.Fatal(err)
		}
	}

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
	setupSymlink := func(fname, target string) {
		t.Helper()
		fullpath := filepath.Join(dir, fname)
		err := os.MkdirAll(filepath.Dir(fullpath), 0755)
		if err != nil {
			t.Fatal(err)
		}
		err = os.Symlink(target, fullpath)
		if err != nil {
			t.Fatal(err)
		}
	}

	setupDir("out_amd64-generic/Release")
	setupDir("out_amd64-generic/Release/gen")
	setupDir("out_amd64-generic/Release/obj/base")
	setupFile("base/base.cc")
	setupSymlink("build/cros_cache/chrome-sdk/symlinks/amd64-generic+15633.0.0+target_toolchain", "../tarballs/target_toolchain/chromiumos-sdk-2023-09-x86_64-cros-linux-gnu-2023.09.28.020006.tar.xz")
	setupFile("build/cros_cache/chrome-sdk/tarballs/target_toolchain/chromiumos-sdk-2023-09-x86_64-cros-linux-gnu-2023.09.28.020006.tar.xz/bin/x86_64-cros-linux-gnu-clang")
	setupSymlink("build/cros_cache/chrome-sdk/symlinks/amd64-generic+15633.0.0+sysroot_chromeos-base_chromeos-chrome.tar.xz", "../tarballs/sysroot_chromeos-base_chromeos-chrome.tar.xz/chromiumos-image-archive-amd64-generic-public-R119-15633.0.0-sysroot_chromeos-base_chromeos-chrome.tar.xz")
	setupFile("build/cros_cache/chrome-sdk/tarballs/sysroot_chromeos-base_chromeos-chrome.tar.xz/chromiumos-image-archive-amd64-generic-public-R119-15633.0.0-sysroot_chromeos-base_chromeos-chrome.tar.xz/usr/include/stdio.h")
	setupFile("third_party/libc++/src/include/stdout.h")

	b := &Builder{
		path:   NewPath(dir, "out_amd64-generic/Release"),
		hashFS: hashFS,
		graph: fakeGraph{
			inputDeps: map[string][]string{
				"build/cros_cache/chrome-sdk/symlinks/amd64-generic+15633.0.0+target_toolchain:headers": {
					"build/cros_cache/chrome-sdk/symlinks/amd64-generic+15633.0.0+target_toolchain/bin/x86_64-cros-linux-gnu-clang",
				},
				"build/cros_cache/chrome-sdk/symlinks/amd64-generic+15633.0.0+sysroot_chromeos-base_chromeos-chrome.tar.xz:headers": {
					"build/cros_cache/chrome-sdk/symlinks/amd64-generic+15633.0.0+sysroot_chromeos-base_chromeos-chrome.tar.xz/usr/include/stdio.h",
				},
				"third_party/libc++/src/include:headers": {
					"third_party/libc++/src/include/stdout.h",
				},
			},
		},
	}

	cmd := &execute.Cmd{
		Args: []string{
			"../../build/cros_cache/chrome-sdk/symlinks/amd64-generic+15633.0.0+target_toolchain/bin/x86_64-cros-linux-gnu-clang",
			"-I.",
			"-Igen",
			"-I../..",
			"-isystem../../third_party/libc++/src/include",
			"--sysroot=../../build/cros_cache/chrome-sdk/symlinks/amd64-generic+15633.0.0+sysroot_chromeos-base_chromeos-chrome.tar.xz",
			"-c",
			"../../base/base.cc",
			"-o",
			"obj/base/base.o",
		},
		Dir: "out_amd64-generic/Release",
	}

	gcc := depsGCC{
		treeInput: func(ctx context.Context, dir string) (merkletree.TreeEntry, error) {
			inputDeps := b.graph.InputDeps(ctx)
			resolveddir, _, err := b.resolveSymlinkForInputDeps(ctx, dir, ":headers", inputDeps)
			if err != nil {
				t.Logf("tree of %s: not found: %v", dir, err)
				return merkletree.TreeEntry{}, err
			}
			return merkletree.TreeEntry{Name: resolveddir}, nil
		},
	}

	got, err := gcc.fixCmdInputs(ctx, b, cmd)
	if err != nil {
		t.Errorf("gcc.fixCmdInputs(ctx, b, cmd)=%q, %v; want nil err", got, err)
	}
	want := []string{
		".",
		"build/cros_cache/chrome-sdk/symlinks/amd64-generic+15633.0.0+sysroot_chromeos-base_chromeos-chrome.tar.xz",
		"build/cros_cache/chrome-sdk/symlinks/amd64-generic+15633.0.0+target_toolchain",
		"out_amd64-generic/Release",
		"out_amd64-generic/Release/gen",
		"third_party/libc++/src/include",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("gcc.fixCmdInputs(ctx, b, cmd): diff -want +got:\n%s", diff)
	}

	wantTrees := []merkletree.TreeEntry{
		{Name: "build/cros_cache/chrome-sdk/symlinks/amd64-generic+15633.0.0+target_toolchain"},
		{Name: "build/cros_cache/chrome-sdk/symlinks/amd64-generic+15633.0.0+sysroot_chromeos-base_chromeos-chrome.tar.xz"},
		{Name: "third_party/libc++/src/include"},
	}
	if diff := cmp.Diff(wantTrees, cmd.TreeInputs); diff != "" {
		t.Errorf("cmd.TreeInputs; diff -want +got:\n%s", diff)
	}
}
