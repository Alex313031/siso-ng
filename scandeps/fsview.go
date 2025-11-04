// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package scandeps

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	log "github.com/golang/glog"

	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/o11y/trace"
	"go.chromium.org/build/siso/runtimex"
	"go.chromium.org/build/siso/sync/semaphore"
)

var cppScanSema = semaphore.New("cppscan", runtimex.NumCPU())

// fsview is a view of filesystem per scandeps process.
// It will reduce unnecessary contention to filesystem.
type fsview struct {
	fs        *filesystem
	execRoot  string
	inputDeps map[string][]string

	// precomputed trees for this include dirs (framework, sysroots).
	precomputedTrees []string

	// search path: i.e. -I
	searchPaths []string

	// framework search path: i.e. -F
	frameworkPaths []string

	// true:exist false:notExist noEntry:not-checked-yet
	dirs  map[string]bool
	files map[string]*scanResult

	// top entries exist in searchPaths
	// dir -> directory entries in the dir.
	topEnts map[string]*sync.Map

	// result
	visited map[string]bool

	// reuse allocations for pathJoin.
	pathbuf bytes.Buffer
}

type searchPathType int

const (
	noSearchPath searchPathType = iota
	includeSearchPath
	frameworkSearchPath
)

func (fv *fsview) addDir(ctx context.Context, dir string, searchPath searchPathType) {
	dirheaders := dir + ":headers"
	if _, ok := fv.inputDeps[dirheaders]; ok {
		// use precomputed subtree for this directory,
		// so no need to handle this dir.
		return
	}
	var sysinc string
	for _, sysinc = range fv.precomputedTrees {
		if dir == sysinc || strings.HasPrefix(dir, sysinc+"/") {
			// use precomputed subtree (sysroot or framework)
			// for this directory, so no need to handle this dir.
			return
		}
	}
	switch searchPath {
	case noSearchPath:
	case includeSearchPath:
		// dir may be added to dir stack, but not in searchPaths yet?
		seen := slices.Contains(fv.searchPaths, dir)
		if !seen {
			fv.searchPaths = append(fv.searchPaths, dir)
			if log.V(1) {
				clog.Infof(ctx, "add dir:%d %s", len(fv.searchPaths), dir)
			}
		}
	case frameworkSearchPath:
		seen := slices.Contains(fv.frameworkPaths, dir)
		if !seen {
			fv.frameworkPaths = append(fv.frameworkPaths, dir)
			if log.V(1) {
				clog.Infof(ctx, "add dir[framework]:%d %s", len(fv.frameworkPaths), dir)
			}
		}
	}
	if log.V(1) {
		clog.Infof(ctx, "add dir readdir %s", dir)
	}
	dents, symlinks, err := fv.fs.ReadDir(ctx, fv.execRoot, dir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			clog.Warningf(ctx, "failed in readdir %s: %v", dir, err)
		}
		return
	}
	// need to add the dir, and its symlinks.
	fv.markVisited(dir)
	fv.markVisited(symlinks...)
	fv.topEnts[dir] = dents
}

func (fv *fsview) get(ctx context.Context, dir, name string) (string, *scanResult, error) {
	top := topElem(name)
	// don't check topEnt for framework headers
	// since it would not work well because framework headers
	// uses symlinks.
	if top != ".." && !strings.HasSuffix(dir, ".framework/Headers") {
		if fv.topEnts[dir] == nil {
			if log.V(1) {
				clog.Infof(ctx, "no dir %s for top:%s", dir, top)
			}
			return "", nil, fs.ErrNotExist
		}
		if _, ok := fv.topEnts[dir].Load(top); !ok {
			if log.V(1) {
				clog.Infof(ctx, "not found in %s for top:%s", dir, top)
			}
			return "", nil, fs.ErrNotExist
		}
	}
	incpath := fv.pathJoin(dir, name)
	if log.V(1) {
		clog.Infof(ctx, "find path %s/%s -> %s", dir, name, incpath)
	}
	if !filepath.IsLocal(incpath) {
		// out of exxecroot?
		if log.V(1) {
			clog.Infof(ctx, "find not local")
		}
		return "", nil, fs.ErrNotExist
	}
	if v, ok := fv.visited[incpath]; ok {
		if !v {
			if log.V(1) {
				clog.Infof(ctx, "find visited not found")
			}
			return "", nil, fs.ErrNotExist
		}
	}
	incpath = fv.fs.pathIntern(incpath)
	sr, err := fv.scanFile(ctx, incpath)
	if log.V(1) {
		clog.Infof(ctx, "scanFile %q %v: %v", incpath, sr, err)
	}
	if err != nil {
		fv.visited[incpath] = false
		return "", nil, err
	}
	fv.markVisited(incpath)
	fv.markVisited(sr.symlinkTargets...)
	return incpath, sr, err
}

