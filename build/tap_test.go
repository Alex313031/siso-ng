// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/golang/glog"
	"google.golang.org/protobuf/types/known/anypb"

	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/execute"
	pb "go.chromium.org/build/siso/execute/proto"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/path"
)

func init() {
	glog.Infof("create log file before setting TMPDIR")
}

func setTapResult(cmd *execute.Cmd, reads, writes, deletes []string) error {
	tapData := &pb.TapResult{
		Reads:   reads,
		Writes:  writes,
		Deletes: deletes,
	}
	any, err := anypb.New(tapData)
	if err != nil {
		return err
	}
	res := &rpb.ActionResult{
		ExecutionMetadata: &rpb.ExecutedActionMetadata{
			AuxiliaryMetadata: []*anypb.Any{any},
		},
	}
	cmd.SetActionResult(res, false)
	return nil
}

func setupDirForTapTest(t *testing.T) (string, string) {
	tmpdir := t.TempDir()
	dir := t.TempDir()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", tmpdir)
	t.Setenv("TMP", tmpdir)
	if path.New(dir).HasPrefix(path.New(tmpdir)) {
		t.Fatalf("dir=%q under TMPDIR=%q", dir, tmpdir)
	}
	wsDir := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(wsDir, 0755); err != nil {
		t.Fatal(err)
	}
	customTmpDir := filepath.Join(dir, "workspace/out/soong/.temp")
	if err := os.MkdirAll(customTmpDir, 0755); err != nil {
		t.Fatal(err)
	}
	return dir, tmpdir
}

func TestTapCanonicalizeCmd_NoTapData(t *testing.T) {
	ctx := t.Context()
	dir, _ := setupDirForTapTest(t)
	wsDir := filepath.Join(dir, "workspace")

	hfs, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}
	defer hfs.Close(ctx)
	err = hfs.WaitReady(ctx)
	if err != nil {
		t.Fatal(err)
	}

	b := &Builder{
		hashFS: hfs,
		path:   NewPath(wsDir, "out/siso"),
	}

	cmd := &execute.Cmd{
		WorkspaceRoot: wsDir,
	}

	err = b.tapCanonicalizeCmd(ctx, cmd)
	if err == nil {
		t.Errorf("tapCanonicalizeCmd(cmd) = nil; want error")
	}
}

