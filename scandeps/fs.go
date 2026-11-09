// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package scandeps

import (
	"context"
	"fmt"
	"hash/maphash"
	"strings"
	"sync"

	log "github.com/golang/glog"

	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/path"
)

// filesystem is mirror of hashfs to optimize for scandeps access pattern.
// it is shared for all scandeps processes.
// without this, hashfs would have lots of negative caches for non-existing
// header files for every include directory.
type filesystem struct {
	hashfs *hashfs.HashFS

	// shard by basename to reduce lock contention
	dirs  sync.Map // basename -> dir -> []dirents
	files sync.Map // basename -> files -> *scanresult

	dircache sync.Map // dir -> base -> bool

	hmaps sync.Map // hmap path -> *hmapresult

	// sharded by maphash to reduce lock contention. A typed
	// map[string]string + RWMutex avoids boxing string keys into
	// interface{}, which a sync.Map does per access (~104M allocs
	// per cache-cold build, top of the alloc profile).
	symtab  [256]internShard  // for incname, macros
	pathtab [4096]internShard // for pathname
	seed    maphash.Seed

	// headersDirs holds the directories with a ":headers" entry in
	// InputDeps, so addDir looks up by dir instead of building
	// dir+":headers" per call (~17M string allocs per cache-cold
	// build). Read-only after ScanDeps.New, so no lock is needed.
	headersDirs map[string]struct{}
}

type dircache struct {
	ready          chan struct{}
	m              sync.Map
	symlinkTargets []string
	err            error
}

// update updates filesystem modification by fi.
func (fsys *filesystem) update(ctx context.Context, fi *hashfs.FileInfo) {
	if log.V(1) {
		clog.Infof(ctx, "update %s dir:%t", fi.Path(), fi.IsDir())
	}
	var dname path.Path
	var base string
	if !fi.IsDir() {
		fname := fi.Path()
		fsys.forgetFile(fname)
		base = string(fname.Base())
		dname = fname.Dir()
	} else {
		dname = fi.Path()
	}
	// fix dircache
	for dname := dname; !strings.HasSuffix(string(dname), "/"); {
		v, ok := fsys.dircache.Load(dname)
		if !ok {
			base = string(dname.Base())
			dname = dname.Dir()
			continue
		}
		dc := v.(*dircache)
		select {
		case <-dc.ready:
		default:
			clog.Infof(ctx, "update race ReadDir&update %s", fi.Path())
			fsys.dircache.Delete(dname)
			base = string(dname.Base())
			dname = dname.Dir()
			continue
		}
		if dc.err != nil {
			// negative cache?
			clog.Infof(ctx, "update clear negative cache %s %v", fi.Path(), dc.err)
			fsys.dircache.Delete(dname)
			base = string(dname.Base())
			dname = dname.Dir()
			continue
		}
		if base != "" {
			dc.m.LoadOrStore(base, true)
		}
		base = string(dname.Base())
		dname = dname.Dir()
	}
	for !strings.HasSuffix(string(dname), "/") {
		if fsys.markDirExists(dname) {
			return
		}
		dname = dname.Dir()
	}
}

func (fsys *filesystem) forgetFile(fname path.Path) {
	base := string(fname.Base())
	v, ok := fsys.files.Load(base)
	if ok {
		m := v.(*sync.Map)
		m.Delete(fname)
	}
}

func (fsys *filesystem) markDirExists(dname path.Path) bool {
	v, _ := fsys.dirs.LoadOrStore(string(dname.Base()), new(sync.Map))
	m := v.(*sync.Map)
	v, ok := m.Load(dname)
	if !ok {
		m.Store(dname, true)
		return false
	}
	exist := v.(bool)
	if exist {
		return true
	}
	m.Store(dname, true)
	return false
}

