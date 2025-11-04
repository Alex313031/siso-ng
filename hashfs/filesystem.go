// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package hashfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"strings"
	"syscall"

	log "github.com/golang/glog"

	"go.chromium.org/build/siso/o11y/clog"
)

// File implements https://pkg.go.dev/io/fs#File.
// This is an in-memory representation of the file.
type File struct {
	buf []byte
	fi  FileInfo
}

// Stat returns a FileInfo describing the file.
func (f *File) Stat() (fs.FileInfo, error) {
	return f.fi, nil
}

// Read reads contents from the file.
func (f *File) Read(buf []byte) (int, error) {
	if len(f.buf) == 0 {
		return 0, io.EOF
	}
	n := copy(buf, f.buf)
	f.buf = f.buf[n:]
	return n, nil
}

// Close closes the file.
func (f *File) Close() error {
	return nil
}

// Dir implements https://pkg.go.dev/io/fs#ReadDirFile.
type Dir struct {
	ents []DirEntry
	fi   FileInfo
}

// Stat returns a FileInfo describing the directory.
func (d *Dir) Stat() (fs.FileInfo, error) {
	return d.fi, nil
}

// Read reads contents from the dir (permission denied).
func (d *Dir) Read(buf []byte) (int, error) {
	return 0, fs.ErrPermission
}

// Close closes the directory.
func (d *Dir) Close() error {
	return nil
}

// ReadDir reads directory entries from the dir.
func (d *Dir) ReadDir(n int) ([]fs.DirEntry, error) {
	if n <= 0 {
		var ents []fs.DirEntry
		for _, e := range d.ents {
			ents = append(ents, e)
		}
		d.ents = nil
		return ents, nil
	}
	if len(d.ents) == 0 {
		return nil, io.EOF
	}
	var i int
	var e DirEntry
	var ents []fs.DirEntry
	for i, e = range d.ents {
		if i == n {
			break
		}
		ents = append(ents, e)
	}
	d.ents = d.ents[len(ents):]
	return ents, nil
}

// FileSystem provides fs.{FS,ReadDirFS,ReadFileFS,StatFS,SubFS} interfaces.
type FileSystem struct {
	hashFS *HashFS
	ctx    context.Context
	dir    string
}

// Open opens a file for name.
func (fsys FileSystem) Open(name string) (fs.File, error) {
	fi, err := fsys.Stat(name)
	if err != nil {
		return nil, &fs.PathError{
			Op:   "open",
			Path: name,
			Err:  err,
		}
	}
	if log.V(1) {
		clog.Infof(fsys.ctx, "fsys open %q: %q", name, fi.(FileInfo).Path())
	}
	if fi.IsDir() {
		ents, err := fsys.hashFS.ReadDir(fsys.ctx, "", fi.(FileInfo).Path())
		if err != nil {
			return nil, &fs.PathError{
				Op:   "open",
				Path: name,
				Err:  err,
			}
		}
		return &Dir{
			ents: ents,
			fi:   fi.(FileInfo),
		}, nil
	}
	buf, err := fsys.hashFS.ReadFile(fsys.ctx, "", fi.(FileInfo).Path())
	if err != nil {
		return nil, &fs.PathError{
			Op:   "open",
			Path: name,
			Err:  err,
		}
	}
	return &File{
		buf: buf,
		fi:  fi.(FileInfo),
	}, nil
}

// resolveSymlinkPath returns resolved symlink at path (relative to root
// or absolute) to target, which is path under root, or absolute path.
func resolveSymlinkPath(root, path, target string) string {
	var name string
	if filepath.IsAbs(target) {
		name = target
	} else {
		name = filepath.Join(filepath.Dir(path), target)
		if !filepath.IsAbs(name) && !filepath.IsLocal(name) {
			if path == "." {
				name = filepath.Join(filepath.Dir(root), name)
			} else {
				name = filepath.Join(root, name)
			}
		}
	}
	name = filepath.ToSlash(name)
	relPath, err := filepath.Rel(root, name)
	if err == nil && filepath.IsLocal(relPath) {
		name = relPath
	}
	return filepath.ToSlash(name)
}