func TestTapCanonicalizeCmd(t *testing.T) {
	ctx := t.Context()
	dir, tmpDir := setupDirForTapTest(t)
	wsDir := filepath.Join(dir, "workspace")

	hfs, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}
	defer hfs.Close(ctx)
	if err := hfs.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}

	setupFile := func(root, rel string) string {
		t.Helper()
		abs := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte("content"), 0644); err != nil {
			t.Fatal(err)
		}
		return abs
	}
	setupDir := func(root, rel string) string {
		t.Helper()
		abs := filepath.Join(root, rel)
		if err := os.MkdirAll(abs, 0755); err != nil {
			t.Fatal(err)
		}
		return abs
	}

	b := &Builder{
		hashFS: hfs,
		path:   NewPath(wsDir, "out/siso"),
	}

	existingInput := setupFile(wsDir, "input_existing.h")
	newInput := setupFile(wsDir, "input_new.h")
	nonExistentInput := filepath.Join(wsDir, "input_nonexistent.h")
	outsideInput := setupFile(tmpDir, "outside_input.h")

	existingOutput := setupFile(wsDir, "out/siso/output_existing.o")
	newOutput := setupFile(wsDir, "out/siso/output_new.o")
	dirOutput := setupDir(wsDir, "out/siso/output_dir")

	toDeleteFile := setupFile(wsDir, "to_delete.txt")

	// Cache toDeleteFile in hashFS first
	_, err = hfs.Stat(ctx, wsDir, path.New("to_delete.txt"))
	if err != nil {
		t.Fatalf("hfs.Stat to_delete.txt failed: %v", err)
	}

	// Remove toDeleteFile from disk to simulate action deletion
	if err := os.Remove(toDeleteFile); err != nil {
		t.Fatal(err)
	}

	cmd := &execute.Cmd{
		WorkspaceRoot: wsDir,
		Inputs:        []path.Path{path.New("input_existing.h")},
		Outputs:       []path.Path{path.New("out/siso/output_existing.o")},
	}

	err = setTapResult(cmd,
		[]string{existingInput, newInput, nonExistentInput, outsideInput},
		[]string{existingOutput, newOutput, dirOutput},
		[]string{toDeleteFile},
	)
	if err != nil {
		t.Fatal(err)
	}

	err = b.tapCanonicalizeCmd(ctx, cmd)
	if err != nil {
		t.Fatalf("tapCanonicalizeCmd returned error: %v", err)
	}

	if !cmd.Pure {
		t.Errorf("cmd.Pure = false; want true")
	}

	wantInputs := []path.Path{path.New("input_existing.h"), path.New("input_new.h")}
	if !slices.Equal(cmd.Inputs, wantInputs) {
		t.Errorf("cmd.Inputs = %v; want %v", cmd.Inputs, wantInputs)
	}

	wantOutputs := []path.Path{path.New("out/siso/output_existing.o"), path.New("out/siso/output_new.o")}
	if !slices.Equal(cmd.Outputs, wantOutputs) {
		t.Errorf("cmd.Outputs = %v; want %v", cmd.Outputs, wantOutputs)
	}

	wantOutputDirs := []path.Path{path.New("out/siso/output_dir")}
	if !slices.Equal(cmd.OutputDirs, wantOutputDirs) {
		t.Errorf("cmd.OutputDirs = %v; want %v", cmd.OutputDirs, wantOutputDirs)
	}

	// Verify to_delete.txt is forgotten from hashFS and returns ErrNotExist
	_, err = hfs.Stat(ctx, wsDir, path.New("to_delete.txt"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("hfs.Stat(to_delete.txt) = %v; want %v after forget", err, fs.ErrNotExist)
	}
}

