// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package e2etests

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/subcommands"

	"go.chromium.org/build/siso/auth/cred"
	"go.chromium.org/build/siso/build/ninjabuild"
	"go.chromium.org/build/siso/hashfs"
	pb "go.chromium.org/build/siso/hashfs/proto"
	"go.chromium.org/build/siso/subcmd/fscmd"
)

const stateFileName = ".siso_fs_state"

func setupGCTestDir(t *testing.T) (context.Context, string) {
	t.Helper()
	ctx := t.Context()
	dir := tempDir(t)

	if err := os.MkdirAll(filepath.Join(dir, "build/config/siso"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "out/siso"), 0755); err != nil {
		t.Fatal(err)
	}

	setupFiles(t, dir, "TestBuild_FSGC_E2E", nil)
	if err := os.Remove(filepath.Join(dir, "out/siso", stateFileName)); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return ctx, dir
}

func runNinja(ctx context.Context, t *testing.T, dir, label string) {
	t.Helper()
	opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{StateFile: stateFileName, BuildLabel: label})
	defer cleanup()

	_, err := ninjabuild.Run(ctx, graph, opt, nil, ninjabuild.RunNinjaOpts{})
	if err != nil {
		t.Fatalf("ninja err for %s, got=%v; want=<nil>", label, err)
	}
}

func runNinjaWithLabel(ctx context.Context, t *testing.T, dir, label, ninjaFile string) {
	t.Helper()
	original := filepath.Join(dir, "out/siso", ninjaFile)
	dest := filepath.Join(dir, "out/siso/build.ninja")
	originalB, err := os.ReadFile(original)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, originalB, 0644); err != nil {
		t.Fatal(err)
	}

	runNinja(ctx, t, dir, label)
}

func getBuildLabelMask(state *pb.State, fname string) uint64 {
	target := filepath.ToSlash(fname)
	for _, lbl := range state.FileBuildLabels {
		if lbl.Path == target || strings.HasSuffix(lbl.Path, "/"+strings.TrimPrefix(target, "/")) {
			return lbl.Mask
		}
	}
	return 0
}

func runGC(ctx context.Context, t *testing.T, dir string, args ...string) error {
	t.Helper()
	cmd := fscmd.Cmd(func() cred.Options { return cred.Options{} })
	fs := flag.NewFlagSet("fs", flag.ContinueOnError)
	cmd.SetFlags(fs)
	fullArgs := append([]string{"gc", "-C", filepath.Join(dir, "out/siso"), "--fs_state", stateFileName}, args...)
	if err := fs.Parse(fullArgs); err != nil {
		return err
	}
	status := cmd.Execute(ctx, fs)
	if status != subcommands.ExitSuccess {
		return fmt.Errorf("fscmd gc exited with status %v", status)
	}
	return nil
}

func TestBuild_FSGC_EvictTarget(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	ctx, dir := setupGCTestDir(t)

	runNinjaWithLabel(ctx, t, dir, "target-1", "build.ninja.0")
	for _, fname := range []string{"out/siso/obj/bar.o", "out/siso/obj/foo.o"} {
		if _, err := os.Stat(filepath.Join(dir, fname)); err != nil {
			t.Fatalf("stat(%s) err, got=%v; want=<nil>", fname, err)
		}
	}

	time.Sleep(50 * time.Millisecond)

	runNinjaWithLabel(ctx, t, dir, "target-2", "build.ninja.1")
	for _, fname := range []string{"out/siso/obj/bar.o", "out/siso/obj/foo.o", "out/siso/obj/baz.o"} {
		if _, err := os.Stat(filepath.Join(dir, fname)); err != nil {
			t.Fatalf("stat(%s) err, got=%v; want=<nil>", fname, err)
		}
	}

	err := runGC(ctx, t, dir, "target-1")
	if err != nil {
		t.Fatalf("runGC err, got=%v; want=<nil>", err)
	}

	// Validate obj/bar.o is physically deleted (exclusive to target-1)
	if _, err := os.Stat(filepath.Join(dir, "out/siso/obj/bar.o")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat(obj/bar.o) err, got=%v; want=os.ErrNotExist", err)
	}
	// Validate obj/foo.o remains intact (preventing accidental shared cache purges - used by target-2)
	if _, err := os.Stat(filepath.Join(dir, "out/siso/obj/foo.o")); err != nil {
		t.Errorf("stat(obj/foo.o) err, got=%v; want=<nil>", err)
	}
}

