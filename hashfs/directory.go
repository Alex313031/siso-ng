// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package hashfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	log "github.com/golang/glog"

	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/reapi/digest"
)

// directory is per-directory entry map to reduce mutex contention.
// TODO: use generics as DirMap<K,V>?
type directory struct {
	// mtime on the local disk when it reads the dir.
	mtime time.Time
	// m is a map of file in a directory's basename to *entry.
	m sync.Map

	// isRoot is true if its' the root directory of the hashfs.
	isRoot bool
}

func (d *directory) String() string {
	if d == nil {
		return "<nil>"
	}
	// better to dump all entries?
	return fmt.Sprintf("&directory{m:%p}", &d.m)
}

// path elements of filepath.
// defer allocation for lookup, but pass elems for store.
type pathElements struct {
	origFname string

	// number of elements processed.
	n int

	// elements processed. maybe empty for lookup
	elems []string
}

// lookup fname in directory and returns an entry of the fname,
// real file name and directory entry that contains the entry,
// and bool indicates file exists or not.
func (d *directory) lookup(ctx context.Context, fname string) (*entry, string, *directory, bool) {
	// expect d.isRoot == true
	for range maxSymlinks {
		e, dir, resolved, ok := d.lookupEntry(ctx, fname)
		if e != nil || dir != nil {
			return e, fname, dir, ok
		}
		if resolved != "" {
			if !d.isRoot {
				clog.Warningf(ctx, "hashfs directory lookup must be called from root directory")
			}
			fname = resolved
			continue
		}
		return nil, fname, nil, false
	}
	return nil, fname, nil, false
}

var missingEntry = func() *entry {
	lready := make(chan bool, 1)
	close(lready)
	return &entry{
		lready: lready,
		err:    fs.ErrNotExist,
	}
}()

func (d *directory) lookupEntry(ctx context.Context, fname string) (*entry, *directory, string, bool) {
	pe := pathElements{
		origFname: fname,
	}
	for fname != "" {
		fname = strings.TrimPrefix(fname, "/")
		elem, rest, ok := strings.Cut(fname, "/")
		if !ok {
			e, ok := d.m.Load(fname)
			if !ok {
				return nil, d, "", false
			}
			return e.(*entry), d, "", true
		}
		fname = rest
		pe.n++
		subdir, target, missing := resolveNextDir(ctx, d, lookupNextDir, pe, elem, fname)
		if subdir == nil {
			if missing {
				return missingEntry, nil, "", true
			}
			return nil, nil, target, false
		}
		d = subdir
	}
	if log.V(1) {
		logOrigFname := pe.origFname
		clog.Infof(ctx, "lookup %s fname empty", logOrigFname)
	}
	return nil, nil, "", false
}

var errRootSymlink = errors.New("symlink resolved from root")

func (d *directory) store(ctx context.Context, fname string, e *entry) (*entry, error) {
	for range maxSymlinks {
		ent, resolved, err := d.storeEntry(ctx, fname, e)
		if resolved != "" {
			if !d.isRoot {
				if filepath.IsAbs(resolved) {
					return nil, fmt.Errorf("root symlink %s: %w", resolved, errRootSymlink)
				}
				if !filepath.IsLocal(resolved) {
					return nil, fmt.Errorf("non local symlink %s: %w", resolved, errRootSymlink)
				}
			}
			fname = resolved
			continue
		}
		return ent, err
	}
	return nil, fmt.Errorf("store %s: %w", fname, syscall.ELOOP)
}

type storeRaceError struct {
	fname     string
	prevEntry *entry
	entry     *entry
	curEntry  any // *entry
	exists    bool
}

func (e storeRaceError) Error() string {
	return fmt.Sprintf("store race %s: %p -> %p -> %p %t", e.fname, e.prevEntry, e.entry, e.curEntry, e.exists)
}

