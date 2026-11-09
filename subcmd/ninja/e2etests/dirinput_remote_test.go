// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package e2etests

import (
	"archive/zip"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/build/ninjabuild"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/reapi/reapitest"
)

// TestBuild_DirInput_Remote runs the dir-input pipeline (unzip into a directory output, then mkzip from it) end to end against a real kajiya backend, exercising the actual REAPI flow (input upload, execution, output digest, tree upload, download into hashfs).
func TestBuild_DirInput_Remote(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	requirePython3(t)

	tests := []struct {
		name        string
		unzipRemote bool
		mkzipRemote bool
	}{
		{name: "AllRemote", unzipRemote: true, mkzipRemote: true},
		{name: "RemoteUnzip", unzipRemote: true, mkzipRemote: false},
		{name: "RemoteMkzip", unzipRemote: false, mkzipRemote: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testDirInputRemote(t, tt.unzipRemote, tt.mkzipRemote)
		})
	}
}

func testDirInputRemote(t *testing.T, unzipRemote, mkzipRemote bool) {
	ctx := t.Context()
	dir := tempDir(t)

	var ds build.DataSource
	defer func() {
		err := ds.Close(ctx)
		if err != nil {
			t.Error(err)
		}
	}()
	// Real kajiya local executor (unsandboxed) running python3 + unzip.py/mkzip.py.
	ds.Client = reapitest.NewLocalExec(ctx, t)
	ds.Cache = ds.Client.CacheStore()

	starConfig := generateStarConfig(unzipRemote, mkzipRemote)
	starPath := filepath.Join(dir, "build/config/siso/main.star")

	runNinja := func(t *testing.T) (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:   ".siso_fs_state",
			DataSource:  ds,
			OutputLocal: func(context.Context, string) bool { return true },
		})
		defer cleanup()
		opt.REAPIClient = ds.Client
		return ninjabuild.Run(ctx, graph, opt, nil, ninjabuild.RunNinjaOpts{})
	}

	setupFiles(t, dir, "TestBuild_DirInput_Remote", nil)
	writeInputZip(t, dir)
	if err := os.WriteFile(starPath, []byte(starConfig), 0644); err != nil {
		t.Fatalf("write star config: %v", err)
	}

	// Assert exact contents: real execution materializes the files from the zip
	// rather than fabricating digests.
	helloContent := []byte("Hello, world!\n")
	nestedContent := []byte("Nested file\n")

	t.Logf("-- first build (unzipRemote=%v, mkzipRemote=%v)", unzipRemote, mkzipRemote)
	stats, err := runNinja(t)
	if err != nil {
		t.Fatalf("ninja err: %v", err)
	}
	t.Logf("first build stats: done=%d skipped=%d remote=%d local=%d", stats.Done, stats.Skipped, stats.Remote, stats.Local)

	wantRemote := 0
	if unzipRemote {
		wantRemote++
	}
	if mkzipRemote {
		wantRemote++
	}
	if stats.Remote != wantRemote {
		t.Errorf("remote=%d; want %d", stats.Remote, wantRemote)
	}

	wantFiles := map[string][]byte{
		"out/siso/obj/extracted/hello.txt":         helloContent,
		"out/siso/obj/extracted/subdir/nested.txt": nestedContent,
	}
	for fname, want := range wantFiles {
		buf, err := os.ReadFile(filepath.Join(dir, fname))
		if err != nil {
			t.Errorf("readfile(%q): %v", fname, err)
			continue
		}
		if string(buf) != string(want) {
			t.Errorf("%s content=%q; want=%q", fname, buf, want)
		}
	}

	// Just check the repackaged zip exists; the local TestBuild_DirInput covers
	// content invariants for the same scripts.
	_, err = os.ReadFile(filepath.Join(dir, "out/siso/obj/repackaged.zip"))
	if err != nil {
		t.Errorf("readfile repackaged.zip: %v", err)
	}

	t.Logf("-- confirm no-op")
	stats, err = runNinja(t)
	if err != nil {
		t.Fatalf("ninja err: %v", err)
	}
	if stats.Skipped != stats.Total {
		t.Errorf("expected all steps skipped, got skipped=%d total=%d", stats.Skipped, stats.Total)
	}
}

