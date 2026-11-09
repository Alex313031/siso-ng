// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package execute

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"

	"go.chromium.org/build/hashigo/digest"
	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/blob"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/path"
	"go.chromium.org/build/siso/reapi"
	"go.chromium.org/build/siso/reapi/merkletree"
)

func newHashFS(t *testing.T) (*hashfs.HashFS, string) {
	t.Helper()
	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	hashFS, err := hashfs.New(t.Context(), hashfs.Option{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hashFS.Close(t.Context()) })
	if err := hashFS.WaitReady(t.Context()); err != nil {
		t.Fatal(err)
	}
	return hashFS, root
}

// traverseTree fetches and flattens a Tree blob exactly as the cache-hit path (expandDirOutputs) does, returning each file's path mapped to its content read back from the store (reading content, not just blob existence, catches empty-tree/wrong-digest regressions).
func traverseTree(t *testing.T, ds *blob.Store, td digest.Digest, base string) map[string]string {
	t.Helper()
	treeData, ok := ds.Get(td)
	if !ok {
		t.Fatalf("tree blob %s not registered in store", td)
	}
	b, err := blob.DataToBytes(t.Context(), treeData)
	if err != nil {
		t.Fatalf("read tree blob: %v", err)
	}
	parseStore := blob.NewStore()
	rootDir, err := reapi.ParseTree(t.Context(), digest.SHA256, b, parseStore)
	if err != nil {
		t.Fatalf("ParseTree: %v", err)
	}
	gotFiles, _, _ := merkletree.Traverse(t.Context(), digest.SHA256, base, rootDir, parseStore)
	contents := make(map[string]string, len(gotFiles))
	for _, f := range gotFiles {
		path := filepath.ToSlash(f.GetPath())
		cData, ok := ds.Get(digest.FromProto(f.GetDigest()))
		if !ok {
			t.Errorf("content blob for %s missing from store (would upload an unfetchable tree)", path)
			continue
		}
		data, err := blob.DataToBytes(t.Context(), cData)
		if err != nil {
			t.Errorf("read content blob for %s: %v", path, err)
			continue
		}
		contents[path] = string(data)
	}
	return contents
}

// TestDirOutputTree_RoundTrip verifies a directory output's tree round-trips through the cache-hit parse+traverse, recovering every file with its exact content (a bare empty-tree node would recover nothing).
func TestDirOutputTree_RoundTrip(t *testing.T) {
	ctx := t.Context()
	hashFS, root := newHashFS(t)
	for rel, content := range map[string]string{
		"out/gen/a.txt":     "A",
		"out/gen/sub/b.txt": "BB",
		"out/gen/sub/c.txt": "CCC",
	} {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	ds := blob.NewStore()
	td, err := dirOutputTree(ctx, hashFS, root, "out/gen", ds)
	if err != nil {
		t.Fatalf("dirOutputTree: %v", err)
	}
	if td.SizeBytes == 0 {
		t.Fatalf("tree digest is empty: %v (bug: directory cached as empty tree)", td)
	}

	got := traverseTree(t, ds, td, "out/gen")
	want := map[string]string{
		"out/gen/a.txt":     "A",
		"out/gen/sub/b.txt": "BB",
		"out/gen/sub/c.txt": "CCC",
	}
	if !maps.Equal(got, want) {
		t.Errorf("traversed files = %v; want %v", got, want)
	}
}

// traverseTreeDirs is like traverseTree but returns the tree's directory paths (including empty subdirectories).
func traverseTreeDirs(t *testing.T, ds *blob.Store, td digest.Digest, base string) []string {
	t.Helper()
	treeData, ok := ds.Get(td)
	if !ok {
		t.Fatalf("tree blob %s not registered in store", td)
	}
	b, err := blob.DataToBytes(t.Context(), treeData)
	if err != nil {
		t.Fatalf("read tree blob: %v", err)
	}
	parseStore := blob.NewStore()
	rootDir, err := reapi.ParseTree(t.Context(), digest.SHA256, b, parseStore)
	if err != nil {
		t.Fatalf("ParseTree: %v", err)
	}
	_, _, gotDirs := merkletree.Traverse(t.Context(), digest.SHA256, base, rootDir, parseStore)
	var paths []string
	for _, d := range gotDirs {
		paths = append(paths, filepath.ToSlash(d.GetPath()))
	}
	sort.Strings(paths)
	return paths
}

// TestDirOutputTree_EmptySubdir verifies an empty subdirectory inside a directory output survives into the uploaded tree.
// hashFS.Entries flattens to files only, so without an explicit directory pass a later cache hit materializes the directory output missing the empty subdir.
func TestDirOutputTree_EmptySubdir(t *testing.T) {
	ctx := t.Context()
	hashFS, root := newHashFS(t)
	if err := os.MkdirAll(filepath.Join(root, "out/gen/emptysub"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "out/gen/inner.txt"), []byte("X"), 0644); err != nil {
		t.Fatal(err)
	}

	ds := blob.NewStore()
	td, err := dirOutputTree(ctx, hashFS, root, "out/gen", ds)
	if err != nil {
		t.Fatalf("dirOutputTree: %v", err)
	}

	gotDirs := traverseTreeDirs(t, ds, td, "out/gen")
	if !slices.Contains(gotDirs, "out/gen/emptysub") {
		t.Errorf("empty subdir out/gen/emptysub not in tree dirs %v; a cache hit would materialize the directory output without it", gotDirs)
	}
}

// TestDirOutputTree_Deterministic verifies a directory output's tree digest is stable across rebuilds of identical content.
// Building rpb.Tree.Children from a Go map gives randomized iteration order, so the digest (the action cache key) differs run to run and defeats cache/CAS dedup.
func TestDirOutputTree_Deterministic(t *testing.T) {
	ctx := t.Context()
	hashFS, root := newHashFS(t)
	// Sibling subdirectories with distinct content, so the tree has multiple
	// children whose map-iteration order would otherwise vary.
	for i, name := range []string{"a", "b", "c", "d", "e"} {
		full := filepath.Join(root, "out/gen", name, "f.txt")
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(strings.Repeat(name, i+1)), 0644); err != nil {
			t.Fatal(err)
		}
	}
	var first digest.Digest
	for i := range 20 {
		ds := blob.NewStore()
		td, err := dirOutputTree(ctx, hashFS, root, "out/gen", ds)
		if err != nil {
			t.Fatalf("dirOutputTree: %v", err)
		}
		if i == 0 {
			first = td
			continue
		}
		if td != first {
			t.Fatalf("dirOutputTree digest not deterministic: run %d = %v, run 0 = %v (tree children built from a Go map -> nondeterministic cache key)", i, td, first)
		}
	}
}