func TestTapCanonicalizeCmd_IgnoreOSTMPDIR(t *testing.T) {
	ctx := t.Context()
	dir, _ := setupDirForTapTest(t)
	wsDir := filepath.Join(dir, "workspace")
	customTmpDir := filepath.Join(dir, "workspace/out/soong/.temp")
	t.Setenv("TMPDIR", customTmpDir)
	t.Setenv("TMP", customTmpDir)

	hfs, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}
	defer hfs.Close(ctx)
	err = hfs.WaitReady(ctx)
	if err != nil {
		t.Fatal(err)
	}

	setupFile := func(abs string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(abs), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte("content"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	wsInput := filepath.Join(wsDir, "input.h")
	setupFile(wsInput)

	tmpRead := filepath.Join(os.TempDir(), "tmp_read.h")
	setupFile(tmpRead)
	defer os.Remove(tmpRead)

	customTmpRead := filepath.Join(customTmpDir, "custom_tmp_read.h")
	setupFile(customTmpRead)

	customTmpWrite := filepath.Join(customTmpDir, "custom_tmp_write.o")
	setupFile(customTmpWrite)

	b := &Builder{
		hashFS: hfs,
		path:   NewPath(wsDir, "out/siso"),
	}

	cmd := &execute.Cmd{
		WorkspaceRoot: wsDir,
	}

	err = setTapResult(cmd,
		[]string{wsInput, tmpRead, customTmpRead},
		[]string{customTmpWrite},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	err = b.tapCanonicalizeCmd(ctx, cmd)
	if err != nil {
		t.Fatalf("tapCanonicalizeCmd returned error: %v", err)
	}

	wantInputs := []path.Path{path.New("input.h")}
	if !slices.Equal(cmd.Inputs, wantInputs) {
		t.Errorf("cmd.Inputs = %v; want %v (tmp reads should be ignored)", cmd.Inputs, wantInputs)
	}

	if len(cmd.Outputs) != 0 {
		t.Errorf("cmd.Outputs = %v; want empty (tmp writes should be ignored)", cmd.Outputs)
	}
}

func TestTapCanonicalizeCmd_IgnoreCmdEnvTMPDIR(t *testing.T) {
	ctx := t.Context()
	dir, _ := setupDirForTapTest(t)
	wsDir := filepath.Join(dir, "workspace")
	customTmpDir := filepath.Join(dir, "workspace/out/soong/.temp")

	hfs, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}
	defer hfs.Close(ctx)
	err = hfs.WaitReady(ctx)
	if err != nil {
		t.Fatal(err)
	}

	setupFile := func(abs string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(abs), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte("content"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	wsInput := filepath.Join(wsDir, "input.h")
	setupFile(wsInput)

	tmpRead := filepath.Join(os.TempDir(), "tmp_read.h")
	setupFile(tmpRead)
	defer os.Remove(tmpRead)

	customTmpRead := filepath.Join(customTmpDir, "custom_tmp_read.h")
	setupFile(customTmpRead)

	customTmpWrite := filepath.Join(customTmpDir, "custom_tmp_write.o")
	setupFile(customTmpWrite)

	b := &Builder{
		hashFS: hfs,
		path:   NewPath(wsDir, "out/siso"),
	}

	cmd := &execute.Cmd{
		WorkspaceRoot: wsDir,
		Env:           []string{"TMPDIR=" + customTmpDir},
	}

	err = setTapResult(cmd,
		[]string{wsInput, tmpRead, customTmpRead},
		[]string{customTmpWrite},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	err = b.tapCanonicalizeCmd(ctx, cmd)
	if err != nil {
		t.Fatalf("tapCanonicalizeCmd returned error: %v", err)
	}

	wantInputs := []path.Path{path.New("input.h")}
	if !slices.Equal(cmd.Inputs, wantInputs) {
		t.Errorf("cmd.Inputs = %v; want %v (tmp reads should be ignored)", cmd.Inputs, wantInputs)
	}

	if len(cmd.Outputs) != 0 {
		t.Errorf("cmd.Outputs = %v; want empty (tmp writes should be ignored)", cmd.Outputs)
	}
}

func TestTapCanonicalizeCmd_Symlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no symlink test on windows")
		return
	}

	ctx := t.Context()
	dir, tmpDir := setupDirForTapTest(t)
	wsDir := filepath.Join(dir, "workspace")

	hfs, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}
	defer hfs.Close(ctx)
	if err := hfs.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}

	setupFile := func(rel string) {
		t.Helper()
		abs := filepath.Join(wsDir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte("content"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	setupSymlink := func(rel, target string) string {
		t.Helper()
		abs := filepath.Join(wsDir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, abs); err != nil {
			t.Fatal(err)
		}
		return abs
	}

	setupFile("target.h")
	symlinkInput := setupSymlink("symlink.h", "target.h")
	setupFile("dir/target2.h")
	subSymlinkInput := setupSymlink("symlink_sub.h", "dir/target2.h")
	outsideFile := filepath.Join(tmpDir, "outside.h")
	if err := os.WriteFile(outsideFile, []byte("content"), 0644); err != nil {
		t.Fatal(err)
	}
	outsideSymlinkInput := setupSymlink("symlink_outside.h", outsideFile)

	b := &Builder{
		hashFS: hfs,
		path:   NewPath(wsDir, "out/siso"),
	}

	cmd := &execute.Cmd{
		WorkspaceRoot: wsDir,
	}

	err = setTapResult(cmd,
		[]string{symlinkInput, subSymlinkInput, outsideSymlinkInput},
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	err = b.tapCanonicalizeCmd(ctx, cmd)
	if err != nil {
		t.Fatalf("tapCanonicalizeCmd returned error: %v", err)
	}

	wantInputs := []path.Path{
		path.New("symlink.h"),
		path.New("target.h"),
		path.New("symlink_sub.h"),
		path.New("dir/target2.h"),
		path.New("symlink_outside.h"),
	}
	if !slices.Equal(cmd.Inputs, wantInputs) {
		t.Errorf("cmd.Inputs = %v; want %v", cmd.Inputs, wantInputs)
	}
}

func TestTapCanonicalizeCmd_TapDetectedZero(t *testing.T) {
	dir, tmpDir := setupDirForTapTest(t)
	wsDir := filepath.Join(dir, "workspace")
	customTmpDir := filepath.Join(dir, "workspace/out/soong/.temp")

	setupFile := func(root, rel string) string {
		t.Helper()
		abs := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte("content"), 0644); err != nil {
			t.Fatal(err)
		}
		return abs
	}

	outsideInput := setupFile(tmpDir, "outside_input.h")
	outsideOutput := setupFile(tmpDir, "outside_output.o")
	customTmpInput := setupFile(customTmpDir, "custom_tmp_read.h")
	customTmpOutput := setupFile(customTmpDir, "custom_tmp_write.o")

	for _, tc := range []struct {
		name    string
		env     []string
		reads   []string
		writes  []string
		deletes []string
	}{
		{
			name: "empty",
		},
		{
			name:   "outside_workspace",
			reads:  []string{outsideInput},
			writes: []string{outsideOutput},
		},
		{
			name:   "nonexistent_files",
			reads:  []string{filepath.Join(wsDir, "nonexistent.h")},
			writes: []string{filepath.Join(wsDir, "out/siso/nonexistent.o")},
		},
		{
			name:   "only_tmpdir",
			env:    []string{"TMPDIR=" + customTmpDir},
			reads:  []string{customTmpInput},
			writes: []string{customTmpOutput},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			hfs, err := hashfs.New(ctx, hashfs.Option{})
			if err != nil {
				t.Fatal(err)
			}
			defer hfs.Close(ctx)
			if err := hfs.WaitReady(ctx); err != nil {
				t.Fatal(err)
			}

			b := &Builder{
				hashFS: hfs,
				path:   NewPath(wsDir, "out/siso"),
			}

			cmd := &execute.Cmd{
				WorkspaceRoot: wsDir,
				Env:           tc.env,
			}

			err = setTapResult(cmd, tc.reads, tc.writes, tc.deletes)
			if err != nil {
				t.Fatal(err)
			}

			err = b.tapCanonicalizeCmd(ctx, cmd)
			if err == nil {
				t.Fatalf("tapCanonicalizeCmd returned nil error; want error")
			}
			t.Logf("tapCanonicalizeCmd: %v", err)
			if cmd.Pure {
				t.Errorf("cmd.Pure = true; want false")
			}
		})
	}
}

func TestTapCanonicalizeCmd_OutputDir(t *testing.T) {
	ctx := t.Context()
	dir, _ := setupDirForTapTest(t)
	wsDir := filepath.Join(dir, "workspace")

	hfs, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}
	defer hfs.Close(ctx)
	if err := hfs.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}

	setupFile := func(root, rel string) string {
		t.Helper()
		abs := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte("content"), 0644); err != nil {
			t.Fatal(err)
		}
		return abs
	}

	b := &Builder{
		hashFS: hfs,
		path:   NewPath(wsDir, "out/siso"),
	}

	existingInput := setupFile(wsDir, "input.h")
	existingOutput := setupFile(wsDir, "out/siso/output.o")
	fileInDirOutput := setupFile(wsDir, "out/siso/output_dir/nested/file.txt")
	fileInAuxDirOutput := setupFile(wsDir, "out/siso/aux_dir/log.txt")
	standaloneOutput := setupFile(wsDir, "out/siso/standalone.txt")

	cmd := &execute.Cmd{
		WorkspaceRoot:          wsDir,
		Inputs:                 []path.Path{path.New("input.h")},
		Outputs:                []path.Path{path.New("out/siso/output.o")},
		OutputDirs:             []path.Path{path.New("out/siso/output_dir")},
		AuxiliaryLogOutputDirs: []path.Path{path.New("out/siso/aux_dir")},
	}

	err = setTapResult(cmd,
		[]string{existingInput},
		[]string{existingOutput, fileInDirOutput, fileInAuxDirOutput, standaloneOutput},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	err = b.tapCanonicalizeCmd(ctx, cmd)
	if err != nil {
		t.Fatalf("tapCanonicalizeCmd returned error: %v", err)
	}

	if !cmd.Pure {
		t.Errorf("cmd.Pure = false; want true")
	}

	wantOutputs := []path.Path{path.New("out/siso/output.o"), path.New("out/siso/standalone.txt")}
	if !slices.Equal(cmd.Outputs, wantOutputs) {
		t.Errorf("cmd.Outputs = %v; want %v", cmd.Outputs, wantOutputs)
	}
}