func generateStarConfig(unzipRemote, mkzipRemote bool) string {
	unzipRule := `            {
                "name": "unzip",
                "action": "unzip",`
	if unzipRemote {
		unzipRule += `
                "remote": True,
                "platform_ref": "default",`
	}
	unzipRule += `
            }`

	mkzipRule := `            {
                "name": "mkzip",
                "action": "mkzip",`
	if mkzipRemote {
		mkzipRule += `
                "remote": True,
                "platform_ref": "default",`
	}
	mkzipRule += `
            }`

	config := `load("@builtin//encoding.star", "json")
load("@builtin//struct.star", "module")

def init(ctx):
    step_config = {
        "platforms": {
            "default": {
                "OSFamily": "Linux",
            },
        },
        "rules": [
` + unzipRule + `,
` + mkzipRule + `,
        ],
    }
    return module(
        "config",
        step_config = json.encode(step_config),
        filegroups = {},
        handlers = {},
    )
`
	return config
}

// readZipEntries opens a zip file and returns name -> content.
func readZipEntries(t *testing.T, path string) map[string]string {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatalf("open zip %s: %v", path, err)
	}
	defer zr.Close()
	out := map[string]string{}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open zip entry %s: %v", f.Name, err)
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("read zip entry %s: %v", f.Name, err)
		}
		out[filepath.ToSlash(f.Name)] = string(b)
	}
	return out
}

// TestBuild_DirInput_Remote_MkzipContents runs unzip locally and mkzip remotely and asserts the re-zipped output round-trips: a directory output consumed by a remote action must reach that action's input tree.
func TestBuild_DirInput_Remote_MkzipContents(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	requirePython3(t)

	ctx := t.Context()
	dir := tempDir(t)

	var ds build.DataSource
	defer func() { _ = ds.Close(ctx) }()
	ds.Client = reapitest.NewLocalExec(ctx, t)
	ds.Cache = ds.Client.CacheStore()

	starConfig := generateStarConfig(false /*unzipRemote*/, true /*mkzipRemote*/)
	starPath := filepath.Join(dir, "build/config/siso/main.star")

	setupFiles(t, dir, "TestBuild_DirInput_Remote", nil)
	writeInputZip(t, dir)
	if err := os.WriteFile(starPath, []byte(starConfig), 0644); err != nil {
		t.Fatalf("write star config: %v", err)
	}

	opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
		StateFile:   ".siso_fs_state",
		DataSource:  ds,
		OutputLocal: func(context.Context, string) bool { return true },
	})
	defer cleanup()
	opt.REAPIClient = ds.Client

	if _, err := ninjabuild.Run(ctx, graph, opt, nil, ninjabuild.RunNinjaOpts{}); err != nil {
		t.Fatalf("ninja err: %v", err)
	}

	got := readZipEntries(t, filepath.Join(dir, "out/siso/obj/repackaged.zip"))
	want := map[string]string{
		"hello.txt":         "Hello, world!\n",
		"subdir/nested.txt": "Nested file\n",
	}
	for name, content := range want {
		if got[name] != content {
			t.Errorf("repackaged.zip[%q] = %q; want %q (dir input not delivered to remote action)", name, got[name], content)
		}
	}
}

