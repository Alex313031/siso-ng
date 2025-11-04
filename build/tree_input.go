// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	log "github.com/golang/glog"

	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/reapi/digest"
	"go.chromium.org/build/siso/reapi/merkletree"
)

func treeInputs(ctx context.Context, fn func(context.Context, string) (merkletree.TreeEntry, error), precomputedDirs, dirs []string) []merkletree.TreeEntry {
	treeEntries := make([]merkletree.TreeEntry, len(precomputedDirs)+len(dirs))
	var wg sync.WaitGroup
	wg.Add(len(treeEntries))
	for i, dir := range precomputedDirs {
		go func() {
			defer wg.Done()
			ti, err := fn(ctx, dir)
			if err != nil {
				clog.Warningf(ctx, "treeinput[precomputed] %s: %v", dir, err)
				return
			}
			treeEntries[i] = ti
		}()
	}
	i0 := len(precomputedDirs)
	for i, dir := range dirs {
		go func() {
			defer wg.Done()
			ti, err := fn(ctx, dir)
			if err != nil {
				if log.V(1) {
					clog.Infof(ctx, "treeinput[dir] %s: %v", dir, err)
				}
				return
			}
			treeEntries[i0+i] = ti
		}()
	}
	wg.Wait()
	treeEntries = slices.DeleteFunc(treeEntries, func(e merkletree.TreeEntry) bool {
		return e.Name == ""
	})
	return treeEntries
}

func (b *Builder) resolveSymlinkForInputDeps(ctx context.Context, dir, labelSuffix string, inputDeps map[string][]string) (string, []string, error) {
	fsys := b.hashFS.FileSystem(ctx, b.path.ExecRoot)
	fi, err := fsys.Stat(dir)
	if log.V(1) {
		clog.Infof(ctx, "input deps stat %q: %v", dir, err)
	}
	if err != nil {
		return "", nil, fmt.Errorf("not in input_deps: stat err %s: %w", dir, err)
	}
	// check given dir and visited paths.
	// visited may be out side of exec root, which won't be returned
	// by VisitedPath, so dir is always exec root relative path.
	for _, dir := range append([]string{dir}, fsys.VisitedPaths(fi)...) {
		files, ok := inputDeps[dir+labelSuffix]
		if ok {
			return dir, files, nil
		}
	}
	return "", nil, fmt.Errorf("not in input_deps %s", dir)
}

func (b *Builder) treeInput(ctx context.Context, dir, labelSuffix string, fixFn func(context.Context, []string) []string) (merkletree.TreeEntry, error) {
	if b.reapiclient == nil {
		return merkletree.TreeEntry{}, errors.New("reapi is not configured")
	}
	m := b.graph.InputDeps(ctx)
	if !filepath.IsLocal(dir) {
		// only allowed for dockerChrootPath=. in platform container image
		absdir := filepath.ToSlash(filepath.Join(b.path.ExecRoot, dir))
		if log.V(1) {
			clog.Infof(ctx, "tree dir: %q %q -> %q", b.path.ExecRoot, dir, absdir)
		}
		dir = absdir
	}
	dir, files, err := b.resolveSymlinkForInputDeps(ctx, dir, labelSuffix, m)
	if err != nil {
		return merkletree.TreeEntry{}, err
	}
	st := &subtree{}
	v, _ := b.trees.LoadOrStore(dir, st)
	st = v.(*subtree)
	err = st.init(ctx, b, dir, files, fixFn)
	if err != nil {
		return merkletree.TreeEntry{}, err
	}
	return merkletree.TreeEntry{
		Name:   dir,
		Digest: st.d,
	}, nil
}

type subtree struct {
	once sync.Once
	d    digest.Digest

	mu  sync.Mutex
	err error
}

func (st *subtree) init(ctx context.Context, b *Builder, dir string, files []string, fixFn func(context.Context, []string) []string) error {
	st.once.Do(func() {
		files = b.expandInputs(ctx, files)
		if fixFn != nil {
			files = fixFn(ctx, files)
		}
		var inputs []string
		for _, f := range files {
			if !strings.HasPrefix(f, dir+"/") {
				continue
			}
			inputs = append(inputs, strings.TrimPrefix(f, dir+"/"))
		}
		sort.Strings(inputs)
		rootDir := dir
		if !filepath.IsAbs(rootDir) {
			rootDir = filepath.Join(b.path.ExecRoot, dir)
		}
		clog.Infof(ctx, "tree init root dir: %q (%q %q)", rootDir, b.path.ExecRoot, dir)
		ents, err := b.hashFS.Entries(ctx, rootDir, inputs)
		if err != nil {
			clog.Warningf(ctx, "failed to get subtree entries %s: %v", dir, err)
			st.err = err
			return
		}
		// keep digest in tree in st.ds
		ds := digest.NewStore()
		mt := merkletree.New(ds)
		for _, ent := range ents {
			err := mt.Set(ent)
			if err != nil {
				clog.Warningf(ctx, "failed to set %v: %v", ent, err)
				st.err = err
				return
			}
		}
		st.d, err = mt.Build(ctx)
		if err != nil {
			clog.Warningf(ctx, "failed to build subtree %s: %v", dir, err)
			st.err = err
			return
		}
		// now subtree's digest is ready to use, but
		// file's digests in subtree may not exist in CAS,
		// check subtree's root digest exist in CAS first.
		// If so, we can assume subtree data exist in CAS.
		// Otherwise, we need to upload subtree data to CAS.
		rootDS := digest.NewStore()
		data, ok := ds.Get(st.d)
		if !ok {
			clog.Warningf(ctx, "no tree root digest in store? %s", st.d)
			st.err = fmt.Errorf("no tree root digst in store")
			return
		}
		rootDS.Set(data)
		ds.Delete(st.d)
		missings, err := b.reapiclient.Missing(ctx, []digest.Digest{st.d})
		fullUpload := func(ctx context.Context) {
			started := time.Now()
			// upload non-root digest first
			n, err := b.reapiclient.UploadAll(ctx, ds)
			if err != nil {
				clog.Warningf(ctx, "failed to upload subtree data %s: %v", dir, err)
				st.mu.Lock()
				defer st.mu.Unlock()
				st.err = err
				return
			}
			// upload root digest last.
			m, err := b.reapiclient.UploadAll(ctx, rootDS)
			clog.Infof(ctx, "upload subtree data %s %d+%d in %s: %v", dir, n, m, time.Since(started), err)
			st.mu.Lock()
			defer st.mu.Unlock()
			st.err = err
		}
		if err == nil && len(missings) == 0 {
			clog.Infof(ctx, "subtree data is ready %s %s", dir, st.d)
			go func() {
				// make sure all data are uploaded in background.
				ctx := context.WithoutCancel(ctx)
				fullUpload(ctx)
			}()
			return
		}
		clog.Infof(ctx, "need to upload subtree data %s (missings=%d): %v", dir, len(missings), err)
		fullUpload(ctx)
	})
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.err
}
