// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package execute

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"google.golang.org/protobuf/proto"

	"go.chromium.org/build/hashigo/digest"
	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/blob"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/path"
	"go.chromium.org/build/siso/reapi/merkletree"
)

func TestCanonicalizeDir(t *testing.T) {
	for _, tc := range []struct {
		name           string
		cmd            *Cmd
		ents           []merkletree.Entry
		treeInputs     []merkletree.TreeEntry
		wantEnts       []merkletree.Entry
		wantTreeInputs []merkletree.TreeEntry
	}{
		{
			name: "empty-dir",
			cmd: &Cmd{
				WorkDir: "",
			},
			ents: []merkletree.Entry{
				{
					Name: "out/Default",
				},
			},
			treeInputs: []merkletree.TreeEntry{
				{
					Name: "toolchain",
				},
			},
			wantEnts: []merkletree.Entry{
				{
					Name: "out/Default",
				},
			},
			wantTreeInputs: []merkletree.TreeEntry{
				{
					Name: "toolchain",
				},
			},
		},
		{
			name: "dot-dir",
			cmd: &Cmd{
				WorkDir: "",
			},
			ents: []merkletree.Entry{
				{
					Name: "out/Default",
				},
			},
			treeInputs: []merkletree.TreeEntry{
				{
					Name: "toolchain",
				},
			},
			wantEnts: []merkletree.Entry{
				{
					Name: "out/Default",
				},
			},
			wantTreeInputs: []merkletree.TreeEntry{
				{
					Name: "toolchain",
				},
			},
		},
		{
			name: "canonicalize-dir",
			cmd: &Cmd{
				WorkDir: "out/Default",
			},
			ents: []merkletree.Entry{
				{
					Name: "out/Default/file",
				},
				{
					Name: "out/Default/dir/file",
				},
				{
					Name: "file",
				},
				{
					Name: "dir/file",
				},
			},
			treeInputs: []merkletree.TreeEntry{
				{
					Name: "toolchain",
				},
				{
					Name: "out/Default/sdk",
				},
			},
			wantEnts: []merkletree.Entry{
				{
					Name: "out/x/file",
				},
				{
					Name: "out/x/dir/file",
				},
				{
					Name: "file",
				},
				{
					Name: "dir/file",
				},
			},
			wantTreeInputs: []merkletree.TreeEntry{
				{
					Name: "toolchain",
				},
				{
					Name: "out/x/sdk",
				},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			ents, treeInputs := tc.cmd.canonicalizeDir(ctx, tc.ents, tc.treeInputs)
			if diff := cmp.Diff(ents, tc.wantEnts, cmp.Comparer(cmpDigestData)); diff != "" {
				t.Errorf("ents: -got +want:\n%s", diff)
			}
			if diff := cmp.Diff(treeInputs, tc.wantTreeInputs); diff != "" {
				t.Errorf("treeInputs: -got +want:\n%s", diff)
			}
		})
	}
}

func cmpDigestData(a, b blob.Data) bool {
	return a.Digest() == b.Digest()
}

func TestEntriesFromResult_Auxiliary(t *testing.T) {
	ctx := t.Context()
	now := time.Now()

	d1 := digest.Digest{Hash: "hash1", SizeBytes: 1}
	d2 := digest.Digest{Hash: "hash2", SizeBytes: 2}
	tree := &rpb.Tree{
		Root: &rpb.Directory{
			Files: []*rpb.FileNode{
				{
					Name:   "file",
					Digest: d1.Proto(),
				},
			},
		},
	}
	treeData, err := blob.FromProtoMessage(digest.SHA256, tree)
	if err != nil {
		t.Fatal(err)
	}
	d3 := treeData.Digest()

	cmd := &Cmd{
		WorkDir: "out/Default",
		Outputs: []path.Path{
			"out/Default/main.o",
		},
		AuxiliaryLogOutputFiles: []path.Path{
			"out/Default/aux.d",
		},
		AuxiliaryLogOutputDirs: []path.Path{
			"out/Default/aux_dir",
		},
		outfiles: map[path.Path]bool{
			"out/Default/main.o": true,
		},
		actionResult: &rpb.ActionResult{
			OutputFiles: []*rpb.OutputFile{
				{Path: "main.o", Digest: d1.Proto()},
				{Path: "aux.d", Digest: d2.Proto()},
			},
			OutputDirectories: []*rpb.OutputDirectory{
				{Path: "aux_dir", TreeDigest: d3.Proto()},
			},
		},
	}

	ds := mockDataSource{}
	entries, additionalEntries := cmd.entriesFromResult(ctx, ds, now)

	// Verify main outputs (entries)
	wantEntries := []hashfs.UpdateEntry{
		{
			Name: "out/Default/main.o",
			Entry: &merkletree.Entry{
				Name: "out/Default/main.o",
				Data: blob.NewData(ds.Source(ctx, d1, "out/Default/main.o"), d1),
			},
			UpdatedTime: now,
			ModTime:     now,
			IsChanged:   true,
			Mode:        0644,
		},
	}
	if diff := cmp.Diff(entries, wantEntries, cmpopts.IgnoreUnexported(hashfs.UpdateEntry{}, merkletree.Entry{}, blob.Data{}), cmpopts.IgnoreFields(hashfs.UpdateEntry{}, "Action")); diff != "" {
		t.Errorf("entries mismatch (-got +want):\n%s", diff)
	}
	// cmp.Diff ignores blob.Data's unexported fields, so the content digest
	// is not compared above. Assert it explicitly: main.o must carry d1.
	if len(entries) == 1 {
		if got := entries[0].Entry.Data.Digest(); got != d1 {
			t.Errorf("main.o entry digest = %v, want %v", got, d1)
		}
	}

	// Verify additional outputs (should be empty for auxiliary outputs)
	if len(additionalEntries) != 0 {
		t.Errorf("additionalEntries: got %q, want empty", additionalEntries)
	}

}