func TestBuild_FSGC_RetainLastX(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	ctx, dir := setupGCTestDir(t)

	runNinjaWithLabel(ctx, t, dir, "target-1", "build.ninja.0")
	time.Sleep(50 * time.Millisecond)
	runNinjaWithLabel(ctx, t, dir, "target-2", "build.ninja.1")

	err := runGC(ctx, t, dir, "--retain_last_x=1")
	if err != nil {
		t.Fatalf("runGC err, got=%v; want=<nil>", err)
	}

	// Older targets (target-1) fall out, exclusively owned artifacts are evicted.
	if _, err := os.Stat(filepath.Join(dir, "out/siso/obj/bar.o")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat(obj/bar.o) err, got=%v; want=os.ErrNotExist", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "out/siso/obj/foo.o")); err != nil {
		t.Errorf("stat(obj/foo.o) err, got=%v; want=<nil>", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "out/siso/obj/baz.o")); err != nil {
		t.Errorf("stat(obj/baz.o) err, got=%v; want=<nil>", err)
	}
}

func TestBuild_FSGC_DryRun(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	ctx, dir := setupGCTestDir(t)

	runNinjaWithLabel(ctx, t, dir, "target-1", "build.ninja.0")

	outputFile := filepath.Join(dir, "evicted.txt")
	err := runGC(ctx, t, dir, "--dry_run", "--output_file", outputFile, "target-1")
	if err != nil {
		t.Fatalf("runGC err, got=%v; want=<nil>", err)
	}

	evictedFilesContent, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("os.ReadFile(%s) err, got=%v; want=<nil>", outputFile, err)
	}
	evictedFiles := strings.Split(strings.TrimSpace(string(evictedFilesContent)), "\n")
	if len(evictedFiles) != 7 {
		t.Errorf("len(evictedFiles), got=%d; want=7", len(evictedFiles))
	}

	// Confirm zero files were dropped, obj/foo.o and obj/bar.o still exist
	if _, err := os.Stat(filepath.Join(dir, "out/siso/obj/foo.o")); err != nil {
		t.Errorf("stat(obj/foo.o) dry run err, got=%v; want=<nil>", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "out/siso/obj/bar.o")); err != nil {
		t.Errorf("stat(obj/bar.o) dry run err, got=%v; want=<nil>", err)
	}
}

func TestBuild_FSGC_ValidationErrors(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	ctx, dir := setupGCTestDir(t)

	t.Run("NoStrategy", func(t *testing.T) {
		err := runGC(ctx, t, dir)
		if err == nil {
			t.Errorf("runGC empty strategy err, got=<nil>; want=error")
		}
	})

	t.Run("ConflictingStrategies", func(t *testing.T) {
		err := runGC(ctx, t, dir, "--retain_last_x=1", "target-1")
		if err == nil {
			t.Errorf("runGC conflicting strategy err, got=<nil>; want=error")
		}
	})
}

func TestBuild_FSGC_ListActiveLabels(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	ctx, dir := setupGCTestDir(t)

	// Seed the HashFS state ledger with actual builds
	runNinjaWithLabel(ctx, t, dir, "target-1", "build.ninja.0")
	time.Sleep(50 * time.Millisecond)
	runNinjaWithLabel(ctx, t, dir, "target-2", "build.ninja.1")

	// Capture stdout asynchronously
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	defer func() {
		os.Stdout = oldStdout
		w.Close() // Ensure restoration and unblocking if runGC panics
	}()

	errCh := make(chan error, 1)
	var outBytes []byte
	go func() {
		var err error
		outBytes, err = io.ReadAll(r)
		errCh <- err
	}()

	err := runGC(ctx, t, dir, "--list_active_labels")

	w.Close()

	if readErr := <-errCh; readErr != nil {
		t.Fatalf("Failed to read stdout pipe: %v", readErr)
	}

	if err != nil {
		t.Fatalf("runGC err, got=%v; want=<nil>", err)
	}

	output := string(outBytes)

	if !strings.Contains(output, "Active Build Labels (2):") {
		t.Errorf("output missing summary header, got:\n%s", output)
	}
	if !strings.Contains(output, " - target-1") {
		t.Errorf("output missing target-1, got:\n%s", output)
	}
	if !strings.Contains(output, " - target-2") {
		t.Errorf("output missing target-2, got:\n%s", output)
	}

	// Confirm zero files were dropped, obj/foo.o and obj/bar.o still exist
	if _, err := os.Stat(filepath.Join(dir, "out/siso/obj/foo.o")); err != nil {
		t.Errorf("stat(obj/foo.o) err, got=%v; want=<nil>", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "out/siso/obj/bar.o")); err != nil {
		t.Errorf("stat(obj/bar.o) err, got=%v; want=<nil>", err)
	}
}