func (fv *fsview) scanFile(ctx context.Context, fname string) (*scanResult, error) {
	sr, err := fv.scanResult(ctx, fname)
	if err != nil {
		return sr, err
	}
	sr.mu.Lock()
	defer sr.mu.Unlock()
	if sr.done {
		return sr, sr.err
	}
	ctx, span := trace.NewSpan(ctx, "scanFile")
	defer span.Close(nil)

	buf, visited, err := fv.fs.readFile(ctx, fv.execRoot, fname)
	if log.V(1) {
		clog.Infof(ctx, "scanFile readfile: %s %s %v", fname, visited, err)
	}
	if err != nil {
		return sr, sr.err
	}
	var includes []string
	var defines map[string][]string
	err = cppScanSema.Do(ctx, func(ctx context.Context) error {
		var err error
		includes, defines, err = CPPScan(ctx, fname, buf)
		return err
	})
	sr.err = err
	sr.includes = make([]string, 0, len(includes))
	for _, incname := range includes {
		sr.includes = append(sr.includes, fv.fs.intern(incname))
	}
	sr.defines = make(map[string][]string, len(defines))
	for k, v := range defines {
		k := fv.fs.intern(k)
		values := make([]string, 0, len(v))
		for _, val := range v {
			values = append(values, fv.fs.intern(val))
		}
		sr.defines[k] = values
	}
	sr.symlinkTargets = visited
	sr.done = true
	return sr, sr.err
}

func (fv *fsview) scanResult(ctx context.Context, incpath string) (*scanResult, error) {
	sr, ok := fv.getFile(incpath)
	if ok {
		if log.V(1) {
			clog.Infof(ctx, "scanResult getFile %q %v", incpath, sr)
		}
		if sr == nil {
			return nil, fs.ErrNotExist
		}
		return sr, nil
	}
	if strings.Contains(incpath, ".framework/Headers/") {
		// framework headers are symlinks to the framework bundle.
		// so we don't need to check the directory existence.
		if log.V(1) {
			clog.Infof(ctx, "scanResult for framework %q", incpath)
		}
	} else {
		i := -1
		for {
			j := strings.IndexByte(incpath[i+1:], '/')
			if j < 0 {
				break
			}
			i += 1 + j
			dirname := incpath[:i]
			exist, ok := fv.checkDir(dirname)
			if ok {
				if exist {
					continue
				}
				if log.V(1) {
					clog.Infof(ctx, "scanResult %q checkDir=%q not exist", incpath, dirname)
				}
				return nil, fs.ErrNotExist
			}
			fi, err := fv.fs.statFollowSymlink(ctx, fv.execRoot, dirname)
			if err != nil {
				fv.setDir(dirname, false)
				if log.V(1) {
					clog.Infof(ctx, "scanResult %q stat dir=%q not exist", incpath, dirname)
				}
				return nil, fs.ErrNotExist
			}
			if !fi.IsDir() {
				fv.setDir(dirname, false)
				if log.V(1) {
					clog.Infof(ctx, "scanResult %q dir=%q mode=%s", incpath, dirname, fi.Mode())
				}
				return nil, fs.ErrNotExist
			}
			fv.setDir(dirname, true)
		}
	}
	fi, err := fv.fs.statFollowSymlink(ctx, fv.execRoot, incpath)
	if log.V(1) {
		clog.Infof(ctx, "scanResult stat %q: %v", incpath, err)
	}
	if err != nil {
		fv.setFile(incpath, nil)
		return nil, fs.ErrNotExist
	}
	if log.V(1) {
		clog.Infof(ctx, "scanResult stat %q mode=%s", incpath, fi.Mode())
	}
	if fi.Mode().IsDir() {
		fv.setDir(incpath, true)
		fv.setFile(incpath, nil)
		return nil, fs.ErrInvalid
	}
	if !fi.Mode().IsRegular() {
		fv.setFile(incpath, nil)
		return nil, fs.ErrInvalid
	}
	sr = &scanResult{}
	fv.setFile(incpath, sr)
	if strings.Contains(incpath, ".framework/Headers/") {
		fv.setDir(path.Dir(incpath), true)
	}
	return sr, nil
}