// TestEntriesFromResult_DirOutput verifies entriesFromResult produces a CmdHash'd directory entry for the root plus CmdHash'd file entries for files under the directory output.
func TestEntriesFromResult_DirOutput(t *testing.T) {
	ctx := t.Context()
	now := time.Now()

	d1 := digest.Digest{Hash: "hash1", SizeBytes: 5}
	treeDg := digest.Digest{Hash: "tree1", SizeBytes: 10}

	cmd := &Cmd{
		WorkDir: "out/Default",
		OutputDirs: []path.Path{
			"out/Default/gendir",
		},
		outfiles: map[path.Path]bool{
			"out/Default/gendir": true,
		},
		CmdHash: []byte("test-cmd-hash"),
		actionResult: &rpb.ActionResult{
			OutputFiles: []*rpb.OutputFile{
				{Path: "gendir/hello.txt", Digest: d1.Proto()},
			},
			OutputDirectories: []*rpb.OutputDirectory{
				{Path: "gendir", TreeDigest: treeDg.Proto()},
			},
		},
	}

	ds := mockDataSource{}
	entries, additionalEntries := cmd.entriesFromResult(ctx, ds, now)

	// gendir/hello.txt is in entries with CmdHash because isOutputFile walks up
	// to "out/Default/gendir", which is in outfiles.
	wantEntries := []hashfs.UpdateEntry{
		{
			Name: "out/Default/gendir/hello.txt",
			Entry: &merkletree.Entry{
				Name: "out/Default/gendir/hello.txt",
				Data: blob.NewData(ds.Source(ctx, d1, "out/Default/gendir/hello.txt"), d1),
			},
			UpdatedTime: now,
			ModTime:     now,
			IsChanged:   true,
			Mode:        0644,
			CmdHash:     []byte("test-cmd-hash"),
		},
		{
			Name: "out/Default/gendir",
			Entry: &merkletree.Entry{
				Name: "out/Default/gendir",
			},
			UpdatedTime: now,
			ModTime:     now,
			IsChanged:   true,
			Mode:        fs.FileMode(0755) | fs.ModeDir,
			CmdHash:     []byte("test-cmd-hash"),
		},
	}
	if diff := cmp.Diff(entries, wantEntries, cmpopts.IgnoreUnexported(hashfs.UpdateEntry{}, merkletree.Entry{}, blob.Data{}), cmpopts.IgnoreFields(hashfs.UpdateEntry{}, "Action")); diff != "" {
		t.Errorf("entries mismatch (-got +want):\n%s", diff)
	}
	// cmp.Diff ignores blob.Data's unexported fields, so assert the flattened
	// file's content digest explicitly: gendir/hello.txt must carry d1.
	gotHello := false
	for _, e := range entries {
		if e.Name != "out/Default/gendir/hello.txt" {
			continue
		}
		gotHello = true
		if got := e.Entry.Data.Digest(); got != d1 {
			t.Errorf("gendir/hello.txt digest = %v, want %v", got, d1)
		}
	}
	if !gotHello {
		t.Errorf("entries missing out/Default/gendir/hello.txt; got %v", entries)
	}
	if len(additionalEntries) != 0 {
		t.Errorf("additionalEntries: got %d, want 0", len(additionalEntries))
	}
}

// TestEntriesFromResult_DirOutput_SubdirsHaveCmdHash verifies every dir entry of a directory output carries CmdHash, not just the root.
// CmdHash marks an entry as generated output and drives state-reload reconciliation; subdirs are as much a product of the step as the root.
func TestEntriesFromResult_DirOutput_SubdirsHaveCmdHash(t *testing.T) {
	ctx := t.Context()
	now := time.Now()

	d1 := digest.Digest{Hash: "hash1", SizeBytes: 5}
	rootTreeDg := digest.Digest{Hash: "tree1", SizeBytes: 10}
	subTreeDg := digest.Digest{Hash: "tree2", SizeBytes: 8}
	deepTreeDg := digest.Digest{Hash: "tree3", SizeBytes: 6}

	cmd := &Cmd{
		WorkDir: "out/Default",
		OutputDirs: []path.Path{
			"out/Default/gendir",
		},
		outfiles: map[path.Path]bool{
			"out/Default/gendir": true,
		},
		CmdHash: []byte("test-cmd-hash"),
		actionResult: &rpb.ActionResult{
			OutputFiles: []*rpb.OutputFile{
				{Path: "gendir/hello.txt", Digest: d1.Proto()},
				{Path: "gendir/sub/nested.txt", Digest: d1.Proto()},
			},
			// The root dir and every subdir from merkletree.Traverse.
			OutputDirectories: []*rpb.OutputDirectory{
				{Path: "gendir", TreeDigest: rootTreeDg.Proto()},
				{Path: "gendir/sub", TreeDigest: subTreeDg.Proto()},
				{Path: "gendir/sub/deep", TreeDigest: deepTreeDg.Proto()},
			},
		},
	}

	ds := mockDataSource{}
	entries, _ := cmd.entriesFromResult(ctx, ds, now)

	// Every dir entry (root and subdirs) must be present with CmdHash.
	want := map[string]bool{
		"out/Default/gendir":          false,
		"out/Default/gendir/sub":      false,
		"out/Default/gendir/sub/deep": false,
	}
	for i := range entries {
		if _, ok := want[string(entries[i].Name)]; !ok {
			continue
		}
		want[string(entries[i].Name)] = true
		if len(entries[i].CmdHash) == 0 {
			t.Errorf("%s CmdHash is empty; want non-empty", entries[i].Name)
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("missing dir entry %s", name)
		}
	}
}