func (fsys *filesystem) ReadDir(ctx context.Context, workspaceRoot string, dname path.Path) (*sync.Map, []string, error) {
	fullpath := path.JoinRoot(workspaceRoot, dname)

	// To avoid allocation of `dircache` in LoadOrStore, call Load first here.
	dv, loaded := fsys.dircache.Load(fullpath)
	if !loaded {
		dv, loaded = fsys.dircache.LoadOrStore(fullpath, &dircache{
			ready: make(chan struct{}),
		})
	}

	dc := dv.(*dircache)
	if !loaded {
		go func() {
			if log.V(1) {
				clog.Infof(ctx, "fsys readdir %s", dname)
			}
			var dents []hashfs.DirEntry
			var visited []string
			var err error
			hfsys := fsys.hashfs.FileSystem(ctx, workspaceRoot)
			fi, err := hfsys.Stat(string(dname))
			if err == nil {
				if len(hfsys.Visited(fi)) > 1 {
					visited = hfsys.ExpandSymlinks(string(dname))
				}
				des, err := hfsys.ReadDir(string(dname))
				if err == nil {
					dents = make([]hashfs.DirEntry, 0, len(des))
					for _, de := range des {
						dents = append(dents, de.(hashfs.DirEntry))
					}
				}
			}
			dc.err = err
			dc.symlinkTargets = visited
			for _, de := range dents {
				if log.V(1) {
					clog.Infof(ctx, "dirent %q %q", fullpath, de.Name())
				}
				dc.m.Store(fsys.pathIntern(de.Name()), true)
			}
			walkName := fullpath
			for !strings.HasSuffix(string(walkName), "/") {
				if fsys.markDirExists(walkName) {
					break
				}
				walkName = walkName.Dir()
			}
			close(dc.ready)
		}()
	}
	select {
	case <-ctx.Done():
		return nil, nil, fmt.Errorf("readdirnames[wait]: %w", context.Cause(ctx))
	case <-dc.ready:
	}
	if dc.err != nil {
		return nil, dc.symlinkTargets, dc.err
	}
	return &dc.m, dc.symlinkTargets, nil
}

// internShard is one shard of a typed string interning table. The
// typed map avoids the per-access interface-boxing allocation a
// sync.Map would pay on the hot scandeps intern paths.
type internShard struct {
	mu sync.RWMutex
	m  map[string]string
}

// get returns the canonical interned string for v, storing it on first
// sight. clone controls whether v is cloned before storing; pass false
// for callers holding a stable Go string (e.g. filepath.Base/Dir).
func (s *internShard) get(v string, clone bool) string {
	s.mu.RLock()
	if existing, ok := s.m[v]; ok {
		s.mu.RUnlock()
		return existing
	}
	s.mu.RUnlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.m[v]; ok {
		return existing
	}
	if s.m == nil {
		s.m = make(map[string]string)
	}
	if clone {
		v = strings.Clone(v)
	}
	s.m[v] = v
	return v
}

func (fsys *filesystem) intern(v string) string {
	i := int(maphash.String(fsys.seed, v) % uint64(len(fsys.symtab)))
	return fsys.symtab[i].get(v, true)
}

func (fsys *filesystem) pathIntern(v string) string {
	i := int(maphash.String(fsys.seed, v) % uint64(len(fsys.pathtab)))
	return fsys.pathtab[i].get(v, false)
}

func (fsys *filesystem) getDir(workspaceRoot string, dname path.Path) (exist, ok bool) {
	base := string(dname.Base())
	v, _ := fsys.dirs.LoadOrStore(base, new(sync.Map))
	m := v.(*sync.Map)
	v, ok = m.Load(path.JoinRoot(workspaceRoot, dname))
	if !ok {
		return false, false
	}
	exist = v.(bool)
	return exist, true
}

func (fsys *filesystem) setDir(workspaceRoot string, dname path.Path, exist bool) {
	v, _ := fsys.dirs.LoadOrStore(string(dname.Base()), new(sync.Map))
	m := v.(*sync.Map)
	m.Store(path.JoinRoot(workspaceRoot, dname), exist)
}

