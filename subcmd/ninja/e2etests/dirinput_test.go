// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package e2etests

import (
	"archive/zip"
	"bytes"
	"context"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/prototext"

	"go.chromium.org/build/hashigo/digest"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/build/ninjabuild"
	"go.chromium.org/build/siso/hashfs"
	nsjailpb "go.chromium.org/build/siso/toolsupport/nsjailutil/proto"
)

// requirePython3 skips the test if python3 is not on $PATH (the dir-input tests drive the build with python3-based unzip/mkzip scripts).
func requirePython3(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skipf("python3 not found on PATH: %v", err)
	}
}

func TestBuild_DirInput(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	requirePython3(t)
	ctx := t.Context()
	dir := tempDir(t)

	runNinjaTest := func(t *testing.T) (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:   ".siso_fs_state",
			OutputLocal: func(context.Context, string) bool { return true },
		})
		defer cleanup()
		return ninjabuild.Run(ctx, graph, opt, nil, ninjabuild.RunNinjaOpts{})
	}

	t.Logf("-- setup workspace")
	setupFiles(t, dir, t.Name(), nil)
	writeInputZip(t, dir)

	t.Logf("-- first build")
	stats, err := runNinjaTest(t)
	if err != nil {
		t.Fatalf("ninja err: %v", err)
	}
	t.Logf("first build stats: done=%d skipped=%d local=%d", stats.Done, stats.Skipped, stats.Local)

	for _, fname := range []string{
		"out/siso/obj/extracted/hello.txt",
		"out/siso/obj/extracted/subdir/nested.txt",
	} {
		_, err := os.Stat(filepath.Join(dir, fname))
		if err != nil {
			t.Errorf("stat(%q)=%v; want nil error", fname, err)
		}
	}
	// The repackaged zip must round-trip the extracted content (mkzip could
	// otherwise produce an empty zip and still "exist").
	wantZip := map[string]string{
		"hello.txt":         "Hello, world!\n",
		"subdir/nested.txt": "Nested file\n",
	}
	if got := readZipEntries(t, filepath.Join(dir, "out/siso/obj/repackaged.zip")); !maps.Equal(got, wantZip) {
		t.Errorf("repackaged.zip entries = %v; want %v", got, wantZip)
	}

	t.Logf("-- confirm no-op")
	stats, err = runNinjaTest(t)
	if err != nil {
		t.Fatalf("ninja err: %v", err)
	}
	if stats.Done != stats.Total || stats.Local != 0 || stats.Skipped != stats.Total {
		t.Errorf("ninja confirm no-op error: done=%d local=%d skipped=%d total=%d; want all skipped", stats.Done, stats.Local, stats.Skipped, stats.Total)
	}

	t.Logf("-- modify file inside extracted directory")
	// mkzip should re-run because its directory input changed.
	modifyFile(t, dir, "out/siso/obj/extracted/hello.txt", func(old []byte) []byte {
		return []byte("Modified content\n")
	})

	t.Logf("-- rebuild after dir modification")
	stats, err = runNinjaTest(t)
	if err != nil {
		t.Fatalf("ninja err: %v", err)
	}
	// unzip skipped (input.zip unchanged), mkzip runs (dir contents changed).
	// Pin exact counts to catch an unnecessary unzip re-run or a missed mkzip.
	if stats.Local != 1 || stats.Skipped != stats.Total-1 {
		t.Errorf("after dir mod: local=%d skipped=%d total=%d; want local=1 skipped=%d (only mkzip should run)", stats.Local, stats.Skipped, stats.Total, stats.Total-1)
	}
	t.Logf("rebuild stats: done=%d skipped=%d local=%d", stats.Done, stats.Skipped, stats.Local)

	t.Logf("-- confirm no-op again")
	stats, err = runNinjaTest(t)
	if err != nil {
		t.Fatalf("ninja err: %v", err)
	}
	if stats.Done != stats.Total || stats.Local != 0 || stats.Skipped != stats.Total {
		t.Errorf("ninja confirm no-op error: done=%d local=%d skipped=%d total=%d; want all skipped", stats.Done, stats.Local, stats.Skipped, stats.Total)
	}

	t.Logf("-- add file inside extracted directory")
	newFile := filepath.Join(dir, "out/siso/obj/extracted/newfile.txt")
	err = os.WriteFile(newFile, []byte("new file\n"), 0644)
	if err != nil {
		t.Fatalf("write newfile.txt: %v", err)
	}
	// Ensure mtime is distinct.
	err = os.Chtimes(newFile, time.Time{}, time.Now().Add(time.Second))
	if err != nil {
		t.Fatalf("chtimes newfile.txt: %v", err)
	}

	t.Logf("-- rebuild after file added to dir")
	stats, err = runNinjaTest(t)
	if err != nil {
		t.Fatalf("ninja err: %v", err)
	}
	// Adding a file bumps the parent dir mtime (Linux), so both unzip (its
	// output dir changed externally) and mkzip (its dir input changed) re-run.
	if stats.Local != 2 || stats.Skipped != stats.Total-2 {
		t.Errorf("after file add: local=%d skipped=%d total=%d; want local=2 skipped=%d", stats.Local, stats.Skipped, stats.Total, stats.Total-2)
	}
	t.Logf("rebuild stats: done=%d skipped=%d local=%d", stats.Done, stats.Skipped, stats.Local)

	t.Logf("-- confirm no-op after add-file rebuild")
	stats, err = runNinjaTest(t)
	if err != nil {
		t.Fatalf("ninja err: %v", err)
	}
	if stats.Done != stats.Total || stats.Local != 0 || stats.Skipped != stats.Total {
		t.Errorf("ninja confirm no-op after add-file: done=%d local=%d skipped=%d total=%d; want all skipped", stats.Done, stats.Local, stats.Skipped, stats.Total)
	}

	t.Logf("-- delete file inside extracted directory")
	err = os.Remove(filepath.Join(dir, "out/siso/obj/extracted/hello.txt"))
	if err != nil {
		t.Fatalf("remove hello.txt: %v", err)
	}

	t.Logf("-- rebuild after file deleted from dir")
	stats, err = runNinjaTest(t)
	if err != nil {
		t.Fatalf("ninja err: %v", err)
	}
	// Deletion bumps parent dir mtime (Linux); both steps re-run as in add-file.
	if stats.Local != 2 || stats.Skipped != stats.Total-2 {
		t.Errorf("after file delete: local=%d skipped=%d total=%d; want local=2 skipped=%d", stats.Local, stats.Skipped, stats.Total, stats.Total-2)
	}
	t.Logf("rebuild stats: done=%d skipped=%d local=%d", stats.Done, stats.Skipped, stats.Local)

	t.Logf("-- confirm no-op after deletion rebuild")
	stats, err = runNinjaTest(t)
	if err != nil {
		t.Fatalf("ninja err: %v", err)
	}
	if stats.Done != stats.Total || stats.Local != 0 || stats.Skipped != stats.Total {
		t.Errorf("ninja confirm no-op error: done=%d local=%d skipped=%d total=%d; want all skipped", stats.Done, stats.Local, stats.Skipped, stats.Total)
	}

	t.Logf("-- modify input.zip")
	modifyFile(t, dir, "input.zip", func(old []byte) []byte {
		return createTestZip(t, map[string]string{
			"hello.txt":         "Changed hello\n",
			"subdir/nested.txt": "Changed nested\n",
			"extra.txt":         "Extra file\n",
		})
	})

	t.Logf("-- rebuild after input change")
	stats, err = runNinjaTest(t)
	if err != nil {
		t.Fatalf("ninja err: %v", err)
	}
	// input.zip changed: unzip re-extracts and mkzip re-packages.
	if stats.Local != 2 || stats.Skipped != stats.Total-2 {
		t.Errorf("after input change: local=%d skipped=%d total=%d; want local=2 (unzip + mkzip)", stats.Local, stats.Skipped, stats.Total)
	}

	if got := readFile(t, filepath.Join(dir, "out/siso/obj/extracted/hello.txt")); got != "Changed hello\n" {
		t.Errorf("%s = %q, want %q", filepath.Join(dir, "out/siso/obj/extracted/hello.txt"), got, "Changed hello\n")
	}
	if got := readFile(t, filepath.Join(dir, "out/siso/obj/extracted/extra.txt")); got != "Extra file\n" {
		t.Errorf("%s = %q, want %q", filepath.Join(dir, "out/siso/obj/extracted/extra.txt"), got, "Extra file\n")
	}
	wantZip = map[string]string{
		"hello.txt":         "Changed hello\n",
		"subdir/nested.txt": "Changed nested\n",
		"extra.txt":         "Extra file\n",
	}
	if got := readZipEntries(t, filepath.Join(dir, "out/siso/obj/repackaged.zip")); !maps.Equal(got, wantZip) {
		t.Errorf("repackaged.zip entries = %v; want %v", got, wantZip)
	}
}

func createTestZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	// Sort keys for deterministic output.
	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		f, err := w.Create(k)
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.Write([]byte(files[k]))
		if err != nil {
			t.Fatal(err)
		}
	}
	err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// writeInputZip writes the standard input.zip fixture (hello.txt and subdir/nested.txt) into the work dir, generated rather than committed so the testdata stays text-only.
func writeInputZip(t *testing.T, dir string) {
	t.Helper()
	data := createTestZip(t, map[string]string{
		"hello.txt":         "Hello, world!\n",
		"subdir/nested.txt": "Nested file\n",
	})
	if err := os.WriteFile(filepath.Join(dir, "input.zip"), data, 0644); err != nil {
		t.Fatal(err)
	}
}

// TestBuild_EmptyDirOutput builds a step whose directory output is empty: the build succeeds, the directory exists on disk, and a rebuild is a no-op.
func TestBuild_EmptyDirOutput(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	requirePython3(t)
	ctx := t.Context()
	dir := tempDir(t)

	runNinjaTest := func(t *testing.T) (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:   ".siso_fs_state",
			OutputLocal: func(context.Context, string) bool { return true },
		})
		defer cleanup()
		return ninjabuild.Run(ctx, graph, opt, []string{"all"}, ninjabuild.RunNinjaOpts{})
	}

	setupFiles(t, dir, t.Name(), nil)

	if _, err := runNinjaTest(t); err != nil {
		t.Fatalf("ninja err: %v", err)
	}
	empty := filepath.Join(dir, "out/siso/empty")
	fi, err := os.Stat(empty)
	if err != nil {
		t.Fatalf("empty dir output not on disk: %v", err)
	}
	if !fi.IsDir() {
		t.Errorf("empty output is not a directory: mode=%v", fi.Mode())
	}
	ents, err := os.ReadDir(empty)
	if err != nil {
		t.Fatalf("ReadDir(empty): %v", err)
	}
	if len(ents) != 0 {
		t.Errorf("empty dir output is not empty: %v", ents)
	}

	stats, err := runNinjaTest(t)
	if err != nil {
		t.Fatalf("ninja (rebuild) err: %v", err)
	}
	if stats.Skipped != stats.Total {
		t.Errorf("rebuild not a no-op: done=%d skipped=%d total=%d", stats.Done, stats.Skipped, stats.Total)
	}
}