// TestEntriesFromResult_DirOutput_SymlinkInside verifies a symlink inside a directory output (flattened into result.OutputSymlinks) is recorded as a CmdHash'd entry.
func TestEntriesFromResult_DirOutput_SymlinkInside(t *testing.T) {
	ctx := t.Context()
	now := time.Now()

	treeDg := digest.Digest{Hash: "tree1", SizeBytes: 10}

	cmd := &Cmd{
		WorkDir: "out/Default",
		OutputDirs: []path.Path{
			"out/Default/gendir",
		},
		outfiles: map[path.Path]bool{
			"out/Default/gendir": true,
		},
		CmdHash: []byte("test-cmd-hash"),
		actionResult: &rpb.ActionResult{
			OutputSymlinks: []*rpb.OutputSymlink{
				{Path: "gendir/link", Target: "hello.txt"},
			},
			OutputDirectories: []*rpb.OutputDirectory{
				{Path: "gendir", TreeDigest: treeDg.Proto()},
			},
		},
	}

	ds := mockDataSource{}
	entries, additionalEntries := cmd.entriesFromResult(ctx, ds, now)

	var linkEnt *hashfs.UpdateEntry
	for i := range entries {
		if entries[i].Name == "out/Default/gendir/link" {
			linkEnt = &entries[i]
			break
		}
	}
	if linkEnt == nil {
		t.Fatalf("missing symlink entry out/Default/gendir/link; got entries: %v", entries)
	}
	if len(linkEnt.CmdHash) == 0 {
		t.Errorf("symlink CmdHash is empty; want non-empty")
	}
	if linkEnt.Mode&fs.ModeSymlink == 0 {
		t.Errorf("symlink mode = %v; want ModeSymlink bit set", linkEnt.Mode)
	}
	if linkEnt.Entry.Target != "hello.txt" {
		t.Errorf("symlink target = %q; want %q", linkEnt.Entry.Target, "hello.txt")
	}
	if len(additionalEntries) != 0 {
		t.Errorf("additionalEntries: got %d, want 0", len(additionalEntries))
	}
}

func TestIsOutputFile(t *testing.T) {
	cmd := &Cmd{
		outfiles: map[path.Path]bool{
			"out/Default/main.o": true,
			"out/Default/gendir": true,
		},
	}
	for _, tc := range []struct {
		name string
		path string
		want bool
	}{
		{name: "exact_match", path: "out/Default/main.o", want: true},
		{name: "dir_match", path: "out/Default/gendir", want: true},
		{name: "file_under_dir", path: "out/Default/gendir/foo.txt", want: true},
		{name: "nested_under_dir", path: "out/Default/gendir/sub/bar.txt", want: true},
		{name: "deeply_nested_under_dir", path: "out/Default/gendir/a/b/c/d/e.txt", want: true},
		{name: "unrelated_file", path: "out/Default/other.o", want: false},
		{name: "partial_prefix", path: "out/Default/gendir_extra/foo.txt", want: false},
		{name: "absolute_path_no_match", path: "/usr/local/bin/tool", want: false},
		{name: "root_path", path: "/", want: false},
		{name: "empty_path", path: "", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := cmd.isOutputFile(path.Path(tc.path))
			if got != tc.want {
				t.Errorf("isOutputFile(%q) = %v; want %v", tc.path, got, tc.want)
			}
		})
	}
}

// TestIsOutputFile_ForwardSlashSemantics verifies isOutputFile uses path.Dir (always forward slash), not filepath.Dir.
// Regression guard: c.outfiles keys are forward-slash, so a filepath.Dir walk would fail to match parents on Windows (Linux can't prove this alone, since the two are identical there).
func TestIsOutputFile_ForwardSlashSemantics(t *testing.T) {
	cmd := &Cmd{
		outfiles: map[path.Path]bool{
			"out/Default/gendir": true,
		},
	}
	if !cmd.isOutputFile("out/Default/gendir/a/b/c.txt") {
		t.Error("isOutputFile should match a nested file under an output dir regardless of platform")
	}
}

type mockDataSource struct{}

func (mockDataSource) Source(ctx context.Context, d digest.Digest, name string) blob.Source {
	return nil
}

func (mockDataSource) Close(ctx context.Context) error { return nil }

// TestRecordPreOutputs verifies the pre-execution output snapshot for a racing
// remote racer (SkipRecordOutputs set, sharing hashFS with the local racer):
//
//   - Outputs that already exist on disk must be snapshotted, so that when the
//     remote side wins, runRacing's RecordOutputs can compare pre/post content
//     and honor restat_content. (My earlier fix skipped the snapshot entirely
//     and regressed this.)
//   - Outputs that do not exist yet must NOT be recorded as negative hashFS
//     entries: doing so races the local racer producing the same file and
//     poisons the cmdhash-less depfile, surfacing as a spurious
//     "failed to get depfile". (The original code regressed this.)
//
// This test fails for both wrong implementations and passes only for the
// existing-only snapshot.
func TestRecordPreOutputs(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "out"), 0o755); err != nil {
		t.Fatal(err)
	}
	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatalf("hashfs.New: %v", err)
	}
	defer hashFS.Close(ctx)

	// Three outputs the racing remote racer snapshots before execution:
	//   onDisk     - already materialized on local disk (a rebuild)
	//   hashfsOnly - left only in hashFS/CAS by a prior remote result, not on disk
	//   depfile    - absent from both disk and hashFS (a fresh action)
	onDisk := "out/ondisk.o"
	hashfsOnly := "out/remote.o"
	depfile := "out/foo.o.d"
	if err := os.WriteFile(filepath.Join(dir, onDisk), []byte("old object"), 0o644); err != nil {
		t.Fatal(err)
	}
	rd := digest.Digest{Hash: "remotehash", SizeBytes: 7}
	if err := hashFS.Update(ctx, dir, []hashfs.UpdateEntry{{
		Name:    path.Path(hashfsOnly),
		Entry:   &merkletree.Entry{Name: path.Path(hashfsOnly), Data: blob.NewData(nil, rd)},
		Mode:    0o644,
		CmdHash: []byte("cmdhash"),
	}}); err != nil {
		t.Fatalf("Update(%q): %v", hashfsOnly, err)
	}

	cmd := &Cmd{
		WorkspaceRoot:     dir,
		Outputs:           []path.Path{path.Path(onDisk), path.Path(hashfsOnly)},
		Depfile:           path.Path(depfile),
		HashFS:            hashFS,
		SkipRecordOutputs: true, // the racing remote racer
	}

	cmd.RecordPreOutputs(ctx)

	// (1) restat: both the on-disk output and the hashFS-only (cached, not
	// materialized locally) output must be in the snapshot (and preserved
	// across Clone), so a later RecordOutputs can compare pre/post content.
	// Dropping either dirties downstream steps under restat_content.
	for name, c := range map[string]*Cmd{"orig": cmd, "clone": cmd.Clone()} {
		got := map[string]bool{}
		for _, e := range c.preOutputEntries {
			got[string(e.Name)] = true
		}
		for _, want := range []string{onDisk, hashfsOnly} {
			if !got[want] {
				t.Errorf("%s: preOutputEntries missing %q; restat_content snapshot lost (have %v)", name, want, got)
			}
		}
	}

	// (2) no poison: the depfile is absent from both disk and hashFS, so it must
	// not be recorded as a negative hashFS entry. After the command writes it,
	// reading it back must succeed.
	if err := os.WriteFile(filepath.Join(dir, depfile), []byte("foo.o: foo.c\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := hashFS.ReadFile(ctx, dir, path.Path(depfile)); err != nil {
		t.Errorf("ReadFile(%q) after RecordPreOutputs = %v; want success (absent output must not be poisoned)", depfile, err)
	}
}

