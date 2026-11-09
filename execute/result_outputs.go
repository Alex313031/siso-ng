// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package execute

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	"google.golang.org/protobuf/proto"

	"go.chromium.org/build/hashigo/digest"
	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/blob"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/path"
	"go.chromium.org/build/siso/reapi/merkletree"
)

// SetResultOutputs populates result's output files, symlinks, and directories from the command's typed outputs, recording every blob in ds.
//
// It consumes the typed Outputs/OutputDirs fields rather than flattened
// AllOutputs, so a directory output is always built into a real Tree and
// never collapses to an empty tree (a flattened list strips its trailing
// slash, after which the recording layer caches a bare empty-tree node).
func (c *Cmd) SetResultOutputs(ctx context.Context, result *rpb.ActionResult, ds *blob.Store) error {
	fileEntries, err := c.HashFS.RawEntries(ctx, c.WorkspaceRoot, c.FileOutputsWithDepfile())
	if err != nil {
		return err
	}
	ResultFromEntries(ctx, c.HashFS.DigestFunction(), result, string(c.WorkDir), fileEntries)
	for _, entry := range fileEntries {
		// A symlink (or directory) output has no content blob: its Data is the
		// zero value. Skip it so a zero/invalid-digest entry is not seeded into
		// the upload store, which UploadAll would reject.
		if entry.Data.IsZero() {
			continue
		}
		ds.Set(entry.Data)
	}
	for _, dir := range c.OutputDirs {
		td, err := dirOutputTree(ctx, c.HashFS, c.WorkspaceRoot, string(dir), ds)
		if err != nil {
			return err
		}
		relPath, err := filepath.Rel(string(c.WorkDir), string(dir))
		if err != nil {
			return fmt.Errorf("rel path for dir output %s: %w", dir, err)
		}
		result.OutputDirectories = append(result.OutputDirectories, &rpb.OutputDirectory{
			Path:       filepath.ToSlash(relPath),
			TreeDigest: td.Proto(),
		})
	}
	return nil
}

// dirOutputTree builds the REAPI Tree for a directory output (relative to root), registers its blobs in ds, and returns the Tree digest.
//
// The Tree digest must be deterministic across rebuilds of identical content:
// it is the action-cache key for the directory output, so an unstable digest
// would defeat cache/CAS dedup of an unchanged output.
func dirOutputTree(ctx context.Context, hashFS *hashfs.HashFS, root, dir string, ds *blob.Store) (digest.Digest, error) {
	entries, err := hashFS.Entries(ctx, root, []path.Path{path.Path(dir + "/")})
	if err != nil {
		return digest.Digest{}, fmt.Errorf("enumerate dir output %s: %w", dir, err)
	}
	mt := merkletree.New(hashFS.DigestFunction(), ds)
	prefix := dir + "/"
	for _, ent := range entries {
		rel, ok := strings.CutPrefix(string(ent.Name), prefix)
		if !ok || rel == "" {
			// The bare directory itself or an entry outside dir; empty
			// subdirectories are added by the walk below.
			continue
		}
		if err := mt.Set(merkletree.Entry{
			Name:         path.Path(rel),
			Data:         ent.Data,
			IsExecutable: ent.IsExecutable,
			Target:       ent.Target,
		}); err != nil {
			return digest.Digest{}, fmt.Errorf("merkletree set %s: %w", ent.Name, err)
		}
	}
	// hashFS.Entries flattens to files, so empty subdirectories produce no
	// entry above. A remote backend records every subdirectory in the tree,
	// so a cached local output must too, else a cache hit materializes the
	// output missing its empty directories. mt.Set is idempotent here.
	fsys := hashFS.FileSystem(ctx, root)
	walkErr := fs.WalkDir(fsys, dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		rel, ok := strings.CutPrefix(p, prefix)
		if !ok || rel == "" {
			return nil
		}
		return mt.Set(merkletree.Entry{Name: path.Path(rel)})
	})
	if walkErr != nil {
		return digest.Digest{}, fmt.Errorf("enumerate dirs for dir output %s: %w", dir, walkErr)
	}
	if _, err := mt.Build(ctx); err != nil {
		return digest.Digest{}, fmt.Errorf("build tree for dir output %s: %w", dir, err)
	}
	rootDir := mt.RootDirectory()
	// Sort children by their own digest for a deterministic child order:
	// mt.Directories() ranges a Go map, and an unstable order would give
	// identical content a different tree digest (cache key) each build.
	children := make([]*rpb.Directory, 0, len(mt.Directories()))
	for _, d := range mt.Directories() {
		if d != rootDir {
			children = append(children, d)
		}
	}
	childHash := make(map[*rpb.Directory]string, len(children))
	for _, d := range children {
		cd, err := blob.FromProtoMessage(hashFS.DigestFunction(), d)
		if err != nil {
			return digest.Digest{}, fmt.Errorf("child dir digest for dir output %s: %w", dir, err)
		}
		childHash[d] = cd.Digest().Hash
	}
	sort.Slice(children, func(i, j int) bool {
		return childHash[children[i]] < childHash[children[j]]
	})
	tree := &rpb.Tree{Root: rootDir, Children: children}
	b, err := proto.Marshal(tree)
	if err != nil {
		return digest.Digest{}, fmt.Errorf("marshal tree for dir output %s: %w", dir, err)
	}
	td := blob.FromBytes(hashFS.DigestFunction(), fmt.Sprintf("tree:%s", dir), b)
	ds.Set(td)
	return td.Digest(), nil
}