// Regression test for b/570100436:
//   - Declared outputs that unlink-before-write (reported in both Writes and
//     Deletes by rbetap) must retain their CmdHash/EdgeHash recorded by
//     RecordOutputsFromLocal.
//   - Tap-discovered undeclared outputs (whether overwritten in-place, unlinked
//     then recreated, or previously negative-cached as ErrNotExist in HashFS)
//     must be updated from local disk with fresh digests.
//   - Files that were written then deleted during execution (absent on local
//     disk when the command finishes) must be forgotten and excluded from
//     cmd.Outputs.
func TestTapCanonicalizeCmd_UnlinkBeforeWriteAndDiscoveredOutputs(t *testing.T) {
	ctx := t.Context()
	dir, _ := setupDirForTapTest(t)
	wsDir := filepath.Join(dir, "workspace")

	hfs, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}
	defer hfs.Close(ctx)
	if err := hfs.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}

	writeFile := func(rel, content string) string {
		t.Helper()
		abs := filepath.Join(wsDir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		return abs
	}

	b := &Builder{
		hashFS: hfs,
		path:   NewPath(wsDir, "out/siso"),
	}

	inputAbs := writeFile("input.txt", "input")

	// Pre-populate files from a prior build / prior step in HashFS.
	declaredRel := path.New("out/siso/declared.o")
	declaredAbs := writeFile(string(declaredRel), "old declared")

	discoveredStaleRel := path.New("out/siso/discovered_stale.o")
	discoveredStaleAbs := writeFile(string(discoveredStaleRel), "old stale")

	discoveredRecreatedRel := path.New("out/siso/discovered_recreated.o")
	discoveredRecreatedAbs := writeFile(string(discoveredRecreatedRel), "old recreated")

	tempDeletedRel := path.New("out/siso/temp_deleted.o")
	tempDeletedAbs := writeFile(string(tempDeletedRel), "temp content")

	// Force HashFS to cache the old entries and digests (and a negative entry
	// for discovered_neg.o) before the command runs.
	preExecPaths := []path.Path{declaredRel, discoveredStaleRel, discoveredRecreatedRel, tempDeletedRel}
	if _, err := hfs.Entries(ctx, wsDir, preExecPaths); err != nil {
		t.Fatalf("hfs.Entries(ctx, %q, %v) = _, %v; want _, nil", wsDir, preExecPaths, err)
	}
	discoveredNegRel := path.New("out/siso/discovered_neg.o")
	if fi, err := hfs.Stat(ctx, wsDir, discoveredNegRel); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("hfs.Stat(ctx, %q, %q) = %v, %v; want nil, %v", wsDir, discoveredNegRel, fi, err, fs.ErrNotExist)
	}

	// Simulate command execution on local disk:
	// 1. declared.o is unlinked and recreated with new content.
	writeFile(string(declaredRel), "new declared")
	// 2. discovered_stale.o is overwritten in place (not in Deletes).
	writeFile(string(discoveredStaleRel), "new stale")
	// 3. discovered_recreated.o is unlinked and recreated (in both Writes and Deletes).
	writeFile(string(discoveredRecreatedRel), "new recreated")
	// 4. discovered_neg.o is newly created on disk (in Writes only).
	discoveredNegAbs := writeFile(string(discoveredNegRel), "new neg")
	// 5. temp_deleted.o is written then deleted before the command exits.
	if err := os.Remove(tempDeletedAbs); err != nil {
		t.Fatal(err)
	}

	cmdHash := []byte("declared-cmd-hash")
	edgeHash := []byte("declared-edge-hash")
	cmd := &execute.Cmd{
		WorkspaceRoot: wsDir,
		WorkDir:       "out/siso",
		Inputs:        []path.Path{path.New("input.txt")},
		Outputs:       []path.Path{declaredRel},
		CmdHash:       cmdHash,
		EdgeHash:      edgeHash,
		HashFS:        hfs,
	}
	cmd.InitOutputs()

	// LocalExec.Run calls RecordOutputsFromLocal before Builder.execLocal calls tapCanonicalizeCmd.
	if err := cmd.RecordOutputsFromLocal(ctx, b.start); err != nil {
		t.Fatalf("cmd.RecordOutputsFromLocal(ctx, %v) = %v; want nil", b.start, err)
	}

	err = setTapResult(cmd,
		[]string{inputAbs},
		[]string{declaredAbs, discoveredStaleAbs, discoveredRecreatedAbs, discoveredNegAbs, tempDeletedAbs},
		[]string{declaredAbs, discoveredRecreatedAbs, tempDeletedAbs},
	)
	if err != nil {
		t.Fatal(err)
	}

	if err := b.tapCanonicalizeCmd(ctx, cmd); err != nil {
		t.Fatalf("b.tapCanonicalizeCmd(ctx, cmd) = %v; want nil", err)
	}

	// 1. Declared output must retain its CmdHash and EdgeHash in HashFS.
	fi, err := hfs.Stat(ctx, wsDir, declaredRel)
	if err != nil {
		t.Fatalf("hfs.Stat(ctx, %q, %q) = nil, %v; want _, nil", wsDir, declaredRel, err)
	}
	if got := fi.CmdHash(); !slices.Equal(got, cmdHash) {
		t.Errorf("fi.CmdHash() for %q = %q; want %q", declaredRel, got, cmdHash)
	}
	if got := fi.EdgeHash(); !slices.Equal(got, edgeHash) {
		t.Errorf("fi.EdgeHash() for %q = %q; want %q", declaredRel, got, edgeHash)
	}

	// 2. cmd.Outputs must include declared output and all 3 live discovered outputs,
	//    and exclude temp_deleted.o.
	wantOutputs := []path.Path{
		declaredRel,
		discoveredStaleRel,
		discoveredRecreatedRel,
		discoveredNegRel,
	}
	if !slices.Equal(cmd.Outputs, wantOutputs) {
		t.Errorf("cmd.Outputs = %v; want %v", cmd.Outputs, wantOutputs)
	}

	// 3. temp_deleted.o must be forgotten from HashFS.
	if fi, err := hfs.Stat(ctx, wsDir, tempDeletedRel); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("hfs.Stat(ctx, %q, %q) = %v, %v; want nil, %v", wsDir, tempDeletedRel, fi, err, fs.ErrNotExist)
	}

	// 4. All live outputs in HashFS must reflect the new on-disk contents.
	for _, tc := range []struct {
		rel  path.Path
		want string
	}{
		{declaredRel, "new declared"},
		{discoveredStaleRel, "new stale"},
		{discoveredRecreatedRel, "new recreated"},
		{discoveredNegRel, "new neg"},
	} {
		got, err := hfs.ReadFile(ctx, wsDir, tc.rel)
		if err != nil || string(got) != tc.want {
			t.Errorf("hfs.ReadFile(ctx, %q, %q) = %q, %v; want %q, nil", wsDir, tc.rel, got, err, tc.want)
		}
	}
}