// TestEntriesFromResult_DirOutput_AuxiliaryInnerFile verifies a file inside a directory output is still recorded when its path is also an auxiliary log output.
// The auxiliary skip (IsAuxiliary && !outfiles[fname]) must not swallow inner files, which are never in c.outfiles (only the dir root is).
func TestEntriesFromResult_DirOutput_AuxiliaryInnerFile(t *testing.T) {
	ctx := t.Context()
	now := time.Now()

	d1 := digest.Digest{Hash: "hash1", SizeBytes: 5}
	cmd := &Cmd{
		WorkDir:                 "out/Default",
		OutputDirs:              []path.Path{"out/Default/gendir"},
		AuxiliaryLogOutputFiles: []path.Path{"out/Default/gendir/info.json"},
		outfiles: map[path.Path]bool{
			"out/Default/gendir": true,
		},
		CmdHash: []byte("test-cmd-hash"),
		actionResult: &rpb.ActionResult{
			OutputFiles: []*rpb.OutputFile{
				{Path: "gendir/info.json", Digest: d1.Proto()},
			},
		},
	}

	ds := mockDataSource{}
	entries, _ := cmd.entriesFromResult(ctx, ds, now)
	found := false
	for _, e := range entries {
		if e.Name == "out/Default/gendir/info.json" {
			found = true
		}
	}
	if !found {
		t.Errorf("dir-output inner file out/Default/gendir/info.json not recorded (dropped by auxiliary skip)")
	}
}

// TestRecordOutputsFromLocal_JailDepfile exercises the nsjail capture path with a depfile: each output (including the depfile) must be moved out of the jail exactly once.
// AllOutputs already includes the depfile, so a duplicate append would os.Rename it twice and the second rename fails ENOENT.
func TestRecordOutputsFromLocal_JailDepfile(t *testing.T) {
	ctx := t.Context()

	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	jail := filepath.Join(root, "jail")
	if err := os.MkdirAll(jail, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jail, "foo.o"), []byte("obj"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jail, "foo.o.d"), []byte("foo.o: foo.c\n"), 0644); err != nil {
		t.Fatal(err)
	}

	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}
	defer hashFS.Close(ctx)
	if err := hashFS.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}

	c := &Cmd{
		WorkspaceRoot: root,
		PostProc: func(ctx context.Context, outputs []path.Path) error {
			for _, out := range outputs {
				outInJail := filepath.Join(jail, string(out))
				outAbs := filepath.Join(root, string(out))
				err := os.Rename(outInJail, outAbs)
				if err != nil {
					return err
				}
			}
			return nil
		},
		Outputs: []path.Path{"foo.o"},
		Depfile: "foo.o.d",
		HashFS:  hashFS,
	}
	c.InitOutputs()

	if err := c.RecordOutputsFromLocal(ctx, time.Now()); err != nil {
		t.Fatalf("RecordOutputsFromLocal: %v (depfile renamed twice out of jail)", err)
	}
	for _, out := range []string{"foo.o", "foo.o.d"} {
		if _, err := os.Stat(filepath.Join(root, out)); err != nil {
			t.Errorf("output %q not captured from jail: %v", out, err)
		}
	}
}

// TestRecordOutputsFromLocal_JailDepfileAlsoOutput covers a depfile path that is also a declared output: AllOutputs lists it twice but the jail capture must rename it only once.
func TestRecordOutputsFromLocal_JailDepfileAlsoOutput(t *testing.T) {
	ctx := t.Context()

	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	jail := filepath.Join(root, "jail")
	if err := os.MkdirAll(jail, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jail, "foo.o"), []byte("obj"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jail, "foo.o.d"), []byte("foo.o: foo.c\n"), 0644); err != nil {
		t.Fatal(err)
	}

	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}
	defer hashFS.Close(ctx)
	if err := hashFS.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}

	// foo.o.d is both a declared output and the depfile.
	c := &Cmd{
		WorkspaceRoot: root,
		PostProc: func(ctx context.Context, outputs []path.Path) error {
			for _, out := range outputs {
				outInJail := filepath.Join(jail, string(out))
				outAbs := filepath.Join(root, string(out))
				err := os.Rename(outInJail, outAbs)
				if err != nil {
					return err
				}
			}
			return nil
		},
		Outputs: []path.Path{"foo.o", "foo.o.d"},
		Depfile: "foo.o.d",
		HashFS:  hashFS,
	}
	c.InitOutputs()

	if err := c.RecordOutputsFromLocal(ctx, time.Now()); err != nil {
		t.Fatalf("RecordOutputsFromLocal: %v (depfile-as-output renamed twice out of jail)", err)
	}
	for _, out := range []string{"foo.o", "foo.o.d"} {
		if _, err := os.Stat(filepath.Join(root, out)); err != nil {
			t.Errorf("output %q not captured from jail: %v", out, err)
		}
	}
}