func TestBuild_FSGC_ManifestFiles(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	ctx, dir := setupGCTestDir(t)

	if err := os.MkdirAll(filepath.Join(dir, "out/siso/obj"), 0755); err != nil {
		t.Fatal(err)
	}

	// Create root build.ninja and subninja sub.ninja in out/siso.
	rootNinja := filepath.Join(dir, "out/siso/build.ninja")
	subNinja := filepath.Join(dir, "out/siso/sub.ninja")
	outObj := filepath.Join(dir, "out/siso/obj/out.txt")

	rootContent := []byte(`subninja sub.ninja

rule gen
  command = python3 ../../tools/gen.py ${in} ${out}

build obj/out.txt: gen ../../base/foo.h.in
build all: phony obj/out.txt
default all
build build.ninja: phony
`)
	subContent := []byte(`# Subninja manifest
`)

	if err := os.WriteFile(rootNinja, rootContent, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(subNinja, subContent, 0644); err != nil {
		t.Fatal(err)
	}

	runNinja(ctx, t, dir, "label-1")

	// Verify ledger state contains both manifest files and compiled output tagged with label-1's bitmask.
	state, err := hashfs.Load(ctx, hashfs.Option{
		StateFile: filepath.Join(dir, "out/siso", stateFileName),
	})
	if err != nil {
		t.Fatalf("hashfs.Load err, got=%v; want=<nil>", err)
	}

	var bitID *uint32
	for _, entry := range state.BuildLabelDictionary {
		if entry != nil && entry.Metadata != nil && entry.Metadata.BuildLabel == "label-1" {
			bitID = &entry.Id
			break
		}
	}
	if bitID == nil {
		t.Fatalf("label-1 missing from label dictionary")
	}
	expectedMask := uint64(1) << *bitID

	if got := getBuildLabelMask(state, "build.ninja"); got != expectedMask {
		t.Errorf("getBuildLabelMask(build.ninja) = %d, want %d", got, expectedMask)
	}
	if got := getBuildLabelMask(state, "sub.ninja"); got != expectedMask {
		t.Errorf("getBuildLabelMask(sub.ninja) = %d, want %d", got, expectedMask)
	}
	if got := getBuildLabelMask(state, "obj/out.txt"); got != expectedMask {
		t.Errorf("getBuildLabelMask(obj/out.txt) = %d, want %d", got, expectedMask)
	}

	// Verify both manifest files and generated output exist on disk before eviction.
	if _, err := os.Stat(rootNinja); err != nil {
		t.Fatalf("os.Stat(build.ninja) err, got=%v; want=<nil>", err)
	}
	if _, err := os.Stat(subNinja); err != nil {
		t.Fatalf("os.Stat(sub.ninja) err, got=%v; want=<nil>", err)
	}
	if _, err := os.Stat(outObj); err != nil {
		t.Fatalf("os.Stat(obj/out.txt) err, got=%v; want=<nil>", err)
	}

	// Run GC evicting label-1.
	err = runGC(ctx, t, dir, "label-1")
	if err != nil {
		t.Fatalf("runGC err, got=%v; want=<nil>", err)
	}

	// Assert that build.ninja, sub.ninja, and obj/out.txt are deleted from disk!
	if _, err := os.Stat(rootNinja); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("os.Stat(build.ninja) err, got=%v; want=os.ErrNotExist", err)
	}
	if _, err := os.Stat(subNinja); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("os.Stat(sub.ninja) err, got=%v; want=os.ErrNotExist", err)
	}
	if _, err := os.Stat(outObj); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("os.Stat(obj/out.txt) err, got=%v; want=os.ErrNotExist", err)
	}
}

