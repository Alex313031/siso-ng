// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package scandeps

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"

	log "github.com/golang/glog"

	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/o11y/trace"
	"go.chromium.org/build/siso/path"
	"go.chromium.org/build/siso/sync/semaphore"
)

var CPPScanSema = semaphore.New("cppscan", runtime.GOMAXPROCS(0))

// fsview is a view of filesystem per scandeps process.
// It will reduce unnecessary contention to filesystem.
type fsview struct {
	fs            *filesystem
	workspaceRoot string
	inputDeps     map[string][]string

	// precomputed trees for this include dirs (framework, sysroots).
	precomputedTrees []path.Path

	// search path: i.e. -I
	searchPaths []path.Path

	// quote search path: i.e. -iquote
	quotePaths []path.Path

	// framework search path: i.e. -F
	frameworkPaths []path.Path

	// true:exist false:notExist noEntry:not-checked-yet
	dirs  map[path.Path]bool
	files map[path.Path]*scanResult

	// top entries exist in searchPaths
	// dir -> directory entries in the dir.
	topEnts map[path.Path]*sync.Map

	// result
	visited map[path.Path]bool

	// reuse allocations for pathJoin.
	pathbuf bytes.Buffer
}

func (fv *fsview) reset(fs *filesystem, workspaceRoot string, inputDeps map[string][]string, precomputedTrees []path.Path) {
	fv.fs = fs
	fv.workspaceRoot = workspaceRoot
	fv.inputDeps = inputDeps
	fv.precomputedTrees = precomputedTrees
	fv.searchPaths = fv.searchPaths[:0]
	fv.quotePaths = fv.quotePaths[:0]
	fv.frameworkPaths = fv.frameworkPaths[:0]
	clear(fv.dirs)
	clear(fv.files)
	clear(fv.topEnts)
	clear(fv.visited)
	fv.pathbuf.Reset()
}

type searchPathType int

const (
	noSearchPath searchPathType = iota
	includeSearchPath
	quoteSearchPath
	frameworkSearchPath
)