// TestComputeOutputEntries_DirOnlyEdgeHash is a regression test: for a dir-only
// output (no file Outputs), the edge hash must land on the declared directory
// entry, not on entries[0] (a flattened child file). outputMtime reads the edge
// hash from the declared output, and needToRun silently skips edge-change
// detection when it is empty, so a misplaced edge hash misses rebuilds.
func TestComputeOutputEntries_DirOnlyEdgeHash(t *testing.T) {
	c := &Cmd{
		WorkDir:    "out/Default",
		OutputDirs: []path.Path{"out/Default/gen"},
		EdgeHash:   []byte("edge-hash"),
		CmdHash:    []byte("cmd-hash"),
	}
	// entriesFromResult emits flattened child files before the directory node.
	entries := []hashfs.UpdateEntry{
		{Name: "out/Default/gen/inner.txt", Entry: &merkletree.Entry{Name: "out/Default/gen/inner.txt"}, Mode: 0644, IsChanged: true},
		{Name: "out/Default/gen", Entry: &merkletree.Entry{Name: "out/Default/gen"}, Mode: fs.ModeDir | 0755, IsChanged: true},
	}
	got := c.computeOutputEntries(entries, time.Unix(2000, 0), c.CmdHash)
	byName := make(map[string]hashfs.UpdateEntry, len(got))
	for _, e := range got {
		byName[string(e.Name)] = e
	}
	if string(byName["out/Default/gen"].EdgeHash) != string(c.EdgeHash) {
		t.Errorf("dir output entry EdgeHash=%q; want %q (edge hash must be on the declared dir output; otherwise outputMtime reads empty and edge changes are missed)", byName["out/Default/gen"].EdgeHash, c.EdgeHash)
	}
}

func TestComputeOutputEntries_RestatContentEmptyFile(t *testing.T) {
	emptyData := blob.FromBytes(digest.SHA256, "empty", nil)
	preTime := time.Unix(1000, 0)
	postTime := time.Unix(2000, 0)

	c := &Cmd{
		WorkDir:       "out/Default",
		Outputs:       []path.Path{"out/Default/foo.stamp"},
		RestatContent: true,
		CmdHash:       []byte("cmd-hash"),
		EdgeHash:      []byte("edge-hash"),
		preOutputEntries: []hashfs.UpdateEntry{
			{
				Name:    "out/Default/foo.stamp",
				Entry:   &merkletree.Entry{Name: "out/Default/foo.stamp", Data: emptyData},
				ModTime: preTime,
			},
		},
	}
	entries := []hashfs.UpdateEntry{
		{
			Name:      "out/Default/foo.stamp",
			Entry:     &merkletree.Entry{Name: "out/Default/foo.stamp", Data: emptyData},
			ModTime:   postTime,
			IsChanged: true,
		},
	}
	got := c.computeOutputEntries(entries, postTime, c.CmdHash)
	if len(got) != 1 {
		t.Fatalf("len(got)=%d; want 1", len(got))
	}
	if got[0].IsChanged {
		t.Errorf("got[0].IsChanged=%v; want false", got[0].IsChanged)
	}
	if !got[0].ModTime.Equal(preTime) {
		t.Errorf("got[0].ModTime=%v; want %v", got[0].ModTime, preTime)
	}
	if !got[0].UpdatedTime.Equal(postTime) {
		t.Errorf("got[0].UpdatedTime=%v; want %v", got[0].UpdatedTime, postTime)
	}
}

// singleFileDirTree registers a REAPI tree blob (and its content blob) for a
// directory containing exactly one file, and returns the tree digest.
func singleFileDirTree(t *testing.T, ds *blob.Store, member, content string) digest.Digest {
	t.Helper()
	cd := blob.FromBytes(digest.SHA256, member, []byte(content))
	ds.Set(cd)
	tree := &rpb.Tree{Root: &rpb.Directory{Files: []*rpb.FileNode{{Name: member, Digest: cd.Digest().Proto()}}}}
	td, err := blob.FromProtoMessage(digest.SHA256, tree)
	if err != nil {
		t.Fatal(err)
	}
	ds.Set(td)
	return td.Digest()
}

// TestRecordOutputs_DirTreeReplacesStaleMembers is a regression test: when a
// remote/cache directory output is re-recorded with a different tree, members
// from the previous tree must not linger. Recording flattens and upserts the
// new tree but, without an explicit prune, HashFS.Update merges it into the old
// one (the local path avoids this by reading disk truth), so a consumer could
// still see the removed member.
func TestRecordOutputs_DirTreeReplacesStaleMembers(t *testing.T) {
	ctx := t.Context()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hashFS.Close(ctx) })
	if err := hashFS.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}

	ds := blob.NewStore()
	td1 := singleFileDirTree(t, ds, "old.txt", "OLD")
	td2 := singleFileDirTree(t, ds, "new.txt", "NEW")

	c := &Cmd{
		WorkspaceRoot: root,
		HashFS:        hashFS,
		OutputDirs:    []path.Path{"gen"},
		CmdHash:       []byte("cmd"),
		EdgeHash:      []byte("edge"),
	}
	c.InitOutputs()
	result := func(td digest.Digest) *rpb.ActionResult {
		return &rpb.ActionResult{OutputDirectories: []*rpb.OutputDirectory{{Path: "gen", TreeDigest: td.Proto()}}}
	}
	dsrc := storeDataSource{s: ds}

	c.SetActionResult(result(td1), false)
	if err := c.RecordOutputs(ctx, dsrc, time.Now()); err != nil {
		t.Fatalf("RecordOutputs #1: %v", err)
	}
	if _, err := hashFS.Stat(ctx, root, "gen/old.txt"); err != nil {
		t.Fatalf("after #1: gen/old.txt should be recorded: %v", err)
	}

	// Re-record the same dir output (same cmd/edge/action) with a different tree.
	c.SetActionResult(result(td2), false)
	if err := c.RecordOutputs(ctx, dsrc, time.Now()); err != nil {
		t.Fatalf("RecordOutputs #2: %v", err)
	}
	if _, err := hashFS.Stat(ctx, root, "gen/new.txt"); err != nil {
		t.Errorf("after #2: gen/new.txt missing: %v", err)
	}
	if _, err := hashFS.Stat(ctx, root, "gen/old.txt"); err == nil {
		t.Errorf("after #2: gen/old.txt still present; the new tree must replace the old, not merge")
	}
}

// TestInputTree_RejectsDirectoryRemoteInput verifies a directory (trailing-slash) RemoteInputs value is rejected.
// HashFS.Entries would expand it into files, so the reverse map keyed on the directory path no longer matches and the remap is silently dropped.
func TestInputTree_RejectsDirectoryRemoteInput(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}
	defer hashFS.Close(ctx)
	if err := hashFS.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}

	c := &Cmd{
		WorkspaceRoot: root,
		WorkDir:       "out/siso",
		HashFS:        hashFS,
		RemoteInputs: map[path.Path]path.Path{
			"remote/extracted/": "obj/extracted/",
		},
	}
	_, err = c.inputTree(ctx)
	if err == nil {
		t.Fatal("inputTree with a directory remote_inputs value = nil error; want rejection")
	}
	if !strings.Contains(err.Error(), "directory") {
		t.Errorf("inputTree error = %q; want it to mention directory", err)
	}
}