// ReadDir reads directory at name.
func (fsys FileSystem) ReadDir(name string) ([]fs.DirEntry, error) {
	pathname := name
	for range maxSymlinks {
		root := fsys.dir // abspath
		if filepath.IsAbs(name) {
			root = ""
		}
		if log.V(1) {
			clog.Infof(fsys.ctx, "fsys readdir %q %q", root, name)
		}
		ents, err := fsys.hashFS.ReadDir(fsys.ctx, root, name)
		if err != nil {
			var serr SymlinkError
			if errors.As(err, &serr) {
				name = resolveSymlinkPath(fsys.dir, serr.Path, serr.Target)
				continue
			}
			return nil, &fs.PathError{
				Op:   "readdir",
				Path: pathname,
				Err:  err,
			}
		}
		dirents := make([]fs.DirEntry, 0, len(ents))
		for _, e := range ents {
			dirents = append(dirents, e)
		}
		return dirents, nil
	}
	return nil, &fs.PathError{
		Op:   "readdir",
		Path: pathname,
		Err:  syscall.ELOOP,
	}
}

// ReadFile reads contents of name.
func (fsys FileSystem) ReadFile(name string) ([]byte, error) {
	pathname := name
	for range maxSymlinks {
		root := fsys.dir // abspath
		if filepath.IsAbs(name) {
			root = ""
		}
		if log.V(1) {
			clog.Infof(fsys.ctx, "fsys readfile %q %q", root, name)
		}
		buf, err := fsys.hashFS.ReadFile(fsys.ctx, root, name)
		if err != nil {
			var serr SymlinkError
			if errors.As(err, &serr) {
				name = resolveSymlinkPath(fsys.dir, serr.Path, serr.Target)
				continue
			}
			return nil, &fs.PathError{
				Op:   "readfile",
				Path: pathname,
				Err:  err,
			}
		}
		return buf, nil
	}
	return nil, &fs.PathError{
		Op:   "readfile",
		Path: pathname,
		Err:  syscall.ELOOP,
	}
}

// ReadLink retrurns the destination of the named symbolink link.
func (fsys FileSystem) ReadLink(name string) (string, error) {
	fi, err := fsys.hashFS.Stat(fsys.ctx, fsys.dir, name)
	if err != nil {
		return "", &fs.PathError{
			Op:   "readlink",
			Path: name,
			Err:  err,
		}
	}
	target := fi.Target()
	if target == "" {
		return "", &fs.PathError{
			Op:   "readlink",
			Path: name,
			Err:  syscall.EINVAL,
		}
	}
	return target, nil
}

// Lstat returns a FileInfo describing the named file.
// Lstat makes no attempt to follow the link.
func (fsys FileSystem) Lstat(name string) (fs.FileInfo, error) {
	fi, err := fsys.hashFS.Stat(fsys.ctx, fsys.dir, name)
	if err != nil {
		return nil, &fs.PathError{
			Op:   "lstat",
			Path: name,
			Err:  err,
		}
	}
	return fi, nil
}