func (fv *fsview) checkDir(dname string) (exist, ok bool) {
	exist, ok = fv.dirs[dname]
	if ok {
		return exist, ok
	}
	exist, ok = fv.fs.getDir(fv.execRoot, dname)
	if ok {
		fv.dirs[dname] = exist
		return exist, true
	}
	return false, false
}

func (fv *fsview) setDir(dname string, exist bool) {
	fv.dirs[dname] = exist
	fv.fs.setDir(fv.execRoot, dname, exist)
}

func (fv *fsview) getFile(fname string) (*scanResult, bool) {
	sr, ok := fv.files[fname]
	if ok {
		return sr, ok
	}
	sr, ok = fv.fs.getFile(fv.execRoot, fname)
	if !ok {
		return nil, false
	}
	fv.files[fname] = sr
	return sr, true
}

func (fv *fsview) setFile(fname string, sr *scanResult) {
	fv.files[fname] = sr
	fv.fs.setFile(fv.execRoot, fname, sr)
}

func (fv *fsview) markVisited(visits ...string) {
	for _, v := range visits {
		if strings.Index(v, ":") > 0 {
			// expand labels.
			fv.markVisited(fv.inputDeps[v]...)
			return
		}
		fv.visited[v] = true
		fv.markVisited(fv.inputDeps[v]...)
	}
}

func (fv *fsview) results() []string {
	results := make([]string, 0, len(fv.visited))
	for k, v := range fv.visited {
		if k == "" {
			continue
		}
		if strings.Contains(k, ":") {
			continue
		}
		if !v {
			continue
		}
		results = append(results, k)
	}
	sort.Strings(results)
	return results
}

func topElem(name string) string {
	name = strings.TrimPrefix(name, "./")
	i := strings.IndexByte(name, '/')
	if i > 0 {
		return name[:i]
	}
	return name
}

func (fv *fsview) pathJoin(dir, fname string) string {
	fv.pathbuf.Reset()
	if dir == "" || dir == "." {
		return fname
	}
	if strings.HasPrefix(fname, ".") {
		// e.g. "./foo.h", "../foo/bar.h"
		return path.Join(dir, fname)
	}
	// no path.Clean
	fv.pathbuf.WriteString(dir)
	fv.pathbuf.WriteByte('/')
	fv.pathbuf.WriteString(fname)
	return fv.pathbuf.String()
}

// getHmap returns hmap excluding files that aren't under execRoot.
func (fv *fsview) getHmap(ctx context.Context, hmap string) (map[string]string, bool) {
	m, ok := fv.fs.getHmap(ctx, fv.execRoot, hmap)
	mm := make(map[string]string)
	for k, v := range m {
		if filepath.IsAbs(v) {
			rel, err := filepath.Rel(fv.execRoot, v)
			if err != nil || !filepath.IsLocal(rel) {
				clog.Warningf(ctx, "unacceptable dir for %s in hmap %s: %s: %v", k, hmap, v, err)
				continue
			}
			v = rel
		}
		mm[k] = v
	}
	return mm, ok
}