// TestDeclaredOutputsAndAllOutputs verifies DeclaredOutputs returns file and directory outputs without the depfile, and AllOutputs adds the depfile.
func TestDeclaredOutputsAndAllOutputs(t *testing.T) {
	for _, tc := range []struct {
		name         string
		cmd          Cmd
		wantDeclared []string
		wantAll      []string
	}{
		{
			name:         "files_only",
			cmd:          Cmd{Outputs: []path.Path{"a.o", "b.o"}},
			wantDeclared: []string{"a.o", "b.o"},
			wantAll:      []string{"a.o", "b.o"},
		},
		{
			name:         "files_and_dirs",
			cmd:          Cmd{Outputs: []path.Path{"a.o"}, OutputDirs: []path.Path{"gen"}},
			wantDeclared: []string{"a.o", "gen"},
			wantAll:      []string{"a.o", "gen"},
		},
		{
			name:         "files_and_depfile",
			cmd:          Cmd{Outputs: []path.Path{"a.o"}, Depfile: "a.o.d"},
			wantDeclared: []string{"a.o"},
			wantAll:      []string{"a.o", "a.o.d"},
		},
		{
			name:         "files_dirs_and_depfile",
			cmd:          Cmd{Outputs: []path.Path{"a.o"}, OutputDirs: []path.Path{"gen"}, Depfile: "a.o.d"},
			wantDeclared: []string{"a.o", "gen"},
			wantAll:      []string{"a.o", "gen", "a.o.d"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if diff := cmp.Diff(tc.wantDeclared, path.Strings(tc.cmd.DeclaredOutputs())); diff != "" {
				t.Errorf("DeclaredOutputs (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.wantAll, path.Strings(tc.cmd.AllOutputs())); diff != "" {
				t.Errorf("AllOutputs (-want +got):\n%s", diff)
			}
		})
	}
}

// TestAllOutputsNoAlias verifies AllOutputs does not write the depfile into Outputs' backing array (which can have spare capacity from stepFileOutputs).
func TestAllOutputsNoAlias(t *testing.T) {
	outputs := make([]path.Path, 1, 4) // len 1, cap 4: spare capacity
	outputs[0] = "a.o"
	c := Cmd{Outputs: outputs, Depfile: "a.o.d"}
	_ = c.AllOutputs()
	// The depfile must not have leaked into Outputs' backing array.
	full := c.Outputs[:cap(c.Outputs)]
	for _, s := range full[len(c.Outputs):] {
		if s == "a.o.d" {
			t.Errorf("AllOutputs wrote depfile into Outputs' backing array: %v", full)
		}
	}
}

func TestOutermostPaths(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []string
		want []string
	}{
		{"file_nested_in_dir_output", []string{"gen/foo.h", "gen"}, []string{"gen"}},
		{"nested_dir_outputs", []string{"gen", "gen/sub"}, []string{"gen"}},
		{"siblings_no_parent_output", []string{"gen/a.o", "gen/b.o"}, []string{"gen/a.o", "gen/b.o"}},
		{"exact_duplicate_depfile_also_output", []string{"foo.o", "foo.o.d", "foo.o.d"}, []string{"foo.o", "foo.o.d"}},
		{"unrelated", []string{"gen", "other.o"}, []string{"gen", "other.o"}},
		// Sorted order can interleave a sibling between a directory root and
		// its contents ('.' < '/'): "gen/foo.stamp" sorts between "gen/foo"
		// and "gen/foo/a.o". The sibling must stay; the nested file must go.
		{"interleaved_sibling", []string{"gen/foo", "gen/foo.stamp", "gen/foo/a.o"}, []string{"gen/foo", "gen/foo.stamp"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := OutermostPaths(path.Paths(tc.in))
			if diff := cmp.Diff(tc.want, path.Strings(got), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("OutermostPaths(%v) mismatch (-want +got):\n%s", tc.in, diff)
			}
		})
	}
}

// TestRecordOutputsFromLocal_JailNestedDirOutput covers a directory output with a file output nested inside it: the jail capture must rename only the outermost output (the directory).
// Renaming the nested file separately and then the directory would let the directory's RemoveAll of its pre-created destination clobber the just-moved nested file.
func TestRecordOutputsFromLocal_JailNestedDirOutput(t *testing.T) {
	ctx := t.Context()

	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	jail := filepath.Join(root, "jail")
	// The action produced gen/ with a nested file under it.
	if err := os.MkdirAll(filepath.Join(jail, "gen"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jail, "gen", "foo.h"), []byte("H"), 0644); err != nil {
		t.Fatal(err)
	}
	// Mimic ensureActionOutputDirs pre-creating the dir output's destination.
	if err := os.MkdirAll(filepath.Join(root, "gen"), 0755); err != nil {
		t.Fatal(err)
	}

	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}
	defer hashFS.Close(ctx)
	if err := hashFS.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}

	c := &Cmd{
		WorkspaceRoot: root,
		PostProc: func(ctx context.Context, outputs []path.Path) error {
			for _, out := range outputs {
				outInJail := filepath.Join(jail, string(out))
				outAbs := filepath.Join(root, string(out))
				if fi, err := os.Lstat(outInJail); err == nil && fi.IsDir() {
					if err := os.RemoveAll(outAbs); err != nil {
						return err
					}
				}
				err := os.Rename(outInJail, outAbs)
				if err != nil {
					return err
				}
			}
			return nil
		},
		Outputs:    []path.Path{"gen/foo.h"},
		OutputDirs: []path.Path{"gen"},
		HashFS:     hashFS,
	}
	c.InitOutputs()

	if err := c.RecordOutputsFromLocal(ctx, time.Now()); err != nil {
		t.Fatalf("RecordOutputsFromLocal: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "gen", "foo.h")); err != nil {
		t.Errorf("nested output gen/foo.h lost during jail capture: %v (the directory output's RemoveAll clobbered it)", err)
	}
}