func (fv *fsview) addDir(ctx context.Context, dir path.Path, searchPath searchPathType) {
	if _, ok := fv.fs.headersDirs[string(dir)]; ok {
		// use precomputed subtree for this directory,
		// so no need to handle this dir.
		return
	}
	var sysinc path.Path
	for _, sysinc = range fv.precomputedTrees {
		if dir == sysinc || (
		// Avoid strings.HasPrefix(dir, sysinc+"/") to reduce allocation in string concat.
		len(dir) > len(sysinc) && dir[len(sysinc)] == '/' && dir[:len(sysinc)] == sysinc) {
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
	case quoteSearchPath:
		seen := slices.Contains(fv.quotePaths, dir)
		if !seen {
			fv.quotePaths = append(fv.quotePaths, dir)
			if log.V(1) {
				clog.Infof(ctx, "add dir[quote]:%d %s", len(fv.quotePaths), dir)
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
	dents, symlinks, err := fv.fs.ReadDir(ctx, fv.workspaceRoot, dir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			clog.Warningf(ctx, "failed in readdir %s: %v", dir, err)
		}
		return
	}
	// need to add the dir, and its symlinks.
	fv.markVisited(dir)
	for _, sl := range symlinks {
		fv.markVisited(path.FromClean(sl))
	}
	fv.topEnts[dir] = dents
}

func (fv *fsview) get(ctx context.Context, dir path.Path, name string) (path.Path, *scanResult, error) {
	top := topElem(name)
	// don't check topEnt for framework headers
	// since it would not work well because framework headers
	// uses symlinks.
	if top != ".." && !strings.HasSuffix(string(dir), ".framework/Headers") {
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
	if !filepath.IsLocal(string(incpath)) {
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
	incpath = path.FromClean(fv.fs.pathIntern(string(incpath)))
	sr, err := fv.scanFile(ctx, incpath)
	if log.V(1) {
		clog.Infof(ctx, "scanFile %q %v: %v", incpath, sr, err)
	}
	if err != nil {
		fv.visited[incpath] = false
		return "", nil, err
	}
	fv.markVisited(incpath)
	for _, sl := range sr.symlinkTargets {
		fv.markVisited(path.FromClean(sl))
	}
	return incpath, sr, err
}

func (fv *fsview) scanFile(ctx context.Context, fname path.Path) (*scanResult, error) {
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

	buf, visited, err := fv.fs.readFile(ctx, fv.workspaceRoot, fname)
	if log.V(1) {
		clog.Infof(ctx, "scanFile readfile: %s %s %v", fname, visited, err)
	}
	if err != nil {
		// Return an isolated empty result, not the shared un-done sr: a
		// coalesced sibling may re-scan sr under sr.mu, so handing it back for
		// an unlocked read would race. The shared sr stays un-done for retry.
		return &scanResult{}, nil
	}
	var includes []string
	var defines map[string][]string
	err = CPPScanSema.Do(ctx, func(ctx context.Context) error {
		var err error
		includes, defines, err = CPPScan(ctx, string(fname), buf)
		return err
	})
	if err != nil && ctx.Err() != nil {
		// Caller's context was canceled: transient. Don't poison the shared
		// scanResult; leave it un-done so another live scan retries.
		return sr, err
	}
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

func (fv *fsview) scanResult(ctx context.Context, incpath path.Path) (*scanResult, error) {
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
	if strings.Contains(string(incpath), ".framework/Headers/") {
		// framework headers are symlinks to the framework bundle.
		// so we don't need to check the directory existence.
		if log.V(1) {
			clog.Infof(ctx, "scanResult for framework %q", incpath)
		}
	} else {
		s := string(incpath)
		i := -1
		for {
			j := strings.IndexByte(s[i+1:], '/')
			if j < 0 {
				break
			}
			i += 1 + j
			dirname := path.FromClean(s[:i])
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
			fi, err := fv.fs.statFollowSymlink(ctx, fv.workspaceRoot, dirname)
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
	fi, err := fv.fs.statFollowSymlink(ctx, fv.workspaceRoot, incpath)
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
	// adopt the shared winner so racing fsviews read the file once.
	sr = fv.setFile(incpath, sr)
	if strings.Contains(string(incpath), ".framework/Headers/") {
		fv.setDir(incpath.Dir(), true)
	}
	return sr, nil
}

func (fv *fsview) checkDir(dname path.Path) (exist, ok bool) {
	exist, ok = fv.dirs[dname]
	if ok {
		return exist, ok
	}
	exist, ok = fv.fs.getDir(fv.workspaceRoot, dname)
	if ok {
		fv.dirs[dname] = exist
		return exist, true
	}
	return false, false
}

func (fv *fsview) setDir(dname path.Path, exist bool) {
	fv.dirs[dname] = exist
	fv.fs.setDir(fv.workspaceRoot, dname, exist)
}

func (fv *fsview) getFile(fname path.Path) (*scanResult, bool) {
	sr, ok := fv.files[fname]
	if ok {
		return sr, ok
	}
	sr, ok = fv.fs.getFile(fv.workspaceRoot, fname)
	if !ok {
		return nil, false
	}
	fv.files[fname] = sr
	return sr, true
}

// setFile delegates to filesystem.setFile and caches the shared winner;
// callers must use the returned value.
func (fv *fsview) setFile(fname path.Path, sr *scanResult) *scanResult {
	actual := fv.fs.setFile(fv.workspaceRoot, fname, sr)
	fv.files[fname] = actual
	return actual
}

func (fv *fsview) markVisited(visits ...path.Path) {
	// Walk visits directly without cloning; only spill into pending
	// when a node has inputDeps to chase. Most files have none, so
	// most calls allocate nothing.
	var pending []path.Path
	for _, v := range visits {
		if fv.visited[v] {
			continue
		}
		fv.visited[v] = true
		if deps := fv.inputDeps[string(v)]; len(deps) > 0 {
			for _, dep := range deps {
				pending = append(pending, path.FromClean(dep))
			}
		}
	}
	for len(pending) > 0 {
		n := len(pending) - 1
		v := pending[n]
		pending = pending[:n]
		if fv.visited[v] {
			continue
		}
		fv.visited[v] = true
		if deps := fv.inputDeps[string(v)]; len(deps) > 0 {
			for _, dep := range deps {
				pending = append(pending, path.FromClean(dep))
			}
		}
	}
}

func (fv *fsview) results() []path.Path {
	results := make([]path.Path, 0, len(fv.visited))
	for k, v := range fv.visited {
		if k == "" {
			continue
		}
		if strings.Contains(string(k), ":") {
			continue
		}
		if !v {
			continue
		}
		results = append(results, k)
	}
	slices.Sort(results)
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

func (fv *fsview) pathJoin(dir path.Path, fname string) path.Path {
	// fname comes verbatim from an #include directive. In headers generated
	// on Windows it can contain backslashes or redundant slashes, so
	// normalize with path.New. A non-clean path here would end up in the
	// remote input tree, where the compiler cannot resolve the include.
	fv.pathbuf.Reset()
	if dir == "" || dir == "." {
		return path.New(fname)
	}
	if strings.HasPrefix(fname, ".") {
		// e.g. "./foo.h", "../foo/bar.h"
		return path.New(string(dir) + "/" + fname)
	}
	fv.pathbuf.WriteString(string(dir))
	fv.pathbuf.WriteByte('/')
	fv.pathbuf.WriteString(fname)
	return path.New(fv.pathbuf.String())
}

// getHmap returns hmap excluding files that aren't under workspaceRoot.
func (fv *fsview) getHmap(ctx context.Context, hmap path.Path) (map[string]string, bool) {
	m, ok := fv.fs.getHmap(ctx, fv.workspaceRoot, hmap)
	mm := make(map[string]string)
	for k, v := range m {
		if filepath.IsAbs(v) {
			rel, err := filepath.Rel(fv.workspaceRoot, v)
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