// TestBuild_DirOutput_RemoteProducedConsumedLocally locks in that a directory output produced REMOTELY with output_local=false is still fully available on disk to a LOCAL consumer.
// The producer does not flush it; the guarantee comes from the consumer's prepareLocalInputs flushing its directory input via hashFS.Flush.
func TestBuild_DirOutput_RemoteProducedConsumedLocally(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	requirePython3(t)

	ctx := t.Context()
	dir := tempDir(t)

	var ds build.DataSource
	defer func() { _ = ds.Close(ctx) }()
	ds.Client = reapitest.NewLocalExec(ctx, t)
	ds.Cache = ds.Client.CacheStore()

	// unzip remote (produces obj/extracted/), mkzip local (consumes it).
	starConfig := generateStarConfig(true /*unzipRemote*/, false /*mkzipRemote*/)
	starPath := filepath.Join(dir, "build/config/siso/main.star")
	setupFiles(t, dir, "TestBuild_DirInput_Remote", nil)
	writeInputZip(t, dir)
	if err := os.WriteFile(starPath, []byte(starConfig), 0644); err != nil {
		t.Fatalf("write star config: %v", err)
	}

	outputLocalFalse := func(context.Context, string) bool { return false }
	opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
		StateFile:   ".siso_fs_state",
		DataSource:  ds,
		OutputLocal: outputLocalFalse,
	})
	defer cleanup()
	opt.REAPIClient = ds.Client

	if _, err := ninjabuild.Run(ctx, graph, opt, nil, ninjabuild.RunNinjaOpts{}); err != nil {
		t.Fatalf("ninja err: %v", err)
	}

	got := readZipEntries(t, filepath.Join(dir, "out/siso/obj/repackaged.zip"))
	want := map[string]string{
		"hello.txt":         "Hello, world!\n",
		"subdir/nested.txt": "Nested file\n",
	}
	for name, content := range want {
		if got[name] != content {
			t.Errorf("repackaged.zip[%q] = %q; want %q (remote-only dir output not materialized to disk for the local consumer)", name, got[name], content)
		}
	}
}

// TestBuild_DirOutput_RemoteOutputLocalFalseSkipsDisk verifies a directory output produced remotely with output_local=false is NOT materialized to local disk when only remote steps consume it (they read it from CAS via the action input tree).
func TestBuild_DirOutput_RemoteOutputLocalFalseSkipsDisk(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	requirePython3(t)

	ctx := t.Context()
	dir := tempDir(t)

	var ds build.DataSource
	defer func() { _ = ds.Close(ctx) }()
	ds.Client = reapitest.NewLocalExec(ctx, t)
	ds.Cache = ds.Client.CacheStore()

	// Both steps remote: the directory output is consumed via the action input tree.
	starConfig := generateStarConfig(true /*unzipRemote*/, true /*mkzipRemote*/)
	starPath := filepath.Join(dir, "build/config/siso/main.star")
	setupFiles(t, dir, "TestBuild_DirInput_Remote", nil)
	writeInputZip(t, dir)
	if err := os.WriteFile(starPath, []byte(starConfig), 0644); err != nil {
		t.Fatalf("write star config: %v", err)
	}

	outputLocalFalse := func(context.Context, string) bool { return false }
	opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
		StateFile:   ".siso_fs_state",
		DataSource:  ds,
		OutputLocal: outputLocalFalse,
	})
	defer cleanup()
	opt.REAPIClient = ds.Client

	stats, err := ninjabuild.Run(ctx, graph, opt, nil, ninjabuild.RunNinjaOpts{})
	if err != nil {
		t.Fatalf("ninja err: %v", err)
	}
	if stats.Remote != 2 {
		t.Errorf("remote=%d; want 2 (both steps remote)", stats.Remote)
	}

	for _, f := range []string{
		"out/siso/obj/extracted/hello.txt",
		"out/siso/obj/extracted/subdir/nested.txt",
	} {
		if _, err := os.Stat(filepath.Join(dir, f)); !os.IsNotExist(err) {
			t.Errorf("output_local=false but %q materialized to disk (stat err=%v)", f, err)
		}
	}
}