func TestNormalizeEnvVars(t *testing.T) {
	for _, tc := range []struct {
		name          string
		substitutions map[string]string
		trimPrefixes  []string
		env           []string
		excludes      map[string]bool
		want          map[string]string
	}{
		{
			name: "replace_cwd_and_strip_prefix",
			substitutions: map[string]string{
				"/home/user/project": "CWD",
			},
			trimPrefixes: []string{"CWD/"},
			env: []string{
				"DIST_DIR=/home/user/project/out/dist",
				"OTHER_DIR=/other/path",
			},
			want: map[string]string{
				"DIST_DIR":  "out/dist",
				"OTHER_DIR": "/other/path",
			},
		},
		{
			name: "replace_cwd_without_trim_prefixes",
			substitutions: map[string]string{
				"/home/user/project": "CWD",
			},
			env: []string{
				"DIST_DIR=/home/user/project/out/dist",
			},
			want: map[string]string{
				"DIST_DIR": "CWD/out/dist",
			},
		},
		{
			name: "relative_path",
			substitutions: map[string]string{
				"/home/user/project": "CWD",
			},
			trimPrefixes: []string{"CWD/"},
			env: []string{
				"DIST_DIR=out/dist",
			},
			want: map[string]string{
				"DIST_DIR": "out/dist",
			},
		},
		{
			name: "excludes",
			substitutions: map[string]string{
				"/home/user/project": "CWD",
			},
			trimPrefixes: []string{"CWD/"},
			env: []string{
				"HOME=/home/user",
				"USER=user",
			},
			excludes: map[string]bool{
				"HOME": true,
				"USER": true,
			},
			want: map[string]string{},
		},
		{
			name: "path_list",
			substitutions: map[string]string{
				"/home/user/project": "CWD",
			},
			trimPrefixes: []string{"CWD/"},
			env: []string{
				"PATH=/home/user/project/bin" + string(os.PathListSeparator) + "/usr/bin" + string(os.PathListSeparator) + "/home/user/project/out/.path" + string(os.PathListSeparator) + "rel/path",
			},
			want: map[string]string{
				"PATH": "CWD/bin" + string(os.PathListSeparator) + "/usr/bin" + string(os.PathListSeparator) + "CWD/out/.path" + string(os.PathListSeparator) + "rel/path",
			},
		},
		{
			name: "cwd_prefix_without_slash",
			substitutions: map[string]string{
				"/home/user/project": "CWD",
			},
			trimPrefixes: []string{"CWD/"},
			env: []string{
				"SOME_VAR=/home/user/project_suffix",
			},
			want: map[string]string{
				"SOME_VAR": "CWD_suffix",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeEnvVars(tc.env, tc.substitutions, tc.trimPrefixes, func(key string) bool {
				return !tc.excludes[key]
			})
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("NormalizeEnvVars() diff -want +got:\n%s", diff)
			}
		})
	}
}

func TestEnvVars(t *testing.T) {
	c := &Cmd{
		WorkspaceRoot:    "/home/user/project",
		Env:              []string{"Z=last", "A=1", "MALFORMED", "A=2", "M=mid=val", "HOME=/home/user", "DIST_DIR=/home/user/project/out/dist"},
		EnvCacheKeyOmits: map[string]bool{"HOME": true},
		EnvCacheKeySubstitutions: map[string]string{
			"/home/user/project": "CWD",
		},
		EnvCacheKeyTrimPrefixes: []string{"CWD/"},
	}
	got := c.EnvVars()
	want := []*rpb.Command_EnvironmentVariable{
		{Name: "A", Value: "2"},
		{Name: "DIST_DIR", Value: "out/dist"},
		{Name: "M", Value: "mid=val"},
		{Name: "Z", Value: "last"},
	}
	if diff := cmp.Diff(want, got, cmpopts.IgnoreUnexported(rpb.Command_EnvironmentVariable{})); diff != "" {
		t.Errorf("EnvVars() diff -want +got:\n%s", diff)
	}
}

func TestCommandDigest_DotWorkDir(t *testing.T) {
	ctx := t.Context()
	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}
	defer hashFS.Close(ctx)

	// WorkDir "." is what a build rooted at the workspace root produces.
	// Two phase caching disables CanonicalizeDir, and CommandDigest must
	// still emit working_directory "" so the digest matches non-2PC builds.
	cmdDotCanon := &Cmd{
		Args:            []string{"cc", "-c", "foo.c"},
		WorkDir:         ".",
		CanonicalizeDir: true,
		HashFS:          hashFS,
	}
	cmdDotNoCanon := &Cmd{
		Args:            []string{"cc", "-c", "foo.c"},
		WorkDir:         ".",
		CanonicalizeDir: false,
		HashFS:          hashFS,
	}
	cmdEmptyNoCanon := &Cmd{
		Args:            []string{"cc", "-c", "foo.c"},
		WorkDir:         "",
		CanonicalizeDir: false,
		HashFS:          hashFS,
	}

	ds := blob.NewStore()
	d1, err := cmdDotCanon.CommandDigest(ctx, ds)
	if err != nil {
		t.Fatal(err)
	}
	d2, err := cmdDotNoCanon.CommandDigest(ctx, ds)
	if err != nil {
		t.Fatal(err)
	}
	d3, err := cmdEmptyNoCanon.CommandDigest(ctx, ds)
	if err != nil {
		t.Fatal(err)
	}

	if d1 != d2 {
		t.Errorf("CommandDigest mismatch for dot WorkDir: CanonicalizeDir=true got %s, CanonicalizeDir=false got %s", d1, d2)
	}
	if d1 != d3 {
		t.Errorf("CommandDigest mismatch: WorkDir=\".\" got %s, WorkDir=\"\" got %s", d1, d3)
	}

	for _, tc := range []struct {
		name   string
		digest digest.Digest
	}{
		{"dot-canon", d1},
		{"dot-nocanon", d2},
		{"empty-nocanon", d3},
	} {
		data, ok := ds.Get(tc.digest)
		if !ok {
			t.Fatalf("%s: digest not found in blob store: %s", tc.name, tc.digest)
		}
		b, err := blob.DataToBytes(ctx, data)
		if err != nil {
			t.Fatalf("%s: failed to read blob: %v", tc.name, err)
		}
		cmd := &rpb.Command{}
		if err := proto.Unmarshal(b, cmd); err != nil {
			t.Fatalf("%s: failed to unmarshal command: %v", tc.name, err)
		}
		if cmd.WorkingDirectory != "" {
			t.Errorf("%s: WorkingDirectory = %q; want %q", tc.name, cmd.WorkingDirectory, "")
		}
	}
}