// shouldKeep checks whether the existing entry ee can be kept
// when a new entry e is stored. It also inherits cmdhash/action
// from ee when appropriate and logs changes.
// Returns (entry to use, keep bool).
// If keep is true, the returned entry should be used as-is (no swap needed).
func shouldKeep(ctx context.Context, origFname string, ee, e *entry) (*entry, bool) {
	if e == ee {
		// if storing entry `e` is the same as stored entry `ee`, no need to update.
		return e, true
	}
	eed := ee.digest()
	// old entry has cmdhash, but new entry has no cmdhash&action (not by Update*).
	if len(ee.cmdhash) > 0 && len(e.cmdhash) == 0 && e.action.IsZero() {
		// keep cmdhash and action
		e.cmdhash = ee.cmdhash
		e.edgehash = ee.edgehash
		e.action = ee.action
		e.local = ee.local
	}
	cmdchanged := !bytes.Equal(ee.cmdhash, e.cmdhash)
	edgechanged := !bytes.Equal(ee.edgehash, e.edgehash)
	actionchanged := ee.action != e.action
	if e.isSymlink() && ee.target != e.target {
		if log.V(1) {
			// lv is to reduce the number of memory allocations when variables are escaping to heap.
			lv := struct {
				origFname         string
				cmdchanged        bool
				edgechanged       bool
				eetarget, etarget string
			}{origFname, cmdchanged, edgechanged, ee.target, e.target}
			clog.Infof(ctx, "store %s: cmdchange:%t edgechanged:%t s:%q to %q", lv.origFname, lv.cmdchanged, lv.edgechanged, lv.eetarget, lv.etarget)
		}
	} else if !e.d.IsZero() && eed != e.d && eed.SizeBytes != 0 && e.d.SizeBytes != 0 {
		if log.V(1) {
			// don't log nil to digest of empty file (size=0)
			// lv is to reduce the number of memory allocations when variables are escaping to heap.
			lv := struct {
				origFname   string
				cmdchanged  bool
				edgechanged bool
				eed, ed     digest.Digest
			}{origFname, cmdchanged, edgechanged, eed, e.d}
			clog.Infof(ctx, "store %s: cmdchange:%t edgechanged:%t d:%v to %v", lv.origFname, lv.cmdchanged, lv.edgechanged, lv.eed, lv.ed)
		}
	} else if cmdchanged || edgechanged || actionchanged {
		if log.V(1) {
			// lv is to reduce the number of memory allocations when variables are escaping to heap.
			lv := struct {
				origFname     string
				cmdchanged    bool
				edgechanged   bool
				actionchanged bool
			}{origFname, cmdchanged, edgechanged, actionchanged}
			clog.Infof(ctx, "store %s: cmdchange:%t edgechanged:%t actionchange:%t", lv.origFname, lv.cmdchanged, lv.edgechanged, lv.actionchanged)
		}
	} else if ee.target == e.target && ee.size == e.size && ee.mode == e.mode && (e.d.IsZero() || eed == e.d) {
		// no change?

		// if e.d is zero, it may be new local entry
		// and ee.d has been calculated

		// update mtime and updatedTime.
		ee.mu.Lock()
		ee.mtimeUpdated = !ee.mtime.Equal(e.mtime)
		ee.mtime = e.mtime
		if ee.updatedTime.Before(e.updatedTime) {
			ee.updatedTime = e.updatedTime
		}
		ee.isChanged = e.isChanged
		ee.mu.Unlock()
		if log.V(1) {
			// lv is to reduce the number of memory allocations when variables are escaping to heap.
			lv := struct {
				origFname   string
				mtime       time.Time
				updatedTime time.Time
			}{origFname, ee.getMtime(), ee.getUpdatedTime()}
			clog.Infof(ctx, "store %s: mtime updated %v %v", lv.origFname, lv.mtime, lv.updatedTime)
		}
		return ee, true
	} else if ee.getDir() != nil && e.getDir() != nil {
		// ok if mkdir with the no cmdhash or same cmdhash.
		if (len(ee.cmdhash) > 0 && len(e.cmdhash) == 0) || bytes.Equal(ee.cmdhash, e.cmdhash) {
			return ee, true
		}
	}
	// e should replace ee.
	return e, false
}

func (d *directory) storeEntry(ctx context.Context, fname string, e *entry) (*entry, string, error) {
	pe := pathElements{
		origFname: fname,
		elems:     make([]string, 0, strings.Count(fname, "/")+1),
	}
	if log.V(8) {
		logOrigFname := pe.origFname
		clog.Infof(ctx, "store %s %v", logOrigFname, e)
	}
	if strings.HasPrefix(fname, "/") {
		pe.elems = append(pe.elems, "/")
	}
	nextDir := func(ctx context.Context, d *directory, pe pathElements, elem string) (*directory, string, bool) {
		return storeNextDir(ctx, d, pe, elem, errors.Is(e.err, fs.ErrNotExist))
	}
	for fname != "" {
		fname = strings.TrimPrefix(fname, "/")
		elem, rest, ok := strings.Cut(fname, "/")
		if !ok {
			v, loaded := d.m.LoadOrStore(fname, e)
			if !loaded {
				if log.V(8) {
					// lv is to reduce the number of memory allocations when variables are escaping to heap.
					lv := struct {
						origFname string
						d         *directory
						fname     string
					}{pe.origFname, d, fname}
					clog.Infof(ctx, "store %s -> %p %s", lv.origFname, lv.d, lv.fname)
				}
				return e, "", nil
			}
			ee := v.(*entry)
			result, keep := shouldKeep(ctx, pe.origFname, ee, e)
			if keep {
				return result, "", nil
			}

			// e should be new value for fname.
			swapped := d.m.CompareAndSwap(fname, ee, e)
			if !swapped {
				// store race?
				v, ok := d.m.Load(fname)
				return nil, "", storeRaceError{
					fname:     fname,
					prevEntry: ee,
					entry:     e,
					curEntry:  v,
					exists:    ok,
				}
			}
			// e is stored for fname
			return e, "", nil
		}
		pe.n++
		pe.elems = append(pe.elems, elem)
		fname = rest
		subdir, resolved, missing := resolveNextDir(ctx, d, nextDir, pe, elem, fname)
		if subdir == nil {
			if missing {
				return missingEntry, "", nil
			}
			if resolved != "" {
				return nil, resolved, nil
			}
			return nil, "", fmt.Errorf("store resolve next dir %s failed: %s", elem, pe.origFname)
		}
		d = subdir
	}
	errOrigFname := pe.origFname
	return nil, "", fmt.Errorf("bad fname? %q", errOrigFname)
}