func (fsys *filesystem) getFile(workspaceRoot string, fname path.Path) (*scanResult, bool) {
	v, ok := fsys.files.Load(string(fname.Base()))
	if !ok {
		return nil, false
	}
	m := v.(*sync.Map)
	v, ok = m.Load(path.JoinRoot(workspaceRoot, fname))
	if !ok {
		return nil, false
	}
	sr := v.(*scanResult)
	return sr, true
}

// setFile caches sr for fname and returns the shared winner; racing callers
// must adopt the return. sr == nil is a negative (not-found) entry: a real
// *scanResult wins over nil and nil never overwrites one, so a regular-file
// scan never adopts a nil and nil-derefs.
func (fsys *filesystem) setFile(workspaceRoot string, fname path.Path, sr *scanResult) *scanResult {
	v, _ := fsys.files.LoadOrStore(string(fname.Base()), new(sync.Map))
	m := v.(*sync.Map)
	key := path.JoinRoot(workspaceRoot, fname)
	if sr == nil {
		actual, _ := m.LoadOrStore(key, sr)
		return actual.(*scanResult)
	}
	for {
		actual, loaded := m.LoadOrStore(key, sr)
		if !loaded {
			return sr
		}
		if cur := actual.(*scanResult); cur != nil {
			return cur
		}
		if m.CompareAndSwap(key, actual, sr) {
			return sr
		}
	}
}

type hmapresult struct {
	mu sync.Mutex
	// done indicates if it has already been computed or not.
	done bool
	// ok indicates if the hmap was successfully parsed or not.
	ok bool
	// hmap entries: include -> file path.
	m map[string]string
}

// getHmap returns hmap and success flag.
// If the same hamp has been computed, the results are returned from cache.
func (fsys *filesystem) getHmap(ctx context.Context, workspaceRoot string, fname path.Path) (map[string]string, bool) {
	clog.Infof(ctx, "check hmap %s", fname)
	v, _ := fsys.hmaps.LoadOrStore(path.JoinRoot(workspaceRoot, fname), new(hmapresult))
	hr := v.(*hmapresult)
	hr.mu.Lock()
	defer hr.mu.Unlock()
	defer func() { hr.done = true }()
	if hr.done {
		clog.Infof(ctx, "check hmap %s: reuse ok=%t", fname, hr.ok)
		return hr.m, hr.ok
	}
	buf, err := fsys.hashfs.ReadFile(ctx, workspaceRoot, fname)
	if err != nil {
		clog.Warningf(ctx, "missing hmap %s: %v", fname, err)
		return nil, false
	}
	m, err := ParseHeaderMap(ctx, buf)
	if err != nil {
		clog.Warningf(ctx, "failed to parse hmap %s: %v", fname, err)
		return nil, false
	}
	clog.Infof(ctx, "hmap %s %d => %v", fname, len(buf), m)
	hr.m = m
	hr.ok = true
	return hr.m, hr.ok
}

func (fsys *filesystem) readFile(ctx context.Context, root string, fname path.Path) ([]byte, []string, error) {
	hfsys := fsys.hashfs.FileSystem(ctx, root)
	fi, err := hfsys.Stat(string(fname))
	if err != nil {
		return nil, nil, err
	}
	var visited []string
	if len(hfsys.VisitedPaths(fi)) > 1 {
		visited = hfsys.ExpandSymlinks(string(fname))
	}
	buf, err := hfsys.ReadFile(string(fname))
	if err != nil {
		return nil, nil, err
	}
	return buf, visited, nil
}

func (fsys *filesystem) statFollowSymlink(ctx context.Context, root string, fname path.Path) (hashfs.FileInfo, error) {
	hfsys := fsys.hashfs.FileSystem(ctx, root)
	fi, err := hfsys.Stat(string(fname))
	if err != nil {
		return hashfs.FileInfo{}, err
	}
	return fi.(hashfs.FileInfo), err
}