func TestCommandDigest_NonRootWorkDir(t *testing.T) {
	ctx := t.Context()
	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}
	defer hashFS.Close(ctx)

	cmdCanon := &Cmd{
		Args:            []string{"cc", "-c", "foo.c"},
		WorkDir:         "out/Default",
		CanonicalizeDir: true,
		HashFS:          hashFS,
	}
	cmdNoCanon := &Cmd{
		Args:            []string{"cc", "-c", "foo.c"},
		WorkDir:         "out/Default",
		CanonicalizeDir: false,
		HashFS:          hashFS,
	}

	ds := blob.NewStore()
	dCanon, err := cmdCanon.CommandDigest(ctx, ds)
	if err != nil {
		t.Fatal(err)
	}
	dNoCanon, err := cmdNoCanon.CommandDigest(ctx, ds)
	if err != nil {
		t.Fatal(err)
	}
	if dCanon == dNoCanon {
		t.Errorf("expected different digests for out/Default with CanonicalizeDir true vs false")
	}

	dataCanon, ok := ds.Get(dCanon)
	if !ok {
		t.Fatalf("dCanon not found in blob store")
	}
	bCanon, err := blob.DataToBytes(ctx, dataCanon)
	if err != nil {
		t.Fatal(err)
	}
	cCanon := &rpb.Command{}
	if err := proto.Unmarshal(bCanon, cCanon); err != nil {
		t.Fatal(err)
	}
	if cCanon.WorkingDirectory != "out/x" {
		t.Errorf("CanonicalizeDir=true WorkingDirectory = %q; want %q", cCanon.WorkingDirectory, "out/x")
	}

	dataNoCanon, ok := ds.Get(dNoCanon)
	if !ok {
		t.Fatalf("dNoCanon not found in blob store")
	}
	bNoCanon, err := blob.DataToBytes(ctx, dataNoCanon)
	if err != nil {
		t.Fatal(err)
	}
	cNoCanon := &rpb.Command{}
	if err := proto.Unmarshal(bNoCanon, cNoCanon); err != nil {
		t.Fatal(err)
	}
	if cNoCanon.WorkingDirectory != "out/Default" {
		t.Errorf("CanonicalizeDir=false WorkingDirectory = %q; want %q", cNoCanon.WorkingDirectory, "out/Default")
	}
}

func TestCommandDigest_RemoteChrootDotWorkDir(t *testing.T) {
	ctx := t.Context()
	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}
	defer hashFS.Close(ctx)

	cmdChrootDot := &Cmd{
		Args:            []string{"cc", "-c", "foo.c"},
		WorkspaceRoot:   "/workspace",
		WorkDir:         ".",
		CanonicalizeDir: false,
		HashFS:          hashFS,
		Platform:        map[string]string{"dockerChrootPath": "."},
	}
	cmdChrootEmpty := &Cmd{
		Args:            []string{"cc", "-c", "foo.c"},
		WorkspaceRoot:   "/workspace",
		WorkDir:         "",
		CanonicalizeDir: true,
		HashFS:          hashFS,
		Platform:        map[string]string{"dockerChrootPath": "."},
	}

	ds := blob.NewStore()
	dDot, err := cmdChrootDot.CommandDigest(ctx, ds)
	if err != nil {
		t.Fatal(err)
	}
	dEmpty, err := cmdChrootEmpty.CommandDigest(ctx, ds)
	if err != nil {
		t.Fatal(err)
	}
	if dDot != dEmpty {
		t.Errorf("chroot digest mismatch: %s != %s", dDot, dEmpty)
	}

	dataDot, ok := ds.Get(dDot)
	if !ok {
		t.Fatalf("dDot not found in blob store")
	}
	b, err := blob.DataToBytes(ctx, dataDot)
	if err != nil {
		t.Fatal(err)
	}
	cmd := &rpb.Command{}
	if err := proto.Unmarshal(b, cmd); err != nil {
		t.Fatal(err)
	}
	if cmd.WorkingDirectory != "workspace" {
		t.Errorf("chroot WorkingDirectory = %q; want %q", cmd.WorkingDirectory, "workspace")
	}
}

func TestCmdWorkingDirectory(t *testing.T) {
	for _, tc := range []struct {
		name            string
		workDir         path.Path
		canonicalizeDir bool
		workspaceRoot   string
		platform        map[string]string
		want            string
	}{
		{
			name:            "dot_canon_true",
			workDir:         ".",
			canonicalizeDir: true,
			want:            "",
		},
		{
			name:            "dot_canon_false",
			workDir:         ".",
			canonicalizeDir: false,
			want:            "",
		},
		{
			name:            "empty",
			workDir:         "",
			canonicalizeDir: false,
			want:            "",
		},
		{
			name:            "empty_canon_true",
			workDir:         "",
			canonicalizeDir: true,
			want:            "",
		},
		{
			name:            "out_default_canon_true",
			workDir:         "out/Default",
			canonicalizeDir: true,
			want:            "out/x",
		},
		{
			name:            "out_default_canon_false",
			workDir:         "out/Default",
			canonicalizeDir: false,
			want:            "out/Default",
		},
		{
			name:          "remote_chroot",
			workDir:       ".",
			workspaceRoot: "/workspace",
			platform:      map[string]string{"dockerChrootPath": "."},
			want:          "workspace",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &Cmd{
				WorkDir:         tc.workDir,
				CanonicalizeDir: tc.canonicalizeDir,
				WorkspaceRoot:   tc.workspaceRoot,
				Platform:        tc.platform,
			}
			got := cmd.WorkingDirectory()
			if got != tc.want {
				t.Errorf("cmd.WorkingDirectory() = %q, want %q", got, tc.want)
			}
		})
	}
}