// storeDataSource adapts a blob.Store to hashfs.DataSource so a test can drive expandDirOutputs.
type storeDataSource struct{ s *blob.Store }

func (d storeDataSource) Source(_ context.Context, dg digest.Digest, _ string) blob.Source {
	src, _ := d.s.GetSource(dg)
	return src
}

// TestExpandDirOutputs_Idempotent verifies expandDirOutputs does not double-record a directory output's files when called twice on the same Cmd.
// It appends flattened files to actionResult in place and the directory nodes remain, so without a guard a second call re-fetches the tree and appends again.
func TestExpandDirOutputs_Idempotent(t *testing.T) {
	ctx := t.Context()
	hashFS, root := newHashFS(t)
	for rel, content := range map[string]string{
		"out/gen/a.txt":     "A",
		"out/gen/sub/b.txt": "BB",
	} {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	ds := blob.NewStore()
	td, err := dirOutputTree(ctx, hashFS, root, "out/gen", ds)
	if err != nil {
		t.Fatalf("dirOutputTree: %v", err)
	}

	cmd := &Cmd{
		WorkspaceRoot: root,
		WorkDir:       "out",
		OutputDirs:    []path.Path{"out/gen"},
		HashFS:        hashFS,
	}
	cmd.InitOutputs()
	cmd.actionResult = &rpb.ActionResult{
		OutputDirectories: []*rpb.OutputDirectory{
			{Path: "gen", TreeDigest: td.Proto()},
		},
	}

	outputPaths := func() []string {
		var ps []string
		for _, f := range cmd.actionResult.GetOutputFiles() {
			ps = append(ps, f.GetPath())
		}
		sort.Strings(ps)
		return ps
	}

	dsrc := storeDataSource{s: ds}
	if err := cmd.expandDirOutputs(ctx, dsrc); err != nil {
		t.Fatalf("expandDirOutputs (1st): %v", err)
	}
	first := outputPaths()
	if len(first) == 0 {
		t.Fatalf("expandDirOutputs recorded no files; expected the directory's contents")
	}
	if err := cmd.expandDirOutputs(ctx, dsrc); err != nil {
		t.Fatalf("expandDirOutputs (2nd): %v", err)
	}
	// Compare the exact path set, not just the count: a drop-and-replace with a
	// different same-size set would pass a count-only check.
	if second := outputPaths(); !slices.Equal(first, second) {
		t.Errorf("expandDirOutputs not idempotent: files after 1st call=%v, after 2nd=%v", first, second)
	}
}

// TestDirOutputTree_Empty verifies an empty directory output produces the canonical empty tree, so it round-trips rather than poisoning the cache.
func TestDirOutputTree_Empty(t *testing.T) {
	ctx := t.Context()
	hashFS, root := newHashFS(t)
	if err := os.MkdirAll(filepath.Join(root, "out/empty"), 0755); err != nil {
		t.Fatal(err)
	}

	ds := blob.NewStore()
	td, err := dirOutputTree(ctx, hashFS, root, "out/empty", ds)
	if err != nil {
		t.Fatalf("dirOutputTree(empty): %v", err)
	}
	if td != digest.SHA256.EmptyTree() {
		t.Errorf("empty dir tree digest = %v; want canonical EmptyTree %v", td, digest.SHA256.EmptyTree())
	}
}

// TestSetResultOutputs_FileAndDir verifies SetResultOutputs records a file output as an OutputFile and a directory output as an OutputDirectory whose tree carries the real contents, driven by the typed Outputs / OutputDirs fields.
func TestSetResultOutputs_FileAndDir(t *testing.T) {
	ctx := t.Context()
	hashFS, root := newHashFS(t)
	for rel, content := range map[string]string{
		"out/a.o":           "obj",
		"out/gen/x.txt":     "X",
		"out/gen/sub/y.txt": "YY",
	} {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	cmd := &Cmd{
		WorkspaceRoot: root,
		WorkDir:       "out",
		HashFS:        hashFS,
		Outputs:       []path.Path{"out/a.o"},
		OutputDirs:    []path.Path{"out/gen"},
	}
	ds := blob.NewStore()
	result := &rpb.ActionResult{}
	if err := cmd.SetResultOutputs(ctx, result, ds); err != nil {
		t.Fatalf("SetResultOutputs: %v", err)
	}

	// File output recorded as an OutputFile carrying obj's digest, never as a
	// tree.
	if len(result.OutputFiles) != 1 || result.OutputFiles[0].GetPath() != "a.o" {
		t.Fatalf("OutputFiles = %v; want one file %q", result.OutputFiles, "a.o")
	}
	wantObj := blob.FromBytes(digest.SHA256, "a.o", []byte("obj")).Digest()
	if got := digest.FromProto(result.OutputFiles[0].GetDigest()); got != wantObj {
		t.Errorf("a.o digest = %v; want %v (digest of %q)", got, wantObj, "obj")
	}
	// Directory output recorded once, with a real (non-empty) tree.
	if len(result.OutputDirectories) != 1 {
		t.Fatalf("OutputDirectories = %v; want exactly one (gen)", result.OutputDirectories)
	}
	od := result.OutputDirectories[0]
	if od.GetPath() != "gen" {
		t.Errorf("OutputDirectory path = %q; want %q", od.GetPath(), "gen")
	}
	td := digest.FromProto(od.GetTreeDigest())
	if td == digest.SHA256.EmptyTree() || td.SizeBytes == 0 {
		t.Fatalf("OutputDirectory tree = %v; want a real non-empty tree (empty tree = cache poisoning)", td)
	}
	got := traverseTree(t, ds, td, "gen")
	want := map[string]string{
		"gen/x.txt":     "X",
		"gen/sub/y.txt": "YY",
	}
	if !maps.Equal(got, want) {
		t.Errorf("dir tree files = %v; want %v", got, want)
	}
}

// TestSetResultOutputs_SymlinkOutput is a regression test for a declared
// symlink output: HashFS.Entries returns it with a Target and a zero-digest
// Data (no content blob). Seeding that into the upload store leaves a
// zero/invalid-digest entry that the subsequent UploadAll rejects (cache write
// fails or panics), so it must be skipped. The symlink is still recorded as an
// OutputSymlink.
func TestSetResultOutputs_SymlinkOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink not available on windows")
	}
	ctx := t.Context()
	hashFS, root := newHashFS(t)
	if err := os.MkdirAll(filepath.Join(root, "out"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target.txt", filepath.Join(root, "out/link")); err != nil {
		t.Fatal(err)
	}

	cmd := &Cmd{
		WorkspaceRoot: root,
		WorkDir:       "out",
		HashFS:        hashFS,
		Outputs:       []path.Path{"out/link"},
	}
	ds := blob.NewStore()
	result := &rpb.ActionResult{}
	if err := cmd.SetResultOutputs(ctx, result, ds); err != nil {
		t.Fatalf("SetResultOutputs: %v", err)
	}

	// The upload store must not carry a zero/invalid-digest entry (the symlink
	// has no content blob); UploadAll would fail or panic on it.
	if _, ok := ds.Get(digest.Digest{}); ok {
		t.Error("upload store seeded with a zero-digest entry for a symlink output; UploadAll would reject it")
	}
	// Recorded as a symlink, never as a file output.
	if len(result.OutputSymlinks) != 1 || result.OutputSymlinks[0].GetPath() != "link" {
		t.Errorf("OutputSymlinks = %v; want one symlink %q", result.OutputSymlinks, "link")
	}
	if len(result.OutputFiles) != 0 {
		t.Errorf("OutputFiles = %v; want none (a symlink is not a file output)", result.OutputFiles)
	}
}
