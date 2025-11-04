// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package merkletree operates on a merkle tree for remote execution API.
//
// You can find the Tree proto in REAPI here:
// https://github.com/bazelbuild/remote-apis/blob/c1c1ad2c97ed18943adb55f06657440daa60d833/build/bazel/remote/execution/v2/remote_execution.proto#L838
// See also https://en.Wikipedia.org/wiki/Merkle_tree for general explanations about Merkle tree.
package merkletree

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/protobuf/proto"

	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/reapi/digest"
)

// MerkleTree represents a merkle tree.
type MerkleTree struct {
	// dirname to Directory.
	// empty dirname is root.
	m     map[string]*rpb.Directory
	store *digest.Store
}

// New creates new merkle tree with digest store.
func New(store *digest.Store) *MerkleTree {
	return &MerkleTree{
		m: map[string]*rpb.Directory{
			"": {},
		},
		store: store,
	}
}

// Entry is an entry in the tree.
type Entry struct {
	// Name is relative path from root dir.
	// it might not be clean path.
	// 'dir1/../dir2/file' will create
	//  - 'dir1/'
	//  - 'dir2/'
	//  - 'dir2/file'
	// error if name goes out to root.
	Name string

	// Data is entry's content. `nil` for directories and symlinks.
	Data digest.Data

	// IsExecutable is true if the file is executable.
	// no need to set this for directory.
	IsExecutable bool

	// If the file is a symlink, then this should be set to the target of the symlink.
	Target string
}

// IsDir returns true when the entry is a directory.
func (e Entry) IsDir() bool {
	return e.Data.IsZero() && e.Target == ""
}

// IsSymlink returns true when the entry is a symlink.
func (e Entry) IsSymlink() bool {
	return e.Data.IsZero() && e.Target != ""
}

var (
	// ErrAbsPath indicates name in Entry is absolute path.
	ErrAbsPath = errors.New("merkletree: absolute path name")

	// ErrAmbigFileSymlink indicates Entry has both `Data` and `Target` fields, cannot determine
	// whether it is File or Symlink.
	ErrAmbigFileSymlink = errors.New("merkletree: unable to determine file vs symlink")

	// ErrBadPath indicates name in Entry contains bad path component
	// e.g. "." or "..".
	ErrBadPath = errors.New("merkletree: bad path component")

	// ErrBadTree indicates TreeEntry has bad digest.
	ErrBadTree = errors.New("merkletree: bad tree")

	// ErrPrecomputedSubTree indicates to set an entry in precomputed subtree, but we can't change anything under the subtree because it used precomputed digest.
	ErrPrecomputedSubTree = errors.New("merkletree: set in precomputed subtree")
)

type dirstate struct {
	name string
	dir  *rpb.Directory
}

func splitElem(fname string) []string {
	return strings.Split(fname, "/")
}