// TestBuild_DirOnlyOutputWithDepfileFails verifies that a directory-only edge
// (trailing-slash output, no file output) that also declares a depfile fails
// loudly instead of silently dropping the depfile's discovered deps. The deps
// log is keyed on a file output (DepsLogKey.Target), so a directory-only edge
// has nowhere to record deps; updateDeps must surface that as an error.
func TestBuild_DirOnlyOutputWithDepfileFails(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	requirePython3(t)
	ctx := t.Context()
	dir := tempDir(t)

	setupFiles(t, dir, t.Name(), nil)

	opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
		StateFile:   ".siso_fs_state",
		OutputLocal: func(context.Context, string) bool { return true },
	})
	defer cleanup()

	_, err := ninjabuild.Run(ctx, graph, opt, []string{"all"}, ninjabuild.RunNinjaOpts{})
	if err == nil {
		t.Fatal("ninja succeeded; want failure for a directory-only output that declares a depfile")
	}
	if !strings.Contains(err.Error(), "deps require a file output") {
		t.Errorf("ninja err = %v; want error mentioning that deps require a file output", err)
	}
}

// TestBuild_DirOutputLocalCleandead verifies cleandead handles a LOCALLY produced directory output: preserve its contents while the target is live, remove the whole directory once the target leaves the graph.
// Local dir outputs record inner files without a cmdhash (only the directory node carries one), so cleandead must treat the directory as a unit keyed on that node; the remote coverage exercises cmdhash'd inner files instead.
func TestBuild_DirOutputLocalCleandead(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	requirePython3(t)
	ctx := t.Context()
	dir := tempDir(t)

	run := func(t *testing.T, cleandead bool, subtool string) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:   ".siso_fs_state",
			OutputLocal: func(context.Context, string) bool { return true },
		})
		defer cleanup()
		_, err := ninjabuild.Run(ctx, graph, opt, []string{"all"}, ninjabuild.RunNinjaOpts{
			Cleandead: cleandead,
			Subtool:   subtool,
		})
		if err != nil {
			t.Fatalf("ninja (cleandead=%v): %v", cleandead, err)
		}
	}

	setupFiles(t, dir, t.Name(), nil)

	// Build: gen/ is produced locally; its inner files get no cmdhash.
	run(t, false, "")
	inner := []string{
		"out/siso/gen/data",
		"out/siso/gen/sub/nested",
	}
	for _, f := range inner {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("after build, %q missing: %v", f, err)
		}
	}

	// SAME graph: gen/ is live, so its contents survive even though the inner
	// files are absent from previouslyGeneratedFiles.
	run(t, true, "cleandead")
	for _, f := range inner {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("cleandead removed live local dir-output content %q: %v", f, err)
		}
	}

	// Drop the dir-output target from the graph; cleandead must now remove
	// the whole directory (contents included) via the directory node.
	newNinja := "build all: phony\nbuild build.ninja: phony\n"
	if err := os.WriteFile(filepath.Join(dir, "out/siso/build.ninja"), []byte(newNinja), 0644); err != nil {
		t.Fatal(err)
	}
	run(t, true, "cleandead")
	gen := filepath.Join(dir, "out/siso/gen")
	if _, err := os.Stat(gen); !os.IsNotExist(err) {
		t.Errorf("cleandead did not remove dead local dir output %q: stat err=%v", gen, err)
	}
}