// TestBuild_DirOutput_RemoteOutputLocalFalseReload is the build-without-bytes counterpart of the local reload test: a CAS-only directory output (output_local=false) must keep every entry (files AND subdirs) across a state reload, with no disk to re-stat, so the second build is a full no-op.
// Exercises three reload layers together: directory entries reload not-local (newEntryFromUpdate respects IsLocal), Stat returns a cmdhash'd not-on-disk directory, and storeDirs stores parents before children so the subtree's cmdhash is not orphaned.
func TestBuild_DirOutput_RemoteOutputLocalFalseReload(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	requirePython3(t)

	ctx := t.Context()
	dir := tempDir(t)

	var ds build.DataSource
	defer func() { _ = ds.Close(ctx) }()
	ds.Client = reapitest.NewLocalExec(ctx, t)
	ds.Cache = ds.Client.CacheStore()

	outputLocalFalse := func(context.Context, string) bool { return false }
	runNinja := func(t *testing.T) build.Stats {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:   ".siso_fs_state",
			DataSource:  ds,
			OutputLocal: outputLocalFalse,
		})
		defer cleanup()
		opt.REAPIClient = ds.Client
		stats, err := ninjabuild.Run(ctx, graph, opt, nil, ninjabuild.RunNinjaOpts{})
		if err != nil {
			t.Fatalf("ninja err: %v", err)
		}
		return stats
	}

	starConfig := generateStarConfig(true /*unzipRemote*/, true /*mkzipRemote*/)
	setupFiles(t, dir, "TestBuild_DirInput_Remote", nil)
	writeInputZip(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "build/config/siso/main.star"), []byte(starConfig), 0644); err != nil {
		t.Fatalf("write star config: %v", err)
	}

	notOnDisk := []string{
		"out/siso/obj/extracted/hello.txt",
		"out/siso/obj/extracted/subdir/nested.txt",
	}

	t.Logf("-- first build (output_local=false; dir contents stay in CAS)")
	stats := runNinja(t)
	if stats.Remote != 2 {
		t.Errorf("remote=%d; want 2 (both steps remote)", stats.Remote)
	}
	for _, f := range notOnDisk {
		if _, err := os.Stat(filepath.Join(dir, f)); !os.IsNotExist(err) {
			t.Errorf("output_local=false but %q materialized to disk (stat err=%v)", f, err)
		}
	}

	t.Logf("-- second build (state reload): the whole CAS-only directory output survives, so the build is a no-op")
	stats = runNinja(t)
	if stats.Skipped != stats.Total || stats.Remote != 0 {
		t.Errorf("reload not a no-op: done=%d skipped=%d total=%d remote=%d; a build-without-bytes directory output must survive reload so the consumer is not re-triggered", stats.Done, stats.Skipped, stats.Total, stats.Remote)
	}
	for _, f := range notOnDisk {
		if _, err := os.Stat(filepath.Join(dir, f)); !os.IsNotExist(err) {
			t.Errorf("after reload, output_local=false but %q materialized to disk (stat err=%v)", f, err)
		}
	}
}

// extractedDirInputFiles is the on-disk content of obj/extracted/ after
// unzipping the TestBuild_DirInput_Remote input.zip fixture.
var extractedDirInputFiles = map[string]string{
	"out/siso/obj/extracted/hello.txt":         "Hello, world!\n",
	"out/siso/obj/extracted/subdir/nested.txt": "Nested file\n",
}

// TestBuild_DirOutput_CleandeadPreservesContents builds a directory output remotely, then runs cleandead: the inner files of a live directory output must survive even though they are not ninja graph nodes.
func TestBuild_DirOutput_CleandeadPreservesContents(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	requirePython3(t)

	ctx := t.Context()
	dir := tempDir(t)

	var ds build.DataSource
	defer func() { _ = ds.Close(ctx) }()
	ds.Client = reapitest.NewLocalExec(ctx, t)
	ds.Cache = ds.Client.CacheStore()

	starConfig := generateStarConfig(true /*unzipRemote*/, false /*mkzipRemote*/)
	starPath := filepath.Join(dir, "build/config/siso/main.star")
	setupFiles(t, dir, "TestBuild_DirInput_Remote", nil)
	writeInputZip(t, dir)
	if err := os.WriteFile(starPath, []byte(starConfig), 0644); err != nil {
		t.Fatalf("write star config: %v", err)
	}

	run := func(cleandead bool, subtool string) {
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:   ".siso_fs_state",
			DataSource:  ds,
			OutputLocal: func(context.Context, string) bool { return true },
		})
		defer cleanup()
		opt.REAPIClient = ds.Client
		if _, err := ninjabuild.Run(ctx, graph, opt, nil, ninjabuild.RunNinjaOpts{
			Cleandead: cleandead,
			Subtool:   subtool,
		}); err != nil {
			t.Fatalf("ninja (cleandead=%v): %v", cleandead, err)
		}
	}

	run(false, "")
	inner := []string{
		"out/siso/obj/extracted/hello.txt",
		"out/siso/obj/extracted/subdir/nested.txt",
	}
	for _, f := range inner {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("after build, %q missing: %v", f, err)
		}
	}

	run(true, "cleandead")
	// "Preserved" must mean byte-intact, not merely present: read content back.
	for f, want := range extractedDirInputFiles {
		if got := readFile(t, filepath.Join(dir, f)); got != want {
			t.Errorf("%s = %q, want %q", filepath.Join(dir, f), got, want)
		}
	}
}