// Stat gets stat of name.
// Stat makes attempt to follow the link.
// It returns non-nil fs.FileInfo even if err != nil, e.g.
// dangling symlink.
// i.e. Even if err != nil, fs.FileInfo may be valid for Visited or
// VisitedPaths, so can get intermediate symlinks's FileInfo.
func (fsys FileSystem) Stat(name string) (fs.FileInfo, error) {
	pathname := name
	var fis []FileInfo
	for range maxSymlinks {
		root := fsys.dir // abspath
		if filepath.IsAbs(name) {
			root = ""
		}
		if log.V(1) {
			clog.Infof(fsys.ctx, "fsys stat %q %q", root, name)
		}
		fi, err := fsys.hashFS.Stat(fsys.ctx, root, name)
		if err != nil {
			return FileInfo{
					root:  root,
					fname: name,
					fis:   fis,
				}, &fs.PathError{
					Op:   "stat",
					Path: pathname,
					Err:  err,
				}
		}
		target := fi.Target()
		if target == "" {
			fi.fis = fis
			return fi, nil
		}
		fis = append(fis, fi)
		name = resolveSymlinkPath(fsys.dir, fi.Path(), target)
		continue
	}
	return FileInfo{
			root:  fsys.dir,
			fname: pathname,
			fis:   fis,
		}, &fs.PathError{
			Op:   "stat",
			Path: pathname,
			Err:  syscall.ELOOP,
		}
}

// Visited returns visited FileInfo to get the fi by Stat, including fi itself.
// Returns empty if fi is not hashfs's FileInfo.
func (fsys FileSystem) Visited(fi fs.FileInfo) []FileInfo {
	hfi, ok := fi.(FileInfo)
	if !ok {
		return nil
	}
	if hfi.e == nil {
		return hfi.fis
	}
	return append(hfi.fis, hfi)
}

// VisitedPaths returns visited paths under fsys's root to get the fi by Stat.
// It won't return escaped paths out of root of fsys.
// Note it may not include given path for Stat, if given path is symlink.
// i.e. visited paths are symlink resolved paths for the given path.
func (fsys FileSystem) VisitedPaths(fi fs.FileInfo) []string {
	var visited []string
	for _, fi := range fsys.Visited(fi) {
		fname := fi.Path()
		rel, err := filepath.Rel(fsys.dir, fname)
		if err != nil {
			continue
		}
		if !filepath.IsLocal(rel) {
			// outside of fsys root.
			continue
		}
		visited = append(visited, filepath.ToSlash(rel))
	}
	return visited
}

// ExpandSymlinks expands all intermediate symlink dirs under fsys's root
// to access name.
func (fsys FileSystem) ExpandSymlinks(name string) []string {
	var names []string
resolve:
	for range maxSymlinks {
		elems := strings.Split(name, "/")
		for i := range elems {
			pathname := strings.Join(elems[:i+1], "/")
			fi, err := fsys.hashFS.Stat(fsys.ctx, fsys.dir, pathname)
			if err != nil {
				clog.Warningf(fsys.ctx, "no intermediate dir for %s: %s: %v", name, pathname, err)
				break resolve
			}
			if target := fi.Target(); target != "" {
				names = append(names, pathname)
				// TODO: support remote chroot?
				if filepath.IsAbs(target) {
					// out of exec root
					break resolve
				}
				targetPath := filepath.ToSlash(filepath.Join(filepath.Dir(pathname), target))
				if !filepath.IsLocal(targetPath) {
					// out of exec root
					break resolve
				}
				name = filepath.ToSlash(filepath.Join(targetPath, strings.Join(elems[i+1:], "/")))
				continue resolve
			}
		}
	}
	names = append(names, name)
	return names
}

// Sub returns an FS corresponding to the subtree rooted at dir.
func (fsys FileSystem) Sub(dir string) (fs.FS, error) {
	fi, err := fsys.Stat(dir)
	if err != nil {
		return nil, &fs.PathError{
			Op:   "sub",
			Path: dir,
			Err:  err,
		}
	}
	if !fi.IsDir() {
		return nil, &fs.PathError{
			Op:   "sub",
			Path: dir,
			Err:  fmt.Errorf("not directory: %s", dir),
		}
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(fsys.dir, dir)
	}
	if log.V(1) {
		clog.Infof(fsys.ctx, "fsys sub %q", dir)
	}
	return FileSystem{
		hashFS: fsys.hashFS,
		ctx:    fsys.ctx,
		dir:    dir,
	}, nil
}