// TestBuild_DirOutputLocalReloadCmdHash verifies a LOCALLY produced directory output records every entry under it (node, subdirs, files) with the producing step's cmdhash, so the whole subtree survives a state save/reload as generated output.
// Without the cmdhash, initDir drops a subdir on reload and inner files reconcile as sources; this is the local counterpart of TestState_DirOutput_ReloadPreservesCmdHash (which covers only the hand-built remote-style entries).
func TestBuild_DirOutputLocalReloadCmdHash(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	requirePython3(t)
	ctx := t.Context()
	dir := tempDir(t)
	// Reuse the local dir-output fixture: gen/ with gen/data and
	// gen/sub/nested, produced by a local step.
	setupFiles(t, dir, "TestBuild_DirOutputLocalCleandead", nil)

	func() {
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:   ".siso_fs_state",
			OutputLocal: func(context.Context, string) bool { return true },
		})
		defer cleanup()
		if _, err := ninjabuild.Run(ctx, graph, opt, []string{"all"}, ninjabuild.RunNinjaOpts{}); err != nil {
			t.Fatalf("ninja err: %v", err)
		}
	}()

	st, err := hashfs.Load(ctx, hashfs.Option{StateFile: filepath.Join(dir, "out/siso/.siso_fs_state")})
	if err != nil {
		t.Fatalf("hashfs.Load: %v", err)
	}
	m := hashfs.StateMap(digest.SHA256, st)
	for _, rel := range []string{
		"out/siso/gen",
		"out/siso/gen/data",
		"out/siso/gen/sub",
		"out/siso/gen/sub/nested",
	} {
		key := filepath.ToSlash(filepath.Join(dir, rel))
		ent, ok := m[key]
		if !ok {
			t.Errorf("%s not present in saved state (every entry under a dir output must survive reload)", rel)
			continue
		}
		if len(ent.GetCmdHash()) == 0 {
			t.Errorf("%s has empty CmdHash; want the producing step's cmdhash (entries under a declared dir output are generated outputs)", rel)
		}
	}
}

func TestBuild_DirInput_Sandbox(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("sandbox is only available on linux")
	}
	if !runInSubProcess(t) {
		return
	}
	ctx := t.Context()
	dir := tempDir(t)

	setupFiles(t, dir, t.Name(), nil)

	opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
		StateFile: ".siso_fs_state",
	})
	stats, err := ninjabuild.Run(ctx, graph, opt, nil, ninjabuild.RunNinjaOpts{})
	cleanup()
	if err != nil {
		t.Fatalf("ninja err: %v", err)
	}
	if stats.Done != stats.Total || stats.Total != 3 {
		t.Errorf("done=%d total=%d; want done=total=3", stats.Done, stats.Total)
	}
	buf, err := os.ReadFile(filepath.Join(dir, "out/siso/nsjail.config"))
	if err != nil {
		t.Fatalf("read nsjail.config: %v", err)
	}
	config := &nsjailpb.NsJailConfig{}
	err = prototext.Unmarshal(buf, config)
	if err != nil {
		t.Fatalf("unmarshal nsjail.config: %v\n%s", err, buf)
	}
	// Assert that input_dir/ is bind-mounted as a directory!
	if !slices.ContainsFunc(config.Mount, func(mount *nsjailpb.MountPt) bool {
		return mount.GetDst() == "/src/out/siso/input_dir" && mount.GetIsBind() && mount.GetIsDir()
	}) {
		t.Errorf("missing input_dir directory mount or wrong config\n%s", buf)
	}
}