// TestBuild_DirOutput_CleandeadRemovesWhenTargetGone is the complement of the previous test: once the directory-output target leaves the graph, cleandead removes the whole directory (contents included).
func TestBuild_DirOutput_CleandeadRemovesWhenTargetGone(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	requirePython3(t)

	ctx := t.Context()
	dir := tempDir(t)

	var ds build.DataSource
	defer func() { _ = ds.Close(ctx) }()
	ds.Client = reapitest.NewLocalExec(ctx, t)
	ds.Cache = ds.Client.CacheStore()

	starConfig := generateStarConfig(true /*unzipRemote*/, false /*mkzipRemote*/)
	starPath := filepath.Join(dir, "build/config/siso/main.star")
	setupFiles(t, dir, "TestBuild_DirInput_Remote", nil)
	writeInputZip(t, dir)
	if err := os.WriteFile(starPath, []byte(starConfig), 0644); err != nil {
		t.Fatalf("write star config: %v", err)
	}

	run := func(cleandead bool, subtool string) {
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:   ".siso_fs_state",
			DataSource:  ds,
			OutputLocal: func(context.Context, string) bool { return true },
		})
		defer cleanup()
		opt.REAPIClient = ds.Client
		if _, err := ninjabuild.Run(ctx, graph, opt, nil, ninjabuild.RunNinjaOpts{
			Cleandead: cleandead,
			Subtool:   subtool,
		}); err != nil {
			t.Fatalf("ninja (cleandead=%v): %v", cleandead, err)
		}
	}

	run(false, "")
	extracted := filepath.Join(dir, "out/siso/obj/extracted")
	if _, err := os.Stat(extracted); err != nil {
		t.Fatalf("after build, %q missing: %v", extracted, err)
	}

	// Drop the directory-output target from the graph.
	newNinja := "build all: phony\nbuild build.ninja: phony\n"
	if err := os.WriteFile(filepath.Join(dir, "out/siso/build.ninja"), []byte(newNinja), 0644); err != nil {
		t.Fatal(err)
	}

	run(true, "cleandead")
	if _, err := os.Stat(extracted); !os.IsNotExist(err) {
		t.Errorf("cleandead did not remove dead directory output %q: stat err=%v", extracted, err)
	}
}