// resolveNextDir resolves a dir named `elem` by calling `next`.
// `next` will return *directory if `elem` entry is directory.
// `next` will return string if `elem` entry is symlink.
// `next` will return (nil, "", true) if `elem` is recorded as not found.
// `next` will return (nil, "", false) if `elem` is not recorded.
// resolveNextDir returns directory if resolved `elem` is directory.
// resolveNextDir returns resolved path name as string if resolved `elem` is symlink.
// resolveNextDir returns true if the next dir entry is recorded as not found.
// resolveNextDir returns (nil, "", false) if `elem` is not recorded.
func resolveNextDir(ctx context.Context, d *directory, next func(context.Context, *directory, pathElements, string) (*directory, string, bool), pe pathElements, elem, rest string) (*directory, string, bool) {
	nextDir, target, missing := next(ctx, d, pe, elem)
	if target != "" {
		if len(pe.elems) != pe.n {
			// reconstruct elems for lookup
			pe.elems = make([]string, 0, pe.n+1)
			if strings.HasPrefix(pe.origFname, "/") {
				pe.elems = append(pe.elems, "/")
			}
			s := pe.origFname
			for range pe.n - 1 {
				s = strings.TrimPrefix(s, "/")
				elem, rest, _ := strings.Cut(s, "/")
				pe.elems = append(pe.elems, elem)
				s = rest
			}
			if runtime.GOOS == "windows" && !strings.HasSuffix(pe.elems[0], `\`) {
				// elems[0] is drive letter. e.g. "C:"
				pe.elems[0] += `\`
			}
			pe.elems = append(pe.elems, elem)
		}
		if filepath.IsAbs(target) {
			resolved := filepath.ToSlash(filepath.Join(target, rest))
			if log.V(1) {
				clog.Infof(ctx, "resolve symlink -> %s", resolved)
			}
			return nil, resolved, false
		}
		pe.elems[len(pe.elems)-1] = target
		pe.elems = append(pe.elems, rest)
		resolved := filepath.ToSlash(filepath.Join(pe.elems...))
		if log.V(1) {
			clog.Infof(ctx, "resolve symlink -> %s", resolved)
		}
		return nil, resolved, false
	}
	if nextDir != nil {
		return nextDir, "", false
	}
	if nextDir == nil && target == "" && missing {
		return nil, "", true
	}
	if log.V(1) {
		clog.Warningf(ctx, "resolve next %q not recorded yet for %q", elem, pe.origFname)
	}
	return nil, "", false
}

// next for lookup case.
func lookupNextDir(ctx context.Context, d *directory, pe pathElements, elem string) (*directory, string, bool) {
	v, ok := d.m.Load(elem)
	if !ok {
		return nil, "", false
	}
	dent := v.(*entry)
	if dent != nil {
		if errors.Is(dent.err, fs.ErrNotExist) {
			return nil, "", true
		}
		if dent.err != nil {
			return nil, "", false
		}
		target := dent.target
		subdir := dent.getDir()
		if subdir == nil && target == "" {
			return nil, "", false
		}
		return subdir, target, false
	}
	return nil, "", false
}

// next for store case.
// storeNextDir will create next dir entry if needed.
// storeNextDir will not create next dir if notExistEntry is true and entry has ErrNotExist.
func storeNextDir(ctx context.Context, d *directory, pe pathElements, elem string, notExistEntry bool) (*directory, string, bool) {
	v, ok := d.m.Load(elem)
	if ok {
		dent := v.(*entry)
		if notExistEntry && dent != nil && errors.Is(dent.err, fs.ErrNotExist) {
			return nil, "", true
		}
		if dent != nil && dent.err == nil {
			target := dent.target
			subdir := dent.getDir()
			if log.V(9) {
				// lv is to reduce the number of memory allocations when variables are escaping to heap.
				lv := struct {
					origFname, elem string
					d               *directory
					dent            *entry
				}{pe.origFname, elem, d, dent}
				clog.Infof(ctx, "store %s subdir0 %s -> %s (%v)", lv.origFname, lv.elem, lv.d, lv.dent)
			}
			if subdir == nil && target == "" {
				if log.V(9) {
					clog.Infof(ctx, "store %s no dir, no symlink", pe.origFname)
				}
				return nil, "", false
			}
			return subdir, target, false
		}
		deleted := d.m.CompareAndDelete(elem, dent)
		if log.V(9) {
			// lv is to reduce the number of memory allocations when variables are escaping to heap.
			lv := struct {
				origFname, elem string
				deleted         bool
			}{pe.origFname, elem, deleted}
			clog.Infof(ctx, "store %s delete missing %s to create dir deleted: %t", lv.origFname, lv.elem, lv.deleted)
		}
	}
	// create intermediate dir of elem.
	mtime := time.Now()
	if runtime.GOOS == "windows" && !strings.HasSuffix(pe.elems[0], `\`) {
		// elems[0] is drive letter. e.g "c:"
		pe.elems[0] += `\`
	}
	fullname := filepath.Join(pe.elems...)
	dfi, err := os.Lstat(fullname)
	if err == nil {
		mtime = dfi.ModTime()
		switch {
		case dfi.IsDir():
		case dfi.Mode().Type() == fs.ModeSymlink:
			target, err := os.Readlink(fullname)
			if err != nil {
				clog.Warningf(ctx, "readlink %s: %v", fullname, err)
				return nil, "", false
			}
			lready := make(chan bool, 1)
			lready <- true
			newDent := &entry{
				lready: lready,
				mode:   0644 | fs.ModeSymlink,
				mtime:  mtime,
				target: target,
			}
			dent := newDent
			v, ok := d.m.LoadOrStore(elem, dent)
			if ok {
				dent = v.(*entry)
			}
			if dent.mode != newDent.mode || dent.target != newDent.target {
				clog.Warningf(ctx, "store %s symlink dir: race? store %s %s / loaded %s %s", pe.origFname, newDent.mode, newDent.target, dent.mode, dent.target)
			}
			return nil, target, false
		default:
			clog.Warningf(ctx, "unexpected mode %s: %s", fullname, dfi.Mode().Type())
			return nil, "", false
		}
	}
	if !notExistEntry {
		err = nil
	}
	lready := make(chan bool, 1)
	lready <- true
	newDent := &entry{
		lready: lready,
		err:    err,
		mode:   0o644 | fs.ModeDir,
		mtime:  mtime,
	}
	if newDent.err == nil {
		// don't set directory.mtime for intermediate dir.
		// mtime will be updated by updateDir
		// when all dirents have been loaded.
		newDent.directory = &directory{}
	}
	// TODO: store newDent.err=fs.ErrNotExist when notExistEntry and err=fs.ErrNotExist?
	var dent *entry
	for {
		dent = newDent
		v, loaded := d.m.LoadOrStore(elem, dent)
		if !loaded {
			break
		}
		dent = v.(*entry)
		if dent != nil && dent.err != nil && newDent.err == nil {
			// A concurrent lstat may have cached an ErrNotExist
			// here (e.g. one filegroup evaluates a missing
			// 'sysroot', while another concurrently evaluates
			// 'sysroot/usr/lib'). We need to upgrade it to a
			// virtual directory to hold nested entries.
			if d.m.CompareAndSwap(elem, dent, newDent) {
				dent = newDent
				break
			}
			continue
		}
		break
	}
	var target string
	if dent != nil {
		target = dent.target
	}
	subdir := dent.getDir()
	if log.V(9) {
		// lv is to reduce the number of memory allocations when variables are escaping to heap.
		lv := struct {
			origFname, elem string
			subdir          *directory
			dent            *entry
		}{pe.origFname, elem, subdir, dent}
		clog.Infof(ctx, "store %s subdir1 %s -> %s (%v)", lv.origFname, lv.elem, lv.subdir, lv.dent)
	}
	d = subdir
	if d == nil && target == "" {
		if notExistEntry && errors.Is(err, fs.ErrNotExist) {
			return nil, "", true
		}
		clog.Warningf(ctx, "store %s no dir, no symlink", pe.origFname)
		return nil, "", false
	}
	return d, target, false
}

func (d *directory) delete(ctx context.Context, fname string) {
	_, _, dir, ok := d.lookup(ctx, fname)
	if !ok || dir == nil {
		clog.Warningf(ctx, "delete %q: lookup filed", fname)
		return
	}
	name := filepath.Base(fname)
	dir.m.Delete(name)
}