func TestBuild_FSGC_ManifestFiles_Shared(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	ctx, dir := setupGCTestDir(t)

	if err := os.MkdirAll(filepath.Join(dir, "out/siso/obj"), 0755); err != nil {
		t.Fatal(err)
	}

	sharedNinja := filepath.Join(dir, "out/siso/shared.ninja")
	root1Ninja := filepath.Join(dir, "out/siso/build.ninja.1")
	root2Ninja := filepath.Join(dir, "out/siso/build.ninja.2")
	activeNinja := filepath.Join(dir, "out/siso/build.ninja")
	outObj1 := filepath.Join(dir, "out/siso/obj/out1.txt")
	outObj2 := filepath.Join(dir, "out/siso/obj/out2.txt")

	sharedContent := []byte(`build obj/out1.txt: gen ../../base/foo.h.in
build obj/out2.txt: gen ../../base/foo.h.in
`)
	root1Content := []byte(`rule gen
  command = python3 ../../tools/gen.py ${in} ${out}

subninja shared.ninja

build all: phony obj/out1.txt
default all
build build.ninja: phony
`)
	root2Content := []byte(`rule gen
  command = python3 ../../tools/gen.py ${in} ${out}

subninja shared.ninja

build all: phony obj/out2.txt
default all
build build.ninja: phony
`)

	if err := os.WriteFile(sharedNinja, sharedContent, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root1Ninja, root1Content, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root2Ninja, root2Content, 0644); err != nil {
		t.Fatal(err)
	}

	runNinjaWithLabel(ctx, t, dir, "target-1", "build.ninja.1")
	time.Sleep(50 * time.Millisecond)
	runNinjaWithLabel(ctx, t, dir, "target-2", "build.ninja.2")

	// Verify all files exist before eviction.
	if _, err := os.Stat(outObj1); err != nil {
		t.Fatalf("os.Stat(obj/out1.txt) err, got=%v; want=<nil>", err)
	}
	if _, err := os.Stat(outObj2); err != nil {
		t.Fatalf("os.Stat(obj/out2.txt) err, got=%v; want=<nil>", err)
	}
	if _, err := os.Stat(sharedNinja); err != nil {
		t.Fatalf("os.Stat(shared.ninja) err, got=%v; want=<nil>", err)
	}
	if _, err := os.Stat(activeNinja); err != nil {
		t.Fatalf("os.Stat(build.ninja) err, got=%v; want=<nil>", err)
	}

	// Evict target-1:
	// - out1.txt was exclusive to target-1, so it must be deleted.
	// - out2.txt belongs to target-2, so it must be preserved.
	// - shared.ninja was used by both target-1 and target-2, so it must NOT be deleted yet!
	// - activeNinja (build.ninja) was used by both target-1 and target-2, so it must NOT be deleted yet!
	err := runGC(ctx, t, dir, "target-1")
	if err != nil {
		t.Fatalf("runGC(target-1) err, got=%v; want=<nil>", err)
	}
	if _, err := os.Stat(outObj1); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat(obj/out1.txt) after target-1 eviction err, got=%v; want=os.ErrNotExist", err)
	}
	if _, err := os.Stat(outObj2); err != nil {
		t.Errorf("stat(obj/out2.txt) after target-1 eviction err, got=%v; want=<nil>", err)
	}
	if _, err := os.Stat(sharedNinja); err != nil {
		t.Errorf("stat(shared.ninja) after target-1 eviction err, got=%v; want=<nil>", err)
	}
	if _, err := os.Stat(activeNinja); err != nil {
		t.Errorf("stat(build.ninja) after target-1 eviction err, got=%v; want=<nil>", err)
	}

	// Now evict target-2:
	// All references to target-2 are gone, so out2.txt, shared.ninja, and build.ninja must be deleted!
	err = runGC(ctx, t, dir, "target-2")
	if err != nil {
		t.Fatalf("runGC(target-2) err, got=%v; want=<nil>", err)
	}
	if _, err := os.Stat(outObj2); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat(obj/out2.txt) after target-2 eviction err, got=%v; want=os.ErrNotExist", err)
	}
	if _, err := os.Stat(sharedNinja); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat(shared.ninja) after target-2 eviction err, got=%v; want=os.ErrNotExist", err)
	}
	if _, err := os.Stat(activeNinja); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat(build.ninja) after target-2 eviction err, got=%v; want=os.ErrNotExist", err)
	}
}