// TestBuild_DirOutput_CleandeadPreservesInputOnlyDir guards against data loss: a previously-generated directory no longer produced but still CONSUMED as an input (OutEdges, no InEdge) must not be removed by cleandead.
func TestBuild_DirOutput_CleandeadPreservesInputOnlyDir(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	requirePython3(t)

	ctx := t.Context()
	dir := tempDir(t)

	var ds build.DataSource
	defer func() { _ = ds.Close(ctx) }()
	ds.Client = reapitest.NewLocalExec(ctx, t)
	ds.Cache = ds.Client.CacheStore()

	starConfig := generateStarConfig(true /*unzipRemote*/, false /*mkzipRemote*/)
	starPath := filepath.Join(dir, "build/config/siso/main.star")
	setupFiles(t, dir, "TestBuild_DirInput_Remote", nil)
	writeInputZip(t, dir)
	if err := os.WriteFile(starPath, []byte(starConfig), 0644); err != nil {
		t.Fatalf("write star config: %v", err)
	}

	run := func(cleandead bool, subtool string) {
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:   ".siso_fs_state",
			DataSource:  ds,
			OutputLocal: func(context.Context, string) bool { return true },
		})
		defer cleanup()
		opt.REAPIClient = ds.Client
		if _, err := ninjabuild.Run(ctx, graph, opt, nil, ninjabuild.RunNinjaOpts{
			Cleandead: cleandead,
			Subtool:   subtool,
		}); err != nil {
			t.Fatalf("ninja (cleandead=%v): %v", cleandead, err)
		}
	}

	run(false, "")
	inner := []string{
		"out/siso/obj/extracted/hello.txt",
		"out/siso/obj/extracted/subdir/nested.txt",
	}
	for _, f := range inner {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("after build, %q missing: %v", f, err)
		}
	}

	// New graph: obj/extracted/ is no longer produced but is still consumed as
	// an input by mkzip (an input-only dir node).
	newNinja := `rule mkzip
  command = python3 ../../tools/mkzip.py ${out} ${in_dir}

build obj/repackaged.zip: mkzip obj/extracted/ | ../../tools/mkzip.py
  in_dir = obj/extracted

build all: phony obj/repackaged.zip

build build.ninja: phony
`
	if err := os.WriteFile(filepath.Join(dir, "out/siso/build.ninja"), []byte(newNinja), 0644); err != nil {
		t.Fatal(err)
	}

	run(true, "cleandead")
	// The still-consumed input dir's files must survive byte-intact.
	for f, want := range extractedDirInputFiles {
		if got := readFile(t, filepath.Join(dir, f)); got != want {
			t.Errorf("%s = %q, want %q", filepath.Join(dir, f), got, want)
		}
	}
}