// Set sets an entry.
// It may return ErrAbsPath/ErrAmbigFileSymlink/ErrBadPath as error.
func (m *MerkleTree) Set(entry Entry) error {
	fname := entry.Name
	if entry.Target != "" && !entry.Data.IsZero() {
		return fmt.Errorf("set %s: %w", fname, ErrAmbigFileSymlink)
	}
	if filepath.IsAbs(fname) || strings.HasPrefix(fname, "/") || strings.HasPrefix(fname, `\`) {
		return fmt.Errorf("set %s: %w", fname, ErrAbsPath)
	}
	fname = filepath.ToSlash(fname)
	if entry.IsDir() || entry.Target != "" {
		if _, exists := m.m[fname]; exists {
			return nil
		}
	}
	elems := splitElem(fname)
	if len(elems) == 0 {
		if !entry.Data.IsZero() {
			return fmt.Errorf("set %s: %w", fname, ErrBadPath)
		}
		return nil
	}
	cur := dirstate{
		name: ".",
		dir:  m.RootDirectory(),
	}
	var dirstack []dirstate
	for {
		var name string
		name, elems = elems[0], elems[1:]
		if len(elems) == 0 {
			// a leaf.
			if entry.Data.IsZero() {
				if name == "" {
					return fmt.Errorf("set %s: empty path element: %w", fname, ErrBadPath)
				}
				if name == "." {
					return nil
				}
				if name == ".." && len(dirstack) == 0 {
					return fmt.Errorf("set %s: out of exec root: %w", fname, ErrBadPath)
				}
				if name == ".." {
					return nil
				}
				if entry.IsSymlink() {
					cur.dir.Symlinks = append(cur.dir.Symlinks, &rpb.SymlinkNode{
						Name:   name,
						Target: entry.Target,
					})
					return nil
				}
				_, err := m.setDir(cur, name)
				if err != nil {
					return err
				}
				return nil
			}
			if name == "." || name == ".." {
				return fmt.Errorf("set %s: unexpected %s: %w", fname, name, ErrBadPath)
			}
			if m.store != nil {
				m.store.Set(entry.Data)
			}
			cur.dir.Files = append(cur.dir.Files, &rpb.FileNode{
				Name:         name,
				Digest:       entry.Data.Digest().Proto(),
				IsExecutable: entry.IsExecutable,
			})
			return nil
		}
		if name == "" {
			continue
		}
		if name == "." {
			continue
		}
		if name == ".." {
			if len(dirstack) == 0 {
				return fmt.Errorf("set %s: out of exec root: %w", fname, ErrBadPath)
			}
			cur, dirstack = dirstack[len(dirstack)-1], dirstack[:len(dirstack)-1]
			continue
		}
		dirstack = append(dirstack, cur)
		var err error
		cur, err = m.setDir(cur, name)
		if err != nil {
			return fmt.Errorf("set %s: %w", fname, err)
		}
	}
}

func pathJoin(dir, base string) string {
	var b strings.Builder
	if dir == "." || dir == "" {
		return base
	}
	b.Grow(len(dir) + 1 + len(base))
	b.WriteString(dir)
	b.WriteByte('/')
	b.WriteString(base)
	return b.String()
}

func (m *MerkleTree) setDir(cur dirstate, name string) (dirstate, error) {
	dirname := pathJoin(cur.name, name)
	dir, exists := m.m[dirname]
	if !exists {
		dirnode := &rpb.DirectoryNode{
			Name: name,
			// compute digest later
		}
		cur.dir.Directories = append(cur.dir.Directories, dirnode)
		dir = &rpb.Directory{}
		m.m[dirname] = dir
	}
	if dir == nil {
		return dirstate{}, ErrPrecomputedSubTree
	}
	return dirstate{name: dirname, dir: dir}, nil
}

// TreeEntry is a precomputed subtree entry in the tree.
// When using the same sets in a directory many times, we can precompute
// digest for the subtree to reduce computation cost and memory cost.
type TreeEntry struct {
	// Name is relative path from root dir.
	// Name should not end with "." or "..".
	Name string

	// Digest is the digest of this sub tree.
	Digest digest.Digest

	// Store is the digest store used to build the subtree.
	// It may be nil, if it is sure that the subtree was uploaded
	// to CAS and no need to check missing blobs / upload blobs
	// of the subtree.
	Store *digest.Store
}

// SetTree sets a subtree entry.
// It may return ErrAbsPath/ErrBadPath/ErrBadTree as error.
func (m *MerkleTree) SetTree(tentry TreeEntry) error {
	dname := tentry.Name
	if tentry.Digest.IsZero() {
		return fmt.Errorf("setTree %s: %w", dname, ErrBadTree)
	}
	if filepath.IsAbs(dname) || strings.HasPrefix(dname, "/") || strings.HasPrefix(dname, `\`) {
		return fmt.Errorf("setTree %s: %w", dname, ErrAbsPath)
	}
	dname = filepath.ToSlash(dname)
	_, exists := m.m[dname]
	if exists {
		return fmt.Errorf("setTree %s: %w", dname, ErrPrecomputedSubTree)
	}
	elems := splitElem(dname)
	if len(elems) == 0 {
		if !tentry.Digest.IsZero() {
			return fmt.Errorf("setTree %s: %w", dname, ErrBadPath)
		}
		return nil
	}
	cur := dirstate{
		name: ".",
		dir:  m.RootDirectory(),
	}
	var dirstack []dirstate
	for {
		var name string
		name, elems = elems[0], elems[1:]
		if len(elems) == 0 {
			// a leaf.
			if name == "" {
				return fmt.Errorf("setTree %s: empty path element: %w", dname, ErrBadPath)
			}
			if name == "." || name == ".." {
				return fmt.Errorf("setTree %s: %s at the leaf: %w", dname, name, ErrBadPath)
			}
			m.setTree(cur, name, tentry.Digest, tentry.Store)
			return nil
		}
		if name == "" {
			continue
		}
		if name == "." {
			continue
		}
		if name == ".." {
			if len(dirstack) == 0 {
				return fmt.Errorf("setTree %s: out of exec root: %w", dname, ErrBadPath)
			}
			cur, dirstack = dirstack[len(dirstack)-1], dirstack[:len(dirstack)-1]
			continue
		}
		dirstack = append(dirstack, cur)
		var err error
		cur, err = m.setDir(cur, name)
		if err != nil {
			return fmt.Errorf("setTree %s: %w", dname, err)
		}
	}
}

func (m *MerkleTree) setTree(cur dirstate, name string, d digest.Digest, store *digest.Store) {
	dirname := pathJoin(cur.name, name)
	dirnode := &rpb.DirectoryNode{
		Name:   name,
		Digest: d.Proto(),
	}
	cur.dir.Directories = append(cur.dir.Directories, dirnode)
	_, exists := m.m[dirname]
	if !exists {
		m.m[dirname] = nil
	}
	if m.store != nil && store != nil {
		ds := store.List()
		for _, d := range ds {
			data, ok := store.Get(d)
			if !ok {
				continue
			}
			m.store.Set(data)
		}
	}
}

// Build builds merkle tree and returns root's digest.
func (m *MerkleTree) Build(ctx context.Context) (digest.Digest, error) {
	err := m.resolveSymlinkDir(ctx, m.RootDirectory(), "")
	if err != nil {
		return digest.Digest{}, err
	}
	d, err := m.buildTree(ctx, m.RootDirectory(), "")
	if err != nil {
		return digest.Digest{}, err
	}
	return digest.FromProto(d), nil
}

// RootDirectory returns root directory in the merkle tree.
func (m *MerkleTree) RootDirectory() *rpb.Directory {
	return m.m[""]
}

// resolveSymlinkDir resolves symlink to directory.
// If "path/to/name" is symlink, target to "other/name",
// and "path/to/name" is directory, then the directory entries
// are merged into "path/to/other/name" directory.
func (m *MerkleTree) resolveSymlinkDir(ctx context.Context, curdir *rpb.Directory, dirname string) error {
	for _, s := range curdir.Symlinks {
		dirname := pathJoin(dirname, s.Name)
		dir, found := m.m[dirname]
		if !found {
			continue
		}
		realDirname := filepath.ToSlash(filepath.Join(filepath.Dir(dirname), s.Target))
		err := m.mergeDir(ctx, realDirname, dir)
		if err != nil {
			return err
		}
		// relocate subdirs under dirname to realDirname.
		for name, dir := range m.m {
			if strings.HasPrefix(name, dirname+"/") {
				tname := realDirname + strings.TrimPrefix(name, dirname)
				tdir, ok := m.m[tname]
				if ok {
					proto.Merge(tdir, dir)
				} else {
					tdir = dir
				}
				m.m[tname] = tdir
				delete(m.m, name)
			}
		}
	}
	for _, subdir := range curdir.Directories {
		dirname := pathJoin(dirname, subdir.Name)
		dir, found := m.m[dirname]
		if !found {
			continue
		}
		if dir != nil && subdir.Digest == nil {
			err := m.resolveSymlinkDir(ctx, dir, dirname)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// mergeDir merges dir into dirname's directory.
func (m *MerkleTree) mergeDir(ctx context.Context, dirname string, dir *rpb.Directory) error {
	elems := splitElem(dirname)
	cur := dirstate{
		name: ".",
		dir:  m.RootDirectory(),
	}
	for {
		var name string
		name, elems = elems[0], elems[1:]
		if len(elems) == 0 {
			// leaf
			cur, err := m.setDir(cur, name)
			if err != nil {
				if errors.Is(err, ErrPrecomputedSubTree) {
					// build/mac_files/SDKs/MacOSX.sdk will hit this.
					clog.Warningf(ctx, "mergeDir set %s: %v", dirname, err)
					return nil
				}
				return fmt.Errorf("mergeDir: set %s: %w", dirname, err)
			}
			cur.dir.Files = append(cur.dir.Files, dir.Files...)
			cur.dir.Symlinks = append(cur.dir.Symlinks, dir.Symlinks...)
			cur.dir.Directories = append(cur.dir.Directories, dir.Directories...)
			return nil
		}
		var err error
		cur, err = m.setDir(cur, name)
		if err != nil {
			if errors.Is(err, ErrPrecomputedSubTree) {
				// files under this dir should exist in precomputed tree.
				clog.Warningf(ctx, "mergeDir set %s: %v", dirname, err)
				return nil
			}
			return fmt.Errorf("mergeDir: set %s: %w", dirname, err)
		}
	}
}

// buildtree builds tree at curdir, which is located as dirname.
func (m *MerkleTree) buildTree(ctx context.Context, curdir *rpb.Directory, dirname string) (*rpb.Digest, error) {
	// directory should not have duplicate name.
	// http://b/124693412
	names := map[string]proto.Message{}
	var files []*rpb.FileNode
	for _, f := range curdir.Files {
		p, found := names[f.Name]
		if found {
			if !proto.Equal(f, p) {
				return nil, fmt.Errorf("duplicate file %s in %s: %s != %s", f.Name, dirname, f, p)
			}
			// Siso might send duplicate entries such as
			//   dir/subdir/../otherdir/name
			//   dir/otherdir/name
			clog.Infof(ctx, "duplicate file %s in %s: %s", f.Name, dirname, f)
			continue
		}
		names[f.Name] = f
		files = append(files, f)
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].Name < files[j].Name
	})
	curdir.Files = files

	var symlinks []*rpb.SymlinkNode
	for _, s := range curdir.Symlinks {
		p, found := names[s.Name]
		if found {
			if !proto.Equal(s, p) {
				return nil, fmt.Errorf("duplicate symlink %s in %s: %s != %s", s.Name, dirname, s, p)
			}
			clog.Infof(ctx, "duplicate symlink %s in %s: %s", s.Name, dirname, s)
			continue
		}
		names[s.Name] = s
		symlinks = append(symlinks, s)
	}
	sort.Slice(symlinks, func(i, j int) bool {
		return symlinks[i].Name < symlinks[j].Name
	})
	curdir.Symlinks = symlinks

	var dirs []*rpb.DirectoryNode
	for _, subdir := range curdir.Directories {
		dirname := pathJoin(dirname, subdir.Name)
		dir, found := m.m[dirname]
		if !found {
			return nil, fmt.Errorf("directory not found: %s", dirname)
		}
		p, found := names[subdir.Name]
		if found {
			switch p.(type) {
			case *rpb.SymlinkNode:
				clog.Infof(ctx, "use symlink for dir: %s", dirname)
				continue
			}
			// other checks later.
		}
		if dir != nil && subdir.Digest == nil {
			digest, err := m.buildTree(ctx, dir, dirname)
			if err != nil {
				return nil, err
			}
			subdir.Digest = digest
		}
		if found {
			if !proto.Equal(subdir, p) {
				return nil, fmt.Errorf("duplicate dir %s in %s: %s != %s", subdir.Name, dirname, subdir, p)
			}
			clog.Infof(ctx, "duplicate dir %s in %s: %s", subdir.Name, dirname, subdir)
			continue
		}
		names[subdir.Name] = subdir
		dirs = append(dirs, subdir)
	}
	sort.Slice(dirs, func(i, j int) bool {
		return dirs[i].Name < dirs[j].Name
	})
	curdir.Directories = dirs

	data, err := digest.FromProtoMessage(curdir)
	if err != nil {
		return nil, fmt.Errorf("directory digest %s: %w", dirname, err)
	}
	if m.store != nil {
		m.store.Set(data)
	}
	return data.Digest().Proto(), nil
}

// Directories returns directories in the merkle tree.
func (m *MerkleTree) Directories() []*rpb.Directory {
	dirs := make([]*rpb.Directory, 0, len(m.m))
	for _, d := range m.m {
		dirs = append(dirs, d)
	}
	return dirs
}