func TestBuild_FSGC_ManifestFiles_Reload(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	ctx, dir := setupGCTestDir(t)

	if err := os.MkdirAll(filepath.Join(dir, "out/siso/obj"), 0755); err != nil {
		t.Fatal(err)
	}

	rootNinja := filepath.Join(dir, "out/siso/build.ninja")
	subNinja := filepath.Join(dir, "out/siso/sub.ninja")
	triggerFile := filepath.Join(dir, "out/siso/trigger.txt")
	regenScript := filepath.Join(dir, "tools/regen.py")
	outObj := filepath.Join(dir, "out/siso/obj/out.txt")

	// Create regen.py which rewrites build.ninja to include sub.ninja, and creates sub.ninja.
	regenContent := []byte(`with open("build.ninja", "w") as f:
  f.write("""subninja sub.ninja

rule gen
  command = python3 ../../tools/gen.py ${in} ${out}

build obj/out.txt: gen ../../base/foo.h.in
build all: phony obj/out.txt
default all
""")

with open("sub.ninja", "w") as f:
  f.write("# subninja dynamically created during manifest reload\n")
`)
	if err := os.WriteFile(regenScript, regenContent, 0755); err != nil {
		t.Fatal(err)
	}
	// Initial build.ninja has a rebuild manifest rule depending on trigger.txt.
	rootContent := []byte(`rule regen
  command = python3 ../../tools/regen.py
  generator = 1

rule gen
  command = python3 ../../tools/gen.py ${in} ${out}

build build.ninja: regen trigger.txt

build obj/out.txt: gen ../../base/foo.h.in
build all: phony obj/out.txt
default all
`)
	if err := os.WriteFile(rootNinja, rootContent, 0644); err != nil {
		t.Fatal(err)
	}

	time.Sleep(20 * time.Millisecond)

	if err := os.WriteFile(triggerFile, []byte("trigger"), 0644); err != nil {
		t.Fatal(err)
	}

	runNinja(ctx, t, dir, "label-reload")

	// Verify state ledger contains sub.ninja tagged with label-reload mask.
	state, err := hashfs.Load(ctx, hashfs.Option{
		StateFile: filepath.Join(dir, "out/siso", stateFileName),
	})
	if err != nil {
		t.Fatalf("hashfs.Load err, got=%v; want=<nil>", err)
	}

	var bitID *uint32
	for _, entry := range state.BuildLabelDictionary {
		if entry != nil && entry.Metadata != nil && entry.Metadata.BuildLabel == "label-reload" {
			bitID = &entry.Id
			break
		}
	}
	if bitID == nil {
		t.Fatalf("label-reload missing from label dictionary")
	}
	expectedMask := uint64(1) << *bitID

	if got := getBuildLabelMask(state, "build.ninja"); got != expectedMask {
		t.Errorf("getBuildLabelMask(build.ninja) = %d, want %d", got, expectedMask)
	}
	if got := getBuildLabelMask(state, "sub.ninja"); got != expectedMask {
		t.Errorf("getBuildLabelMask(sub.ninja) = %d, want %d", got, expectedMask)
	}
	if got := getBuildLabelMask(state, "obj/out.txt"); got != expectedMask {
		t.Errorf("getBuildLabelMask(obj/out.txt) = %d, want %d", got, expectedMask)
	}

	// Verify all files exist before eviction.
	if _, err := os.Stat(rootNinja); err != nil {
		t.Fatalf("os.Stat(build.ninja) err, got=%v; want=<nil>", err)
	}
	if _, err := os.Stat(subNinja); err != nil {
		t.Fatalf("os.Stat(sub.ninja) err, got=%v; want=<nil>", err)
	}
	if _, err := os.Stat(outObj); err != nil {
		t.Fatalf("os.Stat(obj/out.txt) err, got=%v; want=<nil>", err)
	}

	// Run GC evicting label-reload.
	err = runGC(ctx, t, dir, "label-reload")
	if err != nil {
		t.Fatalf("runGC err, got=%v; want=<nil>", err)
	}

	// Assert that all files, including dynamically introduced sub.ninja, are deleted from disk!
	if _, err := os.Stat(rootNinja); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("os.Stat(build.ninja) err, got=%v; want=os.ErrNotExist", err)
	}
	if _, err := os.Stat(subNinja); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("os.Stat(sub.ninja) err, got=%v; want=os.ErrNotExist", err)
	}
	if _, err := os.Stat(outObj); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("os.Stat(obj/out.txt) err, got=%v; want=os.ErrNotExist", err)
	}
}