// runCachedRemoteBuild runs one ninja build of the named testdata against the shared kajiya backend with the action cache enabled for read, using a fresh work dir each call (so a build hits remote exec when cold or the action cache when warm), returning the stats and work dir.
func runCachedRemoteBuild(ctx context.Context, t *testing.T, ds build.DataSource, name string) (build.Stats, string) {
	t.Helper()
	dir := tempDir(t)
	setupFiles(t, dir, name, nil)
	opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
		StateFile:   ".siso_fs_state",
		DataSource:  ds,
		OutputLocal: func(context.Context, string) bool { return true },
	})
	defer cleanup()
	bcache, err := build.NewCache(ctx, build.CacheOptions{
		Store:      ds.Cache,
		EnableRead: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	opt.Cache = bcache
	opt.RECacheEnableRead = true
	opt.REAPIClient = ds.Client
	opt.REExecEnable = true
	stats, err := ninjabuild.Run(ctx, graph, opt, []string{"all"}, ninjabuild.RunNinjaOpts{})
	if err != nil {
		t.Fatalf("ninja err: %v", err)
	}
	return stats, dir
}

// TestBuild_DirOutputCacheHit exercises the action-cache-hit path for a step producing BOTH a file output and a directory output: after the cache hit the directory's inner files must be present on disk, exactly as after a fresh remote execution.
// Before directory outputs were expanded on the recording path, the cache hit recorded only the bare directory node and the directory materialized empty.
func TestBuild_DirOutputCacheHit(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	requirePython3(t)
	ctx := t.Context()

	var ds build.DataSource
	defer func() { _ = ds.Close(ctx) }()
	ds.Client = reapitest.NewLocalExec(ctx, t)
	ds.Cache = ds.Client.CacheStore()

	// base/input is "hello\n"; genmixed prefixes each output.
	wantContent := map[string]string{
		"out/siso/out.txt":            "FILE:hello\n",
		"out/siso/gen/inner.txt":      "INNER:hello\n",
		"out/siso/gen/sub/nested.txt": "NESTED:hello\n",
	}

	t.Logf("-- first build (populates action cache via remote exec)")
	stats1, dir1 := runCachedRemoteBuild(ctx, t, ds, "TestBuild_DirOutputCacheHit")
	t.Logf("build1: done=%d remote=%d cachehit=%d local=%d", stats1.Done, stats1.Remote, stats1.CacheHit, stats1.Local)
	for f, want := range wantContent {
		if got := readFile(t, filepath.Join(dir1, f)); got != want {
			t.Errorf("%s = %q, want %q", filepath.Join(dir1, f), got, want)
		}
	}

	t.Logf("-- second build (fresh dir, no siso state -> must hit the action cache)")
	stats2, dir2 := runCachedRemoteBuild(ctx, t, ds, "TestBuild_DirOutputCacheHit")
	t.Logf("build2: done=%d remote=%d cachehit=%d local=%d", stats2.Done, stats2.Remote, stats2.CacheHit, stats2.Local)
	if stats2.CacheHit == 0 {
		t.Fatalf("build2: expected a cache hit, got cachehit=0 (remote=%d local=%d); cannot exercise the cache-hit dir-output path", stats2.Remote, stats2.Local)
	}
	// The cache hit must restore inner files with their content, not empty placeholders.
	for f, want := range wantContent {
		if got := readFile(t, filepath.Join(dir2, f)); got != want {
			t.Errorf("%s = %q, want %q", filepath.Join(dir2, f), got, want)
		}
	}
}

// TestBuild_DirOnlyCacheHit covers a step whose ONLY output is an EMPTY directory: with no OutputFiles, the OutputDirectory is the sole evidence of an output and validateRemoteActionResult must accept it, or the step re-executes on every build.
// A non-empty dir output repopulates OutputFiles on record, so it would pass anyway; the empty case isolates directory-only acceptance.
func TestBuild_DirOnlyCacheHit(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	requirePython3(t)
	ctx := t.Context()

	var ds build.DataSource
	defer func() { _ = ds.Close(ctx) }()
	ds.Client = reapitest.NewLocalExec(ctx, t)
	ds.Cache = ds.Client.CacheStore()

	// assertEmptyDir checks the directory output exists on disk and is empty.
	assertEmptyDir := func(t *testing.T, dir string) {
		t.Helper()
		gen := filepath.Join(dir, "out/siso/gen")
		fi, err := os.Stat(gen)
		if err != nil {
			t.Errorf("directory output gen not on disk: %v", err)
			return
		}
		if !fi.IsDir() {
			t.Errorf("gen is not a directory: mode=%v", fi.Mode())
			return
		}
		ents, err := os.ReadDir(gen)
		if err != nil {
			t.Errorf("ReadDir(gen): %v", err)
			return
		}
		if len(ents) != 0 {
			t.Errorf("gen should be empty, has %d entries: %v", len(ents), ents)
		}
	}

	t.Logf("-- first build (remote exec populates the action cache)")
	stats1, dir1 := runCachedRemoteBuild(ctx, t, ds, "TestBuild_DirOnlyCacheHit")
	t.Logf("build1: done=%d remote=%d cachehit=%d local=%d", stats1.Done, stats1.Remote, stats1.CacheHit, stats1.Local)
	assertEmptyDir(t, dir1)

	t.Logf("-- second build (fresh dir, no siso state -> must be a cache HIT, not a re-exec)")
	stats2, dir2 := runCachedRemoteBuild(ctx, t, ds, "TestBuild_DirOnlyCacheHit")
	t.Logf("build2: done=%d remote=%d cachehit=%d local=%d", stats2.Done, stats2.Remote, stats2.CacheHit, stats2.Local)
	if stats2.CacheHit == 0 {
		t.Errorf("build2: cachehit=0 (remote=%d local=%d); a directory-only result must be cacheable, not re-executed every build", stats2.Remote, stats2.Local)
	}
	if stats2.Remote != 0 {
		t.Errorf("build2: remote=%d; want 0 (the dir-only step should be served from cache, not re-run)", stats2.Remote)
	}
	assertEmptyDir(t, dir2)
}
