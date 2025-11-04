// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package hashfs provides a filesystem with digest hash.
package hashfs

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	log "github.com/golang/glog"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"go.chromium.org/build/siso/hashfs/osfs"
	pb "go.chromium.org/build/siso/hashfs/proto"
	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/o11y/trace"
	"go.chromium.org/build/siso/reapi/digest"
	"go.chromium.org/build/siso/reapi/merkletree"
	"go.chromium.org/build/siso/reapi/retry"
	"go.chromium.org/build/siso/runtimex"
	"go.chromium.org/build/siso/sync/semaphore"
)

// Linux imposes a limit of at most 40 symlinks in any one path lookup.
// see: https://lwn.net/Articles/650786/
const maxSymlinks = 40

// ForgetMissingsSemaphore is a semaphore to control concurrent ForgetMissings.
// os.Lstat in ForgetMissings would create lots of thread. b/325565625
var ForgetMissingsSemaphore = semaphore.New("fs-forget", runtimex.NumCPU()*2)

// FlushSemaphore is a semaphore to control concurrent flushes.
var FlushSemaphore = semaphore.New("fs-flush", runtimex.NumCPU()*8)

func isExecutable(fi fs.FileInfo, fname string, m map[string]bool) bool {
	if fi.Mode()&0111 != 0 {
		return true
	}
	return m[fname]
}

// NotifyFunc is the type of the function to notify the filesystem changes.
type NotifyFunc func(context.Context, *FileInfo)

// HashFS is a filesystem for digest hash.
type HashFS struct {
	opt       Option
	directory *directory

	notifies []NotifyFunc

	// OS wraps of OS I/O operations in the HashFS.
	OS *osfs.OSFS

	digester digester

	// loadErr keeps load error.
	loadErr error

	// clean if loaded state is matched with local disk's state.
	clean atomic.Bool

	// buildTargets is build targets stored in state file.
	// nil vs []string{} differs.
	// nil is not set (last build failed).
	// []string{} is set (last build succeeded without explicit target requested).
	buildTargets []string

	// loaded if state is loaded.
	loaded atomic.Bool

	// holds generated files (full path) in previous builds.
	previouslyGeneratedFiles []string

	// holds tainted files
	taintedFiles []string

	executables map[string]bool

	// writer for updated entries journal.
	journal journalWriter

	// trigger for SetState background goroutine finish.
	setStateCh chan error

	// records missing outputs to make hashfs non-clean when missing
	// outputs exists, so trigger rebuilds b/374179435
	missingOutputs sync.Map
}

// New creates a HashFS.
func New(ctx context.Context, opt Option) (*HashFS, error) {
	if opt.OutputLocal == nil {
		opt.OutputLocal = func(context.Context, string) bool { return false }
	}
	if opt.Ignore == nil {
		opt.Ignore = func(context.Context, string) bool { return false }
	}
	if opt.DataSource == nil {
		opt.DataSource = noDataSource{}
	}
	fsys := &HashFS{
		opt:       opt,
		directory: &directory{isRoot: true},
		OS:        osfs.New(ctx, "fs", opt.OSFSOption),

		digester: digester{
			q:    make(chan digestReq, 1000),
			quit: make(chan struct{}),
			done: make(chan struct{}),
		},
	}
	if opt.StateFile != "" {
		start := time.Now()
		journalFile := opt.StateFile + ".journal"

		fstate, err := Load(ctx, opt)
		if errors.Is(err, fs.ErrNotExist) {
			clog.Infof(ctx, "missing fs state. new build? %v", err)
			// missing .siso_fs_state is not considered as load error.
			fstate = &pb.State{}
		} else if err != nil {
			clog.Warningf(ctx, "Failed to load fs state from %s: %v", opt.StateFile, err)
			fsys.loadErr = err
			fstate = &pb.State{}
		} else {
			clog.Infof(ctx, "Load fs state from %s: %s", opt.StateFile, time.Since(start))
		}

		// for corrupted fs state, we also don't use journal,
		// as we don't have base state for the journal.
		if fsys.loadErr == nil {
			// if previous build didn't finish properly, journal file
			// is not removed, so recover last build updates from the journal.
			reconciled := loadJournal(ctx, journalFile, fstate)
			if err := fsys.SetState(ctx, fstate); err != nil {
				return nil, err
			}
			if reconciled {
				// save fstate to make it base state for next journaling.
				err := Save(ctx, fstate, opt)
				if err != nil {
					clog.Errorf(ctx, "Failed to save reconciled fs state in %s: %v", opt.StateFile, err)
				}
			}
		}
		err = os.Remove(journalFile)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			clog.Warningf(ctx, "Failed to remove journal: %v", err)
		}

		f, err := os.Create(journalFile)
		if err != nil {
			clog.Warningf(ctx, "Failed to create fs state journal: %v", err)
		} else {
			fsys.journal.w = f
		}
	}
	go fsys.digester.start()
	return fsys, nil
}

// LoadErr returns load error.
func (hfs *HashFS) LoadErr() error {
	return hfs.loadErr
}

// WaitReady waits fs state is updated in memory.
func (hfs *HashFS) WaitReady(ctx context.Context) error {
	if hfs.setStateCh == nil {
		return nil
	}
	started := time.Now()
	select {
	case <-ctx.Done():
		clog.Errorf(ctx, "hashfs does not become ready %s: %v", time.Since(started), context.Cause(ctx))
		return context.Cause(ctx)

	case err := <-hfs.setStateCh:
		hfs.setStateCh = nil
		if err != nil {
			clog.Errorf(ctx, "hashfs does not become ready %s: %v", time.Since(started), err)
			return err
		}
		clog.Infof(ctx, "hashfs becomes ready: %v", time.Since(started))
	}
	return nil
}

// Notify causes the hashfs to relay filesystem motification to f.
func (hfs *HashFS) Notify(f NotifyFunc) {
	hfs.notifies = append(hfs.notifies, f)
}

// SetExecutables sets a map of full paths for files to be
// considered as executable, even if it is not executable on local disk.
func (hfs *HashFS) SetExecutables(ctx context.Context, m map[string]bool) {
	hfs.executables = m
	for fname := range m {
		e, _, _, ok := hfs.directory.lookup(ctx, fname)
		if ok {
			clog.Infof(ctx, "set executable bit on %q", fname)
			e.mode |= 0111
		}
	}
}

// SetBuildTargets sets build targets.
func (hfs *HashFS) SetBuildTargets(ctx context.Context, buildTargets []string, success bool) {
	if !success {
		hfs.buildTargets = nil
		clog.Infof(ctx, "set no build targets")
		return
	}
	hfs.buildTargets = make([]string, len(buildTargets))
	copy(hfs.buildTargets, buildTargets)
	clog.Infof(ctx, "set build targets=%q", hfs.buildTargets)
}

// Close closes the HashFS.
// Persists current state in opt.StateFile.
func (hfs *HashFS) Close(ctx context.Context) error {
	clog.Infof(ctx, "fs close")
	hfs.digester.stop(ctx)
	if hfs.opt.StateFile == "" {
		return nil
	}
	err := hfs.journal.Close()
	if err != nil {
		clog.Warningf(ctx, "Failed to close journal %v", err)
	}
	clog.Infof(ctx, "close journal")
	if hfs.clean.Load() || !hfs.loaded.Load() || len(hfs.taintedFiles) > 0 {
		// don't update fs state when there are tainted files.
		clog.Warningf(ctx, "not save state clean=%t loaded=%t tainted:%d", hfs.clean.Load(), hfs.loaded.Load(), len(hfs.taintedFiles))
		return nil
	}
	err = Save(ctx, hfs.State(ctx), hfs.opt)
	if err != nil {
		clog.Errorf(ctx, "Failed to save fs state in %s: %v", hfs.opt.StateFile, err)
		if rerr := os.Remove(hfs.opt.StateFile); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
			clog.Errorf(ctx, "Failed to remove stale fs state %s: %v", hfs.opt.StateFile, err)
		}
		return err
	}
	clog.Infof(ctx, "Saved fs state in %s", hfs.opt.StateFile)
	return nil
}

// IsClean returns whether hashfs is clean for buildTargets (i.e. sync with local disk).
func (hfs *HashFS) IsClean(buildTargets []string) bool {
	if !hfs.clean.Load() {
		return false
	}
	// we distinguish nil vs []string{}.
	if hfs.buildTargets == nil {
		return false
	}
	return slices.Equal(hfs.buildTargets, buildTargets)
}

// PreviouslyGeneratedFiles returns a list of generated files
// (i.e. has cmdhash) in the previous builds.
// It will reset internal data, so next call will return nil
func (hfs *HashFS) PreviouslyGeneratedFiles() []string {
	p := hfs.previouslyGeneratedFiles
	hfs.previouslyGeneratedFiles = nil
	return p
}

// TaintedFiles returns a list of manually modified generated files.
func (hfs *HashFS) TaintedFiles() []string {
	return hfs.taintedFiles
}

// AddMissingOutput adds a missing output.
func (hfs *HashFS) AddMissingOutput(ctx context.Context, root, fname string) {
	hfs.missingOutputs.Store(filepath.ToSlash(filepath.Join(root, fname)), true)
}

// FileSystem returns FileSystem interface at dir.
func (hfs *HashFS) FileSystem(ctx context.Context, dir string) FileSystem {
	return FileSystem{
		hashFS: hfs,
		ctx:    ctx,
		dir:    dir,
	}
}

// DataSource returns DataSource of the HashFS.
func (hfs *HashFS) DataSource() DataSource {
	return hfs.opt.DataSource
}

// OnCog returns whether it is on Cog or not.
func (hfs *HashFS) OnCog() bool {
	return hfs.opt.CogFS != nil
}

func needPathClean(names ...string) bool {
	for _, name := range names {
		// even on windows, we use /-path in hashfs.
		if strings.Contains(name, `\`) {
			return true
		}
		if strings.Contains(name, "//") {
			return true
		}
		i := strings.IndexByte(name, '.')
		if i < 0 {
			continue
		}
		name = name[i:]
		if strings.HasPrefix(name, "./") || strings.HasPrefix(name, "../") {
			return true
		}
	}
	return false
}

func makeFullpath(root, fname string) string {
	if filepath.IsAbs(fname) {
		return filepath.ToSlash(fname)
	}
	return filepath.ToSlash(filepath.Join(root, fname))
}

func (hfs *HashFS) dirLookup(ctx context.Context, root, fname string) (*entry, string, *directory, bool) {
	if filepath.IsAbs(fname) {
		return hfs.directory.lookup(ctx, filepath.ToSlash(fname))
	}
	if needPathClean(root, fname) {
		return hfs.directory.lookup(ctx, filepath.ToSlash(filepath.Join(root, fname)))
	}
	e, _, _, ok := hfs.directory.lookup(ctx, root)
	if !ok {
		return nil, fname, nil, false
	}
	if e.directory == nil {
		return nil, fname, nil, false
	}
	e, dir, resolved, ok := e.directory.lookupEntry(ctx, fname)
	if ok {
		return e, fname, dir, true
	}
	if resolved != "" {
		resolvedName := resolved
		if !filepath.IsAbs(resolved) {
			resolvedName = filepath.ToSlash(filepath.Join(root, resolved))
		}
		return hfs.directory.lookup(ctx, resolvedName)
	}
	return nil, fname, nil, false
}

func (hfs *HashFS) dirStoreAndNotify(ctx context.Context, fullname string, e *entry) error {
	ee, err := hfs.directory.store(ctx, fullname, e)
	if err != nil {
		return err
	}
	hfs.digester.lazyCompute(ctx, fullname, ee)
	for _, f := range hfs.notifies {
		f(ctx, &FileInfo{fname: fullname, e: ee})
	}
	return nil
}

// Stat returns a FileInfo at root/fname.
func (hfs *HashFS) Stat(ctx context.Context, root, fname string) (FileInfo, error) {
	return hfs.stat(ctx, root, fname, true)
}

func (hfs *HashFS) stat(ctx context.Context, root, fname string, needCompute bool) (FileInfo, error) {
	if log.V(1) {
		clog.Infof(ctx, "stat @%s %s", root, fname)
	}
	e, fname, dir, ok := hfs.dirLookup(ctx, root, fname)
	if log.V(1) {
		clog.Infof(ctx, "stat @%s -> %s", root, fname)
	}
	if ok {
		if e.err != nil {
			return FileInfo{}, e.err
		}
		if e.directory != nil {
			// directory's mtime has been updated locally
			// where hashfs doesn't know. e.g. add new file
			// in the directory by local run.
			fullname := makeFullpath(root, fname)
			lfi, err := hfs.OS.Lstat(ctx, fullname)
			switch {
			case errors.Is(err, fs.ErrNotExist):
				// virtually created dir in hashfs,
				// so no need to update mtime.
				clog.Infof(ctx, "stat hashfs dir %s. doesn't exist in local", fullname)
				return FileInfo{}, err
			case err != nil:
				clog.Warningf(ctx, "unexpected dir stat fail %s: %v", fullname, err)
				return FileInfo{}, err
			default:
				mtime := lfi.ModTime()
				// adjust for clock stepback by NTP
				err = waitUntilModTime(ctx, fullname, mtime)
				if err != nil {
					return FileInfo{}, err
				}
				e.mu.Lock()
				e.mtime = mtime
				if e.updatedTime.Before(mtime) {
					// if no cmdhash, it may not be generated by any step, so keep updated_time with mtime silently.
					if len(e.cmdhash) > 0 {
						clog.Warningf(ctx, "unexpected update dir mtime %s %v; updated_time=%v", fullname, mtime, e.updatedTime)
					}
					e.updatedTime = mtime
				}
				e.mu.Unlock()
			}
		}
		return FileInfo{root: root, fname: fname, e: e}, nil
	}
	fullname := makeFullpath(root, fname)
	e = newLocalEntry()
	e.init(ctx, fullname, hfs.executables, hfs.OS)
	if log.V(1) {
		clog.Infof(ctx, "stat new entry %s %s", fullname, e)
	}
	if errors.Is(e.err, context.Canceled) {
		return FileInfo{}, e.err
	}
	var err error
	if dir != nil {
		e, err = dir.store(ctx, filepath.Base(fullname), e)
		if errors.Is(err, errRootSymlink) {
			e, err = hfs.directory.store(ctx, fullname, e)
		}
	} else {
		e, err = hfs.directory.store(ctx, fullname, e)
	}
	if err != nil {
		clog.Warningf(ctx, "failed to store %s %s in %s: %v", fullname, e, dir, err)
		return FileInfo{}, err
	}
	if e.err != nil {
		return FileInfo{}, e.err
	}
	if needCompute {
		hfs.digester.lazyCompute(ctx, fullname, e)
	}
	return FileInfo{root: root, fname: fname, e: e}, nil
}

// SymlinkError is an error when reading symlink file.
type SymlinkError struct {
	Path   string
	Target string
}

func (e SymlinkError) Error() string {
	return fmt.Sprintf("reading symlink %q: target=%q", e.Path, e.Target)
}

// ReadDir returns directory entries of root/name.
func (hfs *HashFS) ReadDir(ctx context.Context, root, name string) (dents []DirEntry, err error) {
	ctx, span := trace.NewSpan(ctx, "read-dir")
	defer span.Close(nil)
	if log.V(1) {
		clog.Infof(ctx, "readdir @%s %s", root, name)
		defer func() {
			clog.Infof(ctx, "readdir @%s %s -> %d %v", root, name, len(dents), err)
		}()
	}
	dname := makeFullpath(root, name)
	e, dname, _, ok := hfs.directory.lookup(ctx, dname)
	if !ok {
		e = newLocalEntry()
		e.init(ctx, dname, hfs.executables, hfs.OS)
		if errors.Is(e.err, context.Canceled) {
			return nil, e.err
		}
		var err error
		e, err = hfs.directory.store(ctx, dname, e)
		if err != nil {
			clog.Warningf(ctx, "failed to store %s %s: %v", dname, e, err)
			return nil, err
		}
		clog.Infof(ctx, "stat new dir entry %s %s", dname, e)
	}
	err = e.err
	if err != nil {
		return nil, fmt.Errorf("read dir %s: %w", dname, err)
	}
	if e.target != "" {
		relDname, err := filepath.Rel(root, dname)
		if err != nil || !filepath.IsLocal(relDname) {
			clog.Warningf(ctx, "read dir: symlink rel root %q: %v", dname, err)
			return nil, SymlinkError{Path: dname, Target: e.target}
		}
		return nil, SymlinkError{Path: relDname, Target: e.target}
	}
	if e.directory == nil {
		return nil, fmt.Errorf("read dir %s: not dir: %w", dname, os.ErrPermission)
	}
	// TODO(ukai): fix race in updateDir -> store.
	names := e.updateDir(ctx, hfs, dname)
	if log.V(1) {
		clog.Infof(ctx, "update-dir %s -> %d", dname, len(names))
	}
	var ents []DirEntry
	e.directory.m.Range(func(k, v any) bool {
		name := k.(string)
		ee := v.(*entry)
		if ee.err != nil {
			return true
		}
		ents = append(ents, DirEntry{
			fi: FileInfo{
				root:  root,
				fname: filepath.ToSlash(filepath.Join(dname, name)),
				e:     ee,
			},
		})
		return true
	})
	return ents, nil
}

// ReadFile reads a contents of root/fname.
func (hfs *HashFS) ReadFile(ctx context.Context, root, fname string) ([]byte, error) {
	ctx, span := trace.NewSpan(ctx, "read-file")
	defer span.Close(nil)
	if log.V(1) {
		clog.Infof(ctx, "readfile @%s %s", root, fname)
	}
	fname = makeFullpath(root, fname)
	span.SetAttr("fname", fname)
	e, fname, _, ok := hfs.directory.lookup(ctx, fname)
	if !ok {
		e = newLocalEntry()
		e.init(ctx, fname, hfs.executables, hfs.OS)
		if errors.Is(e.err, context.Canceled) {
			return nil, e.err
		}
		var err error
		e, err = hfs.directory.store(ctx, fname, e)
		if err != nil {
			clog.Warningf(ctx, "failed to store %s %s: %v", fname, e, err)
			return nil, err
		}
		clog.Infof(ctx, "stat new entry %s %s", fname, e)
	}
	err := e.err
	if err != nil {
		return nil, fmt.Errorf("read file %s: %w", fname, err)
	}
	if len(e.buf) > 0 {
		return e.buf, nil
	}
	e.mu.RLock()
	ed := e.d
	e.mu.RUnlock()
	// If digest is known and the file is not flushed to disk, read from CAS.
	// Otherwise, read from local disk, which will also compute the digest if unknown.
	if !ed.IsZero() {
		// digest is known.
		lfi, err := hfs.OS.Lstat(ctx, fname)
		// check it is already flushed to disk or not.
		if err != nil || !e.getMtime().Equal(lfi.ModTime()) || ed.SizeBytes != lfi.Size() {
			// not yet flushed, read from CAS
			buf, err := digest.DataToBytes(ctx, digest.NewData(e.src, ed))
			if log.V(1) {
				clog.Infof(ctx, "readfile(%s) %s: %v", ed, fname, err)
			}
			return buf, err
		}
		// already flushed. reading from local disk is faster.
	}
	if e.target != "" {
		relFname, err := filepath.Rel(root, fname)
		if err != nil || !filepath.IsLocal(relFname) {
			clog.Warningf(ctx, "readfile: symlink rel root %q: %v", fname, err)
			return nil, SymlinkError{Path: fname, Target: e.target}
		}
		return nil, SymlinkError{Path: relFname, Target: e.target}
	}
	if e.src == nil {
		return nil, fmt.Errorf("readfile %s: no src", fname)
	}
	src := hfs.OS.FileSource(fname, -1)
	rd, err := src.Open(ctx)
	if err != nil {
		return nil, fmt.Errorf("readfile %s: %w", fname, err)
	}
	defer rd.Close()
	buf, err := io.ReadAll(rd)
	if log.V(1) {
		clog.Infof(ctx, "readfile(disk) %s: %v", fname, err)
	}
	// async compute digest.
	hfs.digester.lazyCompute(ctx, fname, e)
	return buf, err
}

// WriteFile writes a contents in root/fname with mtime and cmdhash, edgehash.
func (hfs *HashFS) WriteFile(ctx context.Context, root, fname string, b []byte, isExecutable bool, mtime time.Time, cmdhash, edgehash []byte) error {
	ctx, span := trace.NewSpan(ctx, "write-file")
	defer span.Close(nil)
	if log.V(1) {
		clog.Infof(ctx, "writefile @%s %s x:%t mtime:%s", root, fname, isExecutable, mtime)
	}
	hfs.clean.Store(false)
	data := digest.FromBytes(fname, b)
	fname = makeFullpath(root, fname)
	span.SetAttr("fname", fname)
	lready := make(chan bool, 1)
	lready <- true
	mode := fs.FileMode(0644)
	if isExecutable {
		mode |= 0111
	}
	e := &entry{
		lready:      lready,
		size:        data.Digest().SizeBytes,
		mtime:       mtime,
		mode:        mode,
		src:         data,
		d:           data.Digest(),
		buf:         b,
		cmdhash:     cmdhash,
		edgehash:    edgehash,
		updatedTime: time.Now(),
		isChanged:   true,
	}
	err := hfs.dirStoreAndNotify(ctx, fname, e)
	clog.Infof(ctx, "writefile %s x:%t mtime:%s: %v", fname, isExecutable, mtime, err)
	if err != nil {
		return err
	}
	hfs.journalEntry(ctx, fname, e)
	return nil
}

// Symlink creates a symlink to target at root/linkpath with mtime and cmdhash, edgehash.
func (hfs *HashFS) Symlink(ctx context.Context, root, target, linkpath string, mtime time.Time, cmdhash, edgehash []byte) error {
	if log.V(1) {
		clog.Infof(ctx, "symlink @%s %s -> %s", root, linkpath, target)
	}
	hfs.clean.Store(false)
	linkfname := makeFullpath(root, linkpath)
	lready := make(chan bool, 1)
	lready <- true
	e := &entry{
		lready:      lready,
		mtime:       mtime,
		mode:        0644 | fs.ModeSymlink,
		cmdhash:     cmdhash,
		edgehash:    edgehash,
		target:      target,
		updatedTime: time.Now(),
		isChanged:   true,
	}
	err := hfs.dirStoreAndNotify(ctx, linkfname, e)
	clog.Infof(ctx, "symlink @%s %s -> %s: %v", root, linkpath, target, err)
	if err != nil {
		return err
	}
	hfs.journalEntry(ctx, linkfname, e)
	return nil
}

// Copy copies a file from root/src to root/dst with mtime and cmdhash, edgehash.
// if src is dir, returns error.
func (hfs *HashFS) Copy(ctx context.Context, root, src, dst string, mtime time.Time, cmdhash, edgehash []byte) error {
	if log.V(1) {
		clog.Infof(ctx, "copy @%s %s to %s", root, src, dst)
	}
	hfs.clean.Store(false)
	srcfname := makeFullpath(root, src)
	dstfname := makeFullpath(root, dst)
	e, _, _, ok := hfs.directory.lookup(ctx, srcfname)
	if !ok {
		e = newLocalEntry()
		if log.V(9) {
			clog.Infof(ctx, "new entry for copy src %s", srcfname)
		}
		e.init(ctx, srcfname, hfs.executables, hfs.OS)
		if errors.Is(e.err, context.Canceled) {
			return e.err
		}
		var err error
		e, err := hfs.directory.store(ctx, srcfname, e)
		if err != nil {
			clog.Warningf(ctx, "failed to store copy src %s: %v", srcfname, err)
			return err
		}
		clog.Infof(ctx, "copy src new entry %s %s", srcfname, e)
	}
	if err := e.err; err != nil {
		return err
	}
	subdir := e.getDir()
	if subdir != nil {
		return fmt.Errorf("is a directory: %s", srcfname)
	}
	if lsrc, ok := e.src.(osfs.FileSource); ok {
		_, err := hfs.OS.Lstat(ctx, lsrc.Fname)
		if err != nil {
			// src file uses local file, but not available
			return fmt.Errorf("copy src: %w", err)
		}
	}
	if e.target == "" {
		hfs.digester.compute(ctx, srcfname, e)
	}
	lready := make(chan bool, 1)
	lready <- true
	newEnt := &entry{
		lready:   lready,
		size:     e.size,
		mtime:    mtime,
		mode:     e.mode,
		cmdhash:  cmdhash,
		edgehash: edgehash,
		target:   e.target,
		// use the same data source as src if any.
		src:         e.src,
		d:           e.d,
		buf:         e.buf,
		updatedTime: time.Now(),
		isChanged:   true,
	}
	err := hfs.dirStoreAndNotify(ctx, dstfname, newEnt)
	if err != nil {
		return err
	}
	hfs.journalEntry(ctx, dstfname, newEnt)
	clog.Infof(ctx, "copy %s to %s", srcfname, dstfname)
	return nil
}

// Mkdir makes a directory at root/dirname.
func (hfs *HashFS) Mkdir(ctx context.Context, root, dirname string, cmdhash, edgehash []byte) error {
	if log.V(1) {
		clog.Infof(ctx, "mkdir @%s %s", root, dirname)
	}
	hfs.clean.Store(false)
	dirname = makeFullpath(root, dirname)
	fi, err := hfs.OS.Lstat(ctx, dirname)
	mtime := time.Now()
	if err == nil && fi.IsDir() {
		err := hfs.OS.Chtimes(ctx, dirname, time.Time{}, mtime)
		if err != nil {
			clog.Warningf(ctx, "failed to set dir mtime %s: %v: %v", dirname, mtime, err)
		}
	} else {
		err := hfs.OS.MkdirAll(ctx, dirname, 0755)
		if err != nil {
			return err
		}
		fi, err := hfs.OS.Lstat(ctx, dirname)
		if err != nil {
			return err
		}
		if mtime.Before(fi.ModTime()) {
			mtime = fi.ModTime()
		}
	}
	lready := make(chan bool, 1)
	lready <- true

	e := &entry{
		lready:      lready,
		mtime:       mtime,
		mode:        0644 | fs.ModeDir,
		cmdhash:     cmdhash,
		edgehash:    edgehash,
		directory:   &directory{},
		updatedTime: time.Now(),
		isChanged:   true,
	}
	err = hfs.dirStoreAndNotify(ctx, dirname, e)
	var serr storeRaceError
	if errors.As(err, &serr) {
		curEntry, ok := serr.curEntry.(*entry)
		if ok {
			// Mkdir succeeds if cur entry is the directory and has the same cmdhash, or cur cmdhash exists but trying to add no cmdhash.
			if curEntry != nil && curEntry.getDir() != nil && (bytes.Equal(cmdhash, curEntry.cmdhash) || (len(curEntry.cmdhash) > 0 && len(cmdhash) == 0)) {
				err = nil
			}
		}
	}
	clog.Infof(ctx, "mkdir %s %s: %v", dirname, mtime, err)
	if err != nil {
		return err
	}
	if len(cmdhash) > 0 {
		hfs.journalEntry(ctx, dirname, e)
	}
	return nil
}

// Remove removes a file at root/fname.
func (hfs *HashFS) Remove(ctx context.Context, root, fname string) error {
	if log.V(1) {
		clog.Infof(ctx, "remove @%s %s", root, fname)
	}
	hfs.clean.Store(false)
	fname = makeFullpath(root, fname)
	lready := make(chan bool, 1)
	lready <- true
	e := &entry{
		lready: lready,
		err:    fs.ErrNotExist,
	}
	_, err := hfs.directory.store(ctx, fname, e)
	clog.Infof(ctx, "remove %s: %v", fname, err)
	return err
}

// RemoveAll removes all files under root/name.
// Also removes from the disk at the same time.
func (hfs *HashFS) RemoveAll(ctx context.Context, root, name string) error {
	if log.V(1) {
		clog.Infof(ctx, "removeAll @%s %s", root, name)
	}
	hfs.clean.Store(false)
	name = makeFullpath(root, name)
	err := os.RemoveAll(name)
	if err == nil {
		err = fs.ErrNotExist
	}
	lready := make(chan bool, 1)
	lready <- true
	e := &entry{
		lready: lready,
		err:    err,
	}
	_, err = hfs.directory.store(ctx, name, e)
	clog.Infof(ctx, "removeAll %s [%v]: %v", name, e.err, err)
	return err
}

// Forget forgets cached entry for inputs under root.
func (hfs *HashFS) Forget(ctx context.Context, root string, inputs []string) {
	for _, fname := range inputs {
		fullname := makeFullpath(root, fname)
		hfs.directory.delete(fullname)
	}
}

// ForgetMissingsInDir forgets cached entry under root/dir if it isn't
// generated files/dirs by any steps and doesn't exist on local disk.
// It is used for a step that removes files under a dir. b/350662100
func (hfs *HashFS) ForgetMissingsInDir(ctx context.Context, root, dir string) {
	inputs := []string{dir}
	var needCheck []string
	for len(inputs) > 0 {
		fname := inputs[0]
		copy(inputs, inputs[1:])
		inputs = inputs[:len(inputs)-1]
		fi, err := hfs.Stat(ctx, root, fname)
		if errors.Is(err, fs.ErrNotExist) {
			// If it doesn't exist in hashfs,
			// no need to check more.
			continue
		}
		if err == nil {
			if fi.IsDir() {
				dents, err := hfs.ReadDir(ctx, root, fname)
				if err != nil {
					clog.Warningf(ctx, "readdir failed for %q: %v", fname, err)
					needCheck = append(needCheck, fname)
					continue
				}
				for _, dent := range dents {
					inputs = append(inputs, filepath.ToSlash(filepath.Join(fname, dent.Name())))
				}
			}
			if fi.IsChanged() {
				// it is explicitly generated file/dir,
				// no need to check more.
				continue
			}
		}
		needCheck = append(needCheck, fname)
	}
	err := ForgetMissingsSemaphore.Do(ctx, func(ctx context.Context) error {
		for _, fname := range needCheck {
			fullname := makeFullpath(root, fname)
			_, err := hfs.OS.Lstat(ctx, fullname)
			if errors.Is(err, fs.ErrNotExist) {
				clog.Infof(ctx, "forget missing %s", fullname)
				hfs.directory.delete(fullname)
				continue
			}
		}
		return nil
	})
	if err != nil {
		clog.Warningf(ctx, "forget missings in dir: %v", err)
	}
}

// ForgetMissings forgets cached entry for input under root
// if it doesn't exist on local disk, and returns valid inputs.
// It is currently used for deps=msvc only to workaround clang-cl issue.
// https://github.com/llvm/llvm-project/issues/58726
func (hfs *HashFS) ForgetMissings(ctx context.Context, root string, inputs []string) []string {
	availables := make([]string, 0, len(inputs))
	needCheck := make([]string, 0, len(inputs))
	for _, fname := range inputs {
		fi, err := hfs.Stat(ctx, root, fname)
		if errors.Is(err, fs.ErrNotExist) {
			// If it doesn't exist in hashfs,
			// no need to check with os.Lstat.
			clog.Infof(ctx, "remove from inputs %s: %v", fname, err)
			continue
		}
		if err == nil && (fi.IsChanged() || fi.IsMissingChecked()) {
			// it is explicit generated file.
			// no need to check on disk.
			availables = append(availables, fname)
			continue
		}
		needCheck = append(needCheck, fname)
	}

	err := ForgetMissingsSemaphore.Do(ctx, func(ctx context.Context) error {
		for _, fname := range needCheck {
			fullname := makeFullpath(root, fname)
			_, err := hfs.OS.Lstat(ctx, fullname)
			if errors.Is(err, fs.ErrNotExist) {
				clog.Infof(ctx, "forget missing %s", fullname)
				hfs.directory.delete(fullname)
				continue
			}
			fi, err := hfs.Stat(ctx, root, fname)
			if err == nil {
				fi.e.mu.Lock()
				fi.e.isMissingChecked = true
				fi.e.mu.Unlock()
			}
			availables = append(availables, fname)
		}
		return nil
	})
	if err != nil {
		clog.Warningf(ctx, "forget missings: %v", err)
	}
	return availables
}

// Availables returns valid inputs (i.e. exist in hashfs).
func (hfs *HashFS) Availables(ctx context.Context, root string, inputs []string) []string {
	availables := make([]string, 0, len(inputs))
	for _, fname := range inputs {
		_, err := hfs.Stat(ctx, root, fname)
		if errors.Is(err, fs.ErrNotExist) {
			// If it doesn't exist in hashfs,
			// no need to check with os.Lstat.
			clog.Infof(ctx, "remove from inputs %s: %v", fname, err)
			continue
		}
		availables = append(availables, fname)
	}
	return availables
}

// Entries gets merkletree entries for inputs at root.
// it won't return entries symlink escaped from root.
// root can be an empty string "" when inputs are absolute paths.
func (hfs *HashFS) Entries(ctx context.Context, root string, inputs []string) ([]merkletree.Entry, error) {
	ctx, span := trace.NewSpan(ctx, "fs-entries")
	defer span.Close(nil)
	var nwait int
	var wg sync.WaitGroup
	ents := make([]*entry, 0, len(inputs))
	for _, fname := range inputs {
		fname := makeFullpath(root, fname)
		e, _, _, ok := hfs.directory.lookup(ctx, fname)
		if ok {
			if log.V(2) {
				clog.Infof(ctx, "tree cache hit %s", fname)
			}
			ents = append(ents, e)
			if e.mode.IsRegular() {
				e.mu.RLock()
				ready := !e.d.IsZero()
				e.mu.RUnlock()
				if !ready {
					wg.Add(1)
					nwait++
					go func() {
						defer wg.Done()
						hfs.digester.compute(ctx, fname, e)
					}()
				}
			}
			continue
		}
		e = newLocalEntry()
		e.init(ctx, fname, hfs.executables, hfs.OS)
		if errors.Is(e.err, context.Canceled) {
			return nil, e.err
		}
		if log.V(1) {
			clog.Infof(ctx, "tree new entry %s", fname)
		}
		e, err := hfs.directory.store(ctx, fname, e)
		if err != nil {
			return nil, err
		}
		ents = append(ents, e)
		wg.Add(1)
		nwait++
		go func() {
			defer wg.Done()
			hfs.digester.compute(ctx, fname, e)
		}()
	}
	// wait ensures all entries have computed the digests.
	_, wspan := trace.NewSpan(ctx, "fs-entries-wait")
	wg.Wait()
	wspan.SetAttr("waits", nwait)
	wspan.Close(nil)
	entries := make([]merkletree.Entry, 0, len(inputs))
	for i, fname := range inputs {
		e := ents[i]
		d := e.digest()
		if e.err != nil || (d.IsZero() && e.target == "" && e.directory == nil) {
			// TODO(b/435555841): hard fail instead
			if e.entryErrLogged.CompareAndSwap(false, true) {
				clog.Warningf(ctx, "missing %s data:%v target:%q: %v", fname, e.d, e.target, e.err)
			}
			continue
		}
		data := digest.NewData(e.src, d)
		isExecutable := e.mode&0111 != 0
		target := e.target
		if target != "" {
			var tname string
			name := filepath.Join(root, fname)
			elink := e
			for range maxSymlinks {
				tname = makeFullpath(filepath.Dir(name), elink.target)
				if log.V(1) {
					clog.Infof(ctx, "symlink %s -> %s", name, tname)
				}
				if strings.HasPrefix(tname, root+"/") {
					break
				}
				// symlink to out of exec root (e.g. ../.cipd/pkgs/..)
				name = tname
				tname = ""
				var ok bool
				elink, _, _, ok = hfs.directory.lookup(ctx, name)
				if ok {
					if log.V(2) {
						clog.Infof(ctx, "tree cache hit %s", name)
					}
				} else {
					elink = newLocalEntry()
					elink.init(ctx, name, hfs.executables, hfs.OS)
					if log.V(1) {
						clog.Infof(ctx, "tree new entry %s", name)
					}
					var err error
					elink, err = hfs.directory.store(ctx, name, elink)
					if err != nil {
						return nil, err
					}
					hfs.digester.lazyCompute(ctx, name, elink)
				}
				if elink.err != nil || elink.target == "" {
					break
				}
			}
			if e != elink {
				clog.Infof(ctx, "resolve symlink %s to %s", fname, name)
				target = elink.target
				hfs.digester.compute(ctx, name, elink)
				d := elink.digest()
				data = digest.NewData(elink.src, d)
				isExecutable = elink.mode&0111 != 0
			}
		}
		entries = append(entries, merkletree.Entry{
			Name:         fname,
			Data:         data,
			IsExecutable: isExecutable,
			Target:       target,
		})
	}
	return entries, nil
}

// UpdateEntry is an entry for Update.
type UpdateEntry struct {
	Name string

	// if Entry is nil, use local disk (from RetrieveUpdateEntriesFromLocal), so need to calculate digest from file.
	// If Entry is not nil, use digest in Entry, rather than calculating digest from file.
	Entry *merkletree.Entry

	Mode        fs.FileMode
	ModTime     time.Time
	CmdHash     []byte
	EdgeHash    []byte
	Action      digest.Digest
	UpdatedTime time.Time
	IsChanged   bool

	// IsLocal=true uses Entry, but assumes file exists on local,
	// to avoid unnecessary flush operation.
	IsLocal bool
}

func (e UpdateEntry) String() string {
	var buf strings.Builder
	fmt.Fprintf(&buf, "%q ", e.Name)
	if e.Entry != nil {
		switch {
		case !e.Entry.Data.IsZero():
			fmt.Fprintf(&buf, "file=%s ", e.Entry.Data.Digest())
		case e.Entry.Target != "":
			fmt.Fprintf(&buf, "symlink=%s ", e.Entry.Target)
		default:
			fmt.Fprintf(&buf, "dir ")
		}
	} else {
		fmt.Fprintf(&buf, "local ")
	}
	fmt.Fprintf(&buf, "mode=%s ", e.Mode)
	fmt.Fprintf(&buf, "mtime=%s ", e.ModTime.Format(time.RFC3339Nano))
	fmt.Fprintf(&buf, "cmdhash=%s ", base64.StdEncoding.EncodeToString(e.CmdHash))
	fmt.Fprintf(&buf, "edgehash=%s ", base64.StdEncoding.EncodeToString(e.EdgeHash))
	fmt.Fprintf(&buf, "action=%s ", e.Action)
	if !e.ModTime.Equal(e.UpdatedTime) {
		fmt.Fprintf(&buf, "updated_time=%s ", e.UpdatedTime.Format(time.RFC3339Nano))
	}
	fmt.Fprintf(&buf, "changed=%t is_local=%t", e.IsChanged, e.IsLocal)
	return buf.String()
}

// Update updates cache information for entries under execRoot.
func (hfs *HashFS) Update(ctx context.Context, execRoot string, entries []UpdateEntry) error {
	ctx, span := trace.NewSpan(ctx, "fs-update")
	defer span.Close(nil)
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	default:
	}
	hfs.clean.Store(false)

	// sort inputs, so update dir containing files first. b/300385880
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name < entries[j].Name
	})

	if hfs.opt.CogFS != nil || hfs.opt.ArtFS != nil {
		// TODO: pass UpdateEntry so artfs can set mtime?
		var updates []merkletree.Entry
		var updateIdx []int
		var nFromLocals, nNonFiles int
		for i, ent := range entries {
			if ent.Entry == nil {
				// UpdateEntry was captured by RetrieveUpdateEntriesFromLocal
				// so file already exist on local disk
				nFromLocals++
				continue
			}
			if ent.Entry.Data.IsZero() {
				// symlink or dir. handled in usual way.
				nNonFiles++
				continue
			}
			updateIdx = append(updateIdx, i)
			updates = append(updates, *ent.Entry)
		}
		if len(updates) > 0 {
			err := hfs.opt.ArtFS.ArtfsInsert(ctx, execRoot, updates)
			if err != nil {
				clog.Warningf(ctx, "artfs insert %d under %s: %v", len(updates), execRoot, err)
			} else {
				clog.Infof(ctx, "artfs insert %d under %s", len(updates), execRoot)
				// artfsfs inserted the update, so we can assume
				// these files exist locally.
				for _, i := range updateIdx {
					entries[i].IsLocal = true
				}
			}
		} else {
			clog.Warningf(ctx, "artfs insert 0 from_local=%d not_file=%d", nFromLocals, nNonFiles)
		}
	}

	for _, ent := range entries {
		clog.Infof(ctx, "update %v", ent)
		fname := filepath.Join(execRoot, ent.Name)
		fname = filepath.ToSlash(fname)
		if ent.Entry == nil {
			// UpdateEntry was captured by RetrieveUpdateEntriesFromLocal
			// so the entry should exists in hfs.directory.
			e, _, _, ok := hfs.dirLookup(ctx, execRoot, ent.Name)
			if !ok {
				clog.Warningf(ctx, "failed to update: no entry %s", ent.Name)
				continue
			}
			if e.getMtime().Equal(ent.ModTime) || e.getDir() != nil {
				e.mu.Lock()
				e.mtime = ent.ModTime
				e.mode = ent.Mode
				e.cmdhash = ent.CmdHash
				e.edgehash = ent.EdgeHash
				e.action = ent.Action
				e.local = ent.IsLocal
				e.updatedTime = ent.UpdatedTime
				e.isChanged = ent.IsChanged
				e.entryErrLogged.Store(false)
				e.mu.Unlock()
			} else {
				e = newLocalEntry()
				e.init(ctx, fname, hfs.executables, hfs.OS)
				e.mtime = ent.ModTime
				e.cmdhash = ent.CmdHash
				e.edgehash = ent.EdgeHash
				e.action = ent.Action
				e.local = ent.IsLocal
				e.updatedTime = ent.UpdatedTime
				e.isChanged = ent.IsChanged
			}
			if errors.Is(e.err, context.Canceled) {
				return e.err
			}
			// notify this output to scandeps.
			err := hfs.dirStoreAndNotify(ctx, fname, e)
			if err != nil {
				return err
			}
			hfs.journalEntry(ctx, fname, e)
			if ent.IsLocal && e.isChanged && e.target == "" {
				// Update mtime for the local entry if it has changed.
				// Don't update mtime for symlink,
				// since os.Chtimes updates the mtime of target
				// and it makes the target invalidated
				// in .siso_fs_state since mtime doesn't match.
				err := hfs.OS.Chtimes(ctx, fname, time.Time{}, e.getMtime())
				if errors.Is(err, fs.ErrNotExist) {
					clog.Warningf(ctx, "failed to update mtime of %s: %v", fname, err)
					continue
				}
				if err != nil {
					return fmt.Errorf("failed to update mtime of %s: %w", fname, err)
				}
			}
			continue
		}
		switch {
		case !ent.Entry.Data.IsZero():
			lready := make(chan bool, 1)
			if ent.IsLocal {
				close(lready)
			} else {
				lready <- true
			}
			mode := ent.Mode
			if ent.Entry.IsExecutable {
				mode |= 0111
			}
			e := &entry{
				lready:      lready,
				size:        ent.Entry.Data.Digest().SizeBytes,
				mtime:       ent.ModTime,
				mode:        mode,
				cmdhash:     ent.CmdHash,
				edgehash:    ent.EdgeHash,
				action:      ent.Action,
				local:       ent.IsLocal,
				src:         ent.Entry.Data,
				d:           ent.Entry.Data.Digest(),
				updatedTime: ent.UpdatedTime,
				isChanged:   ent.IsChanged,
			}
			err := hfs.dirStoreAndNotify(ctx, fname, e)
			if err != nil {
				return err
			}
			hfs.journalEntry(ctx, fname, e)
			if ent.IsLocal && e.isChanged {
				err = hfs.OS.Chtimes(ctx, fname, time.Time{}, e.getMtime())
				if errors.Is(err, fs.ErrNotExist) {
					clog.Warningf(ctx, "failed to update mtime of %s: %v", fname, err)
					continue
				}
				if err != nil {
					return fmt.Errorf("failed to update mtime of %s: %w", fname, err)
				}
			}
		case ent.Entry.Target != "":
			lready := make(chan bool, 1)
			if ent.IsLocal {
				close(lready)
			} else {
				lready <- true
			}
			mode := ent.Mode
			mode |= fs.ModeSymlink
			e := &entry{
				lready:   lready,
				mtime:    ent.ModTime,
				mode:     mode,
				cmdhash:  ent.CmdHash,
				edgehash: ent.EdgeHash,
				action:   ent.Action,
				local:    true,
				target:   ent.Entry.Target,

				updatedTime: ent.UpdatedTime,
				isChanged:   ent.IsChanged,
			}
			err := hfs.dirStoreAndNotify(ctx, fname, e)
			if err != nil {
				return err
			}
			hfs.journalEntry(ctx, fname, e)
		default: // directory
			lready := make(chan bool, 1)
			if ent.IsLocal {
				close(lready)
			} else {
				lready <- true
			}
			mode := ent.Mode
			mode |= fs.ModeDir
			e := &entry{
				lready:    lready,
				mtime:     ent.ModTime,
				mode:      mode,
				cmdhash:   ent.CmdHash,
				edgehash:  ent.EdgeHash,
				action:    ent.Action,
				local:     true,
				directory: &directory{},

				updatedTime: ent.UpdatedTime,
				isChanged:   ent.IsChanged,
			}
			err := hfs.dirStoreAndNotify(ctx, fname, e)
			if err != nil {
				return err
			}
			hfs.journalEntry(ctx, fname, e)
			err = hfs.OS.Chtimes(ctx, fname, time.Time{}, ent.ModTime)
			if err != nil {
				clog.Warningf(ctx, "failed to update dir mtime %s: %v", fname, err)
			}
		}
	}
	return nil
}

// RetrieveUpdateEntries gets UpdateEntry for fnames at root.
func (hfs *HashFS) RetrieveUpdateEntries(ctx context.Context, root string, fnames []string) []UpdateEntry {
	ctx, span := trace.NewSpan(ctx, "fs-update-entries")
	defer span.Close(nil)
	ents, err := hfs.Entries(ctx, root, fnames)
	if err != nil {
		clog.Warningf(ctx, "failed to get entries: %v", err)
	}
	entries := make([]UpdateEntry, 0, len(ents))
	for _, ent := range ents {
		fi, err := hfs.Stat(ctx, root, ent.Name)
		if err != nil {
			clog.Warningf(ctx, "failed to stat %s: %v", ent.Name, err)
			continue
		}
		entries = append(entries, UpdateEntry{
			Name:        ent.Name,
			Entry:       &ent,
			Mode:        fi.Mode(),
			ModTime:     fi.ModTime(),
			CmdHash:     fi.CmdHash(),
			EdgeHash:    fi.EdgeHash(),
			Action:      fi.Action(),
			UpdatedTime: fi.UpdatedTime(),
			IsChanged:   fi.IsChanged(),
		})
	}
	return entries
}

// RetrieveUpdateEntriesFromLocal gets UpdateEntry for fnames at root from local disk.
// It is intended to be used for local execution outputs in cmd.RecordOutputsFromLocal.
// It won't wait for digest calculation for entries, so UpdateEntry's Entry
// will be nil.
// It will forget recorded enties when err (doesn't exist or so).
func (hfs *HashFS) RetrieveUpdateEntriesFromLocal(ctx context.Context, root string, fnames []string) []UpdateEntry {
	ctx, span := trace.NewSpan(ctx, "fs-update-entries-from-local")
	defer span.Close(nil)

	ents := make([]UpdateEntry, 0, len(fnames))
	for _, fname := range fnames {
		fullname := makeFullpath(root, fname)
		lfi, err := hfs.OS.Lstat(ctx, fullname)
		if errors.Is(err, fs.ErrNotExist) {
			clog.Warningf(ctx, "missing local %s: %v", fname, err)
			hfs.directory.delete(fullname)
			continue
		} else if err != nil {
			clog.Warningf(ctx, "failed to access local %s: %v", fname, err)
			hfs.directory.delete(fullname)
			continue
		}
		if !lfi.IsDir() {
			// forget old entries unless dir.
			// need to keep dir to keep other files in the dir.
			hfs.directory.delete(fullname)
		}
		ent := UpdateEntry{
			Name:    fname,
			Mode:    lfi.Mode(),
			ModTime: lfi.ModTime(),
			IsLocal: true,
		}
		fi, err := hfs.Stat(ctx, root, fname)
		if err != nil {
			clog.Warningf(ctx, "failed to stat after invalidate %s: %v", fname, err)
		} else {
			ent.CmdHash = fi.CmdHash()
			ent.EdgeHash = fi.EdgeHash()
			ent.Action = fi.Action()
			ent.UpdatedTime = fi.UpdatedTime()
			ent.IsChanged = fi.IsChanged()
		}
		ents = append(ents, ent)
	}
	return ents
}

type noSource struct {
	filename string
}

func (ns noSource) Open(ctx context.Context) (io.ReadCloser, error) {
	return nil, fmt.Errorf("no source for %s", ns.filename)
}

func (ns noSource) String() string {
	return fmt.Sprintf("noSource:%s", ns.filename)
}

type noDataSource struct{}

func (noDataSource) Source(_ context.Context, d digest.Digest, fname string) digest.Source {
	return noSource{fname}
}

// NeedFlush returns whether the fname need to be flushed based on OutputLocal option.
func (hfs *HashFS) NeedFlush(ctx context.Context, execRoot, fname string) bool {
	return hfs.opt.OutputLocal(ctx, makeFullpath(execRoot, fname))
}

// Flush flushes cached information for files under execRoot to local disk.
func (hfs *HashFS) Flush(ctx context.Context, execRoot string, files []string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ctx, span := trace.NewSpan(ctx, "flush")
	defer span.Close(nil)
	eg, ctx := errgroup.WithContext(ctx)
	for _, file := range files {
		fname := makeFullpath(execRoot, file)
		e, _, _, ok := hfs.directory.lookup(ctx, fname)
		if !ok {
			// If it doesn't exist in memory, just use local disk as is.
			continue
		}
		select {
		case need := <-e.lready:
			if !need {
				// need=false means file is already downloaded,
				// or entry was constructed from local disk.
				if log.V(1) {
					clog.Infof(ctx, "flush %s local ready", fname)
				}
				e.mu.Lock()
				if e.mtimeUpdated && e.target == "" {
					// mtime was updated after entry sets mtime from the local disk.
					// Don't update mtime for symlink,
					// since os.Chtimes updates the mtime of target
					// and it makes the target invalidated
					// in .siso_fs_state since mtime doesn't match.
					err := hfs.OS.Chtimes(ctx, fname, time.Time{}, e.mtime)
					if errors.Is(err, fs.ErrNotExist) {
						e.mu.Unlock()
						return fmt.Errorf("flush %s local-ready: %w", fname, err)
					}
					clog.Infof(ctx, "flush %s local ready mtime update: %v", fname, err)
					if err == nil {
						e.mtimeUpdated = false
					}
				}
				e.mu.Unlock()
				err := e.err
				if errors.Is(err, fs.ErrNotExist) || errors.Is(err, errNotRegular) {
					clog.Warningf(ctx, "flush %s local-ready: %v", fname, err)
					continue
				}
				if err != nil {
					return fmt.Errorf("flush %s local-ready: %w", fname, err)
				}
				continue
			}
		case <-ctx.Done():
			return fmt.Errorf("flush wait local-ready %s: %w", fname, context.Cause(ctx))
		}
		hfs.digester.compute(ctx, fname, e)
		ctx, done, err := FlushSemaphore.WaitAcquire(ctx)
		if err != nil {
			// flush failed, so may need to flush again.
			select {
			case e.lready <- true:
			default:
			}
			return fmt.Errorf("flush sempahore %s: %w", fname, err)
		}
		eg.Go(func() (err error) {
			defer func() { done(err) }()
			err = e.flush(ctx, fname, hfs.OS, max(e.d.FetchTimeout(), hfs.opt.MinFlushTimeout))
			// flush should not fail with cas not found error.
			// but if it failed, current recorded digest should
			// be wrong, so should delete from the hashfs.
			if code := status.Code(err); code == codes.NotFound {
				clog.Warningf(ctx, "flush failed. delete %s from hashfs: %v", fname, err)
				hfs.directory.delete(fname)
			}
			return err
		})
	}
	return eg.Wait()
}

// Refresh refreshes cached file entries under execRoot.
func (hfs *HashFS) Refresh(ctx context.Context, execRoot string) error {
	// TODO: optimize?
	state := hfs.State(ctx)
	// reset loaded as it reset entry data.
	hfs.loaded.Store(false)
	hfs.directory = &directory{isRoot: true}
	err := hfs.SetState(ctx, state)
	werr := hfs.WaitReady(ctx)
	if err != nil {
		return err
	}
	return werr
}

type entry struct {
	// lready represents whether it is ready to use local file.
	// true - need to download contents.
	// block - download is in progress.
	// closed/false - file is already downloaded.
	lready chan bool

	err  error
	size int64
	mode fs.FileMode

	// cmdhash is hash of command lines that generated this file.
	// e.g. hash('touch output.stamp')
	cmdhash []byte

	// edgehash is hash of inputs/outputs paths that generated this file.
	edgehash []byte

	// digest of action that generated this file.
	action digest.Digest

	// local indicates the file is generated locally.
	local bool
	// isChanged indicates the file is changed in the session.
	isChanged bool
	// isMissingChecked indicates the file is checked in ForgetMissings.
	isMissingChecked bool

	target string // symlink.

	src digest.Source
	buf []byte // from WriteFile.

	mu sync.RWMutex
	// mtime of entry in hashfs.
	mtime        time.Time
	mtimeUpdated bool
	// updatedTime is timestamp when the file has been updated
	// by Update or UpdateFromLocal.
	// need to distinguish from mtime for restat=1.
	// updatedTime should be equal or newer than mtime.
	updatedTime time.Time

	entryErrLogged atomic.Bool

	d         digest.Digest
	dch       chan struct{} // wait for digest computation
	directory *directory
}

func newLocalEntry() *entry {
	lready := make(chan bool)
	close(lready)
	return &entry{
		lready: lready,
	}
}

func (e *entry) String() string {
	if e.err != nil {
		return fmt.Sprintf("err:%v", e.err)
	}
	return fmt.Sprintf("size:%d mode:%s mtime:%s", e.size, e.mode, e.getMtime())
}

var errNotRegular = errors.New("unexpected filetype not regular")

func (e *entry) init(ctx context.Context, fname string, executables map[string]bool, osfs *osfs.OSFS) {
	fi, err := osfs.Lstat(ctx, fname)
	if errors.Is(err, fs.ErrNotExist) {
		if log.V(1) {
			clog.Infof(ctx, "not exist %s", fname)
		}
		e.err = err
		return
	}
	if err != nil {
		clog.Warningf(ctx, "failed to lstat %s: %v", fname, err)
		e.err = err
		return
	}
	err = waitUntilModTime(ctx, fname, fi.ModTime())
	if err != nil {
		e.err = err
		return
	}
	switch {
	case fi.IsDir():
		if log.V(1) {
			clog.Infof(ctx, "tree entry %s: is dir", fname)
		}
		e.directory = &directory{}
		e.mode = 0644 | fs.ModeDir
	case fi.Mode().Type() == fs.ModeSymlink:
		e.mode = 0644 | fs.ModeSymlink
		e.target, err = osfs.Readlink(ctx, fname)
		if err != nil {
			e.err = err
		}
		if log.V(1) {
			clog.Infof(ctx, "tree entry %s: symlink to %s: %v", fname, e.target, e.err)
		}
	case fi.Mode().IsRegular():
		e.mode = 0644
		if isExecutable(fi, fname, executables) {
			e.mode |= 0111
		}
		e.size = fi.Size()
		e.src = osfs.FileSource(fname, fi.Size())
	default:
		// e.g. fifo in chromiumos build tree?
		// /build/amd64-generic/tmp/portage/chromeos-base/chromeos-chrome-139.0.7206.0_rc-r1/.ipc/in: unknown filetype prwxrwx---
		e.err = fmt.Errorf("entry %s: %s: %w", fname, fi.Mode(), errNotRegular)
		clog.Warningf(ctx, "tree entry %s: unknown filetype %s", fname, fi.Mode())
		return
	}
	if e.mtime.Before(fi.ModTime()) {
		e.mtime = fi.ModTime()
	}
	if e.updatedTime.Before(e.mtime) {
		e.updatedTime = e.mtime
	}
}

func (e *entry) compute(ctx context.Context, fname string) error {
	needCompute, doCompute, err := func() (bool, bool, error) {
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.err != nil {
			return false, false, e.err
		}
		if !e.d.IsZero() {
			return false, false, nil
		}
		if e.src == nil {
			return false, false, nil
		}
		doCompute := e.dch == nil
		if doCompute {
			e.dch = make(chan struct{})
		}
		return true, doCompute, nil
	}()
	if !needCompute {
		return err
	}
	if doCompute {
		data, err := localDigest(ctx, e.src, fname)
		if err != nil {
			e.mu.Lock()
			close(e.dch)
			e.err = err
			e.entryErrLogged.Store(false)
			e.mu.Unlock()
			return err
		}
		e.mu.Lock()
		e.d = data.Digest()
		close(e.dch)
		e.entryErrLogged.Store(false)
		e.mu.Unlock()
	} else {
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-e.dch:
		}
	}
	return nil
}

func (e *entry) digest() digest.Digest {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.d
}

func (e *entry) getMtime() time.Time {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.mtime
}

func (e *entry) getUpdatedTime() time.Time {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.updatedTime
}

func (e *entry) updateDir(ctx context.Context, hfs *HashFS, dname string) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	defer e.entryErrLogged.Store(false)
	d, err := os.Open(dname)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			clog.Warningf(ctx, "updateDir %s: open %v", dname, err)
		}
		return nil
	}
	defer d.Close()
	fi, err := d.Stat()
	if err != nil {
		clog.Warningf(ctx, "updateDir %s: stat %v", dname, err)
		return nil
	}
	if !fi.IsDir() {
		clog.Warningf(ctx, "updateDir %s: is not dir?", dname)
		return nil
	}
	if fi.ModTime().Equal(e.directory.mtime) {
		if log.V(1) {
			clog.Infof(ctx, "updateDir %s: up-to-date %s", dname, e.mtime)
		}
		return nil
	}
	started := time.Now()
	names, err := d.Readdirnames(-1)
	if err != nil {
		clog.Warningf(ctx, "updateDir %s: readdirnames %v", dname, err)
		return nil
	}
	if hfs.OnCog() {
		var wg sync.WaitGroup
		for _, name := range names {
			// don't scan temporary file by readdir.
			// it may cause race on windows.
			// b/294318963 b/381947692
			if strings.HasSuffix(name, ".tmp") || strings.HasPrefix(name, ".tempfile.") {
				continue
			}
			if hfs.opt.Ignore(ctx, filepath.Join(dname, name)) {
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				// update entry in e.directory.
				_, err := hfs.stat(ctx, dname, name, false)
				if err != nil {
					clog.Warningf(ctx, "updateDir stat %s: %v", name, err)
				}
			}()
		}
		wg.Wait()
	} else {
		for _, name := range names {
			// don't scan temporary file by readdir.
			// it may cause race on windows.
			// b/294318963 b/381947692
			if strings.HasSuffix(name, ".tmp") || strings.HasPrefix(name, ".tempfile.") {
				continue
			}
			if hfs.opt.Ignore(ctx, filepath.Join(dname, name)) {
				continue
			}
			// update entry in e.directory.
			_, err := hfs.stat(ctx, dname, name, false)
			if err != nil {
				clog.Warningf(ctx, "updateDir stat %s: %v", name, err)
			}
		}
	}
	clog.Infof(ctx, "updateDir mtime %s %d %s -> %s: %s", dname, len(names), e.directory.mtime, fi.ModTime(), time.Since(started))
	e.directory.mtime = fi.ModTime()
	// if local dir is updated after hashfs update, update hashfs mtime.
	if e.mtime.Before(e.directory.mtime) {
		e.mtime = e.directory.mtime
	}
	return names
}

// getDir returns directory of entry.
func (e *entry) getDir() *directory {
	if e == nil {
		return nil
	}
	return e.directory
}

func (e *entry) flush(ctx context.Context, fname string, osfs *osfs.OSFS, timeout time.Duration) (retErr error) {
	defer func() {
		if retErr != nil {
			// flush failed, so may need to flush again.
			clog.Warningf(ctx, "failed to flush %s: %v", fname, retErr)
			select {
			case e.lready <- true:
			default:
			}
			return
		}
		// flush successfully completed.
		// no need to flush again.
		close(e.lready)
	}()
	started := time.Now()

	if errors.Is(e.err, fs.ErrNotExist) {
		// to protect concurrent digest calculation and removal
		// on Windows.
		digestLock.Lock()
		for {
			if _, ok := digestFnames[fname]; !ok {
				break
			}
			// wait if digest calculation on fname is under progress
			digestCond.Wait()
		}
		err := osfs.Remove(ctx, fname)
		digestLock.Unlock()
		clog.Infof(ctx, "flush remove %s: %v", fname, err)
		return err
	}
	d := e.digest()
	mtime := e.getMtime()
	switch {
	case e.directory != nil:
		// directory
		fi, err := osfs.Lstat(ctx, fname)
		if err == nil && fi.IsDir() && fi.ModTime().Equal(mtime) {
			if log.V(1) {
				clog.Infof(ctx, "flush dir %s: already exist", fname)
			}
			return nil
		}
		err = osfs.MkdirAll(ctx, fname, 0755)
		if err != nil {
			clog.Infof(ctx, "flush dir %s: %v", fname, err)
		} else {
			err = osfs.Chtimes(ctx, fname, time.Time{}, mtime)
			clog.Infof(ctx, "flush dir chtime %s %v: %v", fname, mtime, err)
		}
		return err
	case d.IsZero() && e.target != "":
		target, err := osfs.Readlink(ctx, fname)
		if err == nil && e.target == target {
			return nil
		}
		e.mu.Lock()
		err = osfs.Symlink(ctx, e.target, fname)
		if errors.Is(err, fs.ErrExist) {
			err = osfs.Remove(ctx, fname)
			if err == nil {
				err = osfs.Symlink(ctx, e.target, fname)
			}
		}
		e.mu.Unlock()
		clog.Infof(ctx, "flush symlink %s -> %s: %v", fname, e.target, err)
		// don't change mtimes. it fails if target doesn't exist.
		return err
	default:
	}
	fi, err := osfs.Lstat(ctx, fname)
	// need to remove the file after it reads from data source,
	// since data source will read from the local disk.
	var removeReason string
	if err == nil {
		if fi.IsDir() {
			err := &fs.PathError{
				Op:   "flush",
				Path: fname,
				Err:  syscall.EISDIR,
			}
			clog.Warningf(ctx, "flush %s: %v", fname, err)
			return err
		}
		if fi.Size() == d.SizeBytes && fi.ModTime().Equal(mtime) {
			// TODO: check hash, mode?
			clog.Infof(ctx, "flush %s: already exist", fname)
			return nil
		}
		if isHardlink(fi) {
			removeReason = "hardlink"
		} else if !fi.Mode().IsRegular() {
			removeReason = fmt.Sprintf("non-regular file %s", fi.Mode())
		} else if fi.Size() == d.SizeBytes {
			// check existing file and hashfs entry are identical
			// or not by checking digest.
			// if size differs, no need to check and force
			// to write hashfs entry to the disk.
			var fileDigest digest.Digest
			src := osfs.FileSource(fname, fi.Size())
			ld, err := localDigest(ctx, src, fname)
			if err == nil {
				fileDigest = ld.Digest()
				if fileDigest == d {
					clog.Infof(ctx, "flush %s: already exist - hash match", fname)
					if !fi.ModTime().Equal(mtime) {
						err = osfs.Chtimes(ctx, fname, time.Time{}, mtime)
					}
					return err
				}
			}
			clog.Warningf(ctx, "flush %s: exists but mismatch size:%d!=%d mtime:%s!=%s d:%v!=%v", fname, fi.Size(), d.SizeBytes, fi.ModTime(), mtime, fileDigest, d)
		}
		if removeReason == "" && fi.Mode()&0200 == 0 {
			// need to be writable. otherwise os.WriteFile fails with permission denied.
			err = osfs.Chmod(ctx, fname, fi.Mode()|0200)
			clog.Warningf(ctx, "flush %s: not writable? %s: %v", fname, fi.Mode(), err)
		}
	}
	err = osfs.MkdirAll(ctx, filepath.Dir(fname), 0755)
	if err != nil {
		clog.Warningf(ctx, "flush %s: mkdir: %v", fname, err)
		return fmt.Errorf("failed to create directory for %s: %w", fname, err)
	}
	if d.SizeBytes == 0 {
		if removeReason != "" {
			err = osfs.Remove(ctx, fname)
			clog.Infof(ctx, "flush %s: remove %s: %v", fname, removeReason, err)
		}
		clog.Infof(ctx, "flush %s: empty file", fname)
		err := osfs.WriteFile(ctx, fname, nil, 0644)
		if err != nil {
			return err
		}
		err = osfs.Chtimes(ctx, fname, time.Time{}, mtime)
		if err != nil {
			return err
		}
		return nil
	}
	buf := e.buf
	removeBeforeWrite := func() {
		if removeReason != "" {
			err = osfs.Remove(ctx, fname)
			clog.Infof(ctx, "flush %s: remove %s: %v", fname, removeReason, err)
		}
	}
	if len(buf) == 0 {
		if e.d.IsZero() {
			return fmt.Errorf("no data: retrieve %s: ", fname)
		}
		err = func() error {
			// check if hashfs entry is copy of local file,
			// i.e. created by hashfs Copy method.
			// if hashfs entry is set by remote action,
			// it would not be osfs.FileSource
			lsrc, ok := osfs.AsFileSource(e.src)
			type clonefiler interface {
				Clonefile(context.Context, string, string) error
			}
			var osfsany any = osfs
			osfsc, cok := osfsany.(clonefiler)
			if ok && cok {
				if lsrc.Fname == fname {
					err = osfs.Chmod(ctx, fname, e.mode)
					return err
				}
				removeBeforeWrite()
				clog.Infof(ctx, "flush %s %s clone from source %s", fname, d, lsrc.Fname)
				err := osfsc.Clonefile(ctx, lsrc.Fname, fname)
				if err == nil {
					err = osfs.Chmod(ctx, fname, e.mode)
					return err
				}
				// clonefile err, fallback to normal copy
				clog.Warningf(ctx, "clonefile failed: %v", err)
			}
			var srcname string
			if ok {
				srcname = lsrc.Fname
			}
			// write into tmp and rename after remove.
			// e.src may be the same as fname, but
			// we may need to remove fname for some reason
			// (hardlink etc).
			tmpname := filepath.Join(filepath.Dir(fname), "."+filepath.Base(fname)+".tmp")
			ctx, cancel := digest.ContextWithTimeout(ctx, e.d)
			defer cancel()
			err := retry.Do(ctx, func() error {
				ctx, cancel := context.WithTimeout(ctx, timeout)
				defer cancel()
				err := osfs.WriteDigestData(ctx, tmpname, e.src, e.mode)
				if status.Code(err) == codes.DeadlineExceeded || errors.Is(err, context.DeadlineExceeded) {
					// make it retriable error
					return status.Errorf(codes.Aborted, "timed out in WriteDigestData %s %s: %v", e.d, time.Since(started), err)
				}
				return err
			})
			if err != nil {
				return fmt.Errorf("flush tmp %s size=%d %s: %w", tmpname, d.SizeBytes, time.Since(started), err)
			}
			removeBeforeWrite()
			clog.Infof(ctx, "flush %s %s from source %s", fname, d, srcname)
			err = osfs.Rename(ctx, tmpname, fname)
			return err
		}()
	} else {
		removeBeforeWrite()
		clog.Infof(ctx, "flush %s from embedded buf", fname)
		err = osfs.WriteFile(ctx, fname, buf, e.mode)
	}
	if err != nil {
		return err
	}
	err = osfs.Chtimes(ctx, fname, time.Time{}, mtime)
	if err != nil {
		return err
	}
	return nil
}

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
		subdir, target, ok := resolveNextDir(ctx, d, lookupNextDir, pe, elem, fname)
		if subdir == nil {
			if target != "" {
				return nil, nil, target, false
			}
			if !ok {
				return nil, nil, "", false
			}
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
			// check whether there is an update from previous entry.
			ee := v.(*entry)
			if e == ee {
				// if storing entry `e` is the same as stored entry `ee`, no need to update.
				return e, "", nil
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
			if e.target != "" && ee.target != e.target {
				if log.V(1) {
					// lv is to reduce the number of memory allocations when variables are escaping to heap.
					lv := struct {
						origFname         string
						cmdchanged        bool
						edgechanged       bool
						eetarget, etarget string
					}{pe.origFname, cmdchanged, edgechanged, ee.target, e.target}
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
					}{pe.origFname, cmdchanged, edgechanged, eed, e.d}
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
					}{pe.origFname, cmdchanged, edgechanged, actionchanged}
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
					}{pe.origFname, ee.getMtime(), ee.getUpdatedTime()}
					clog.Infof(ctx, "store %s: mtime updated %v %v", lv.origFname, lv.mtime, lv.updatedTime)
				}
				return ee, "", nil
			} else if ee.getDir() != nil && e.getDir() != nil {
				// ok if mkdir with the no cmdhash or same cmdhash.
				if (len(ee.cmdhash) > 0 && len(e.cmdhash) == 0) || bytes.Equal(ee.cmdhash, e.cmdhash) {
					return ee, "", nil
				}
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
		subdir, resolved, ok := resolveNextDir(ctx, d, nextDir, pe, elem, fname)
		if resolved != "" {
			return nil, resolved, nil
		}
		if !ok {
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
// resolveNextDir returns directory if resolved `elem` is directory.
// resolveNextDir returns resolved path name as string if resolved `elem` is symlink.
func resolveNextDir(ctx context.Context, d *directory, next func(context.Context, *directory, pathElements, string) (*directory, string, bool), pe pathElements, elem, rest string) (*directory, string, bool) {
	for i := range maxSymlinks {
		nextDir, target, ok := next(ctx, d, pe, elem)
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

		if !ok {
			return nil, "", false
		}
		if nextDir != nil {
			return nextDir, "", true
		}
		if log.V(1) {
			clog.Infof(ctx, "next %s %d", elem, i)
		}
	}
	if log.V(1) {
		clog.Warningf(ctx, "resolve loop?")
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
		if dent.err != nil {
			return nil, "", false
		}
		target := dent.target
		subdir := dent.getDir()
		if subdir == nil && target == "" {
			return nil, "", false
		}
		return subdir, target, true
	}
	return nil, "", false
}

// next for store case.
// nextDir will create next dir entry if needed.
func nextDir(ctx context.Context, d *directory, pe pathElements, elem string) (*directory, string, bool) {
	v, ok := d.m.Load(elem)
	if ok {
		dent := v.(*entry)
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
			return subdir, target, true
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
			return nil, target, true
		default:
			clog.Warningf(ctx, "unexpected mode %s: %s", fullname, dfi.Mode().Type())
			return nil, "", false
		}
	}
	lready := make(chan bool, 1)
	lready <- true
	newDent := &entry{
		lready: lready,
		mode:   0644 | fs.ModeDir,
		mtime:  mtime,
		// don't set directory.mtime for intermediate dir.
		// mtime will be updated by updateDir
		// when all dirents have been loaded.
		directory: &directory{},
	}
	dent := newDent
	v, ok = d.m.LoadOrStore(elem, dent)
	if ok {
		dent = v.(*entry)
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
		clog.Warningf(ctx, "store %s no dir, no symlink", pe.origFname)
		return nil, "", false
	}
	return d, target, true
}

func (d *directory) delete(fname string) {
	for fname != "" {
		fname = strings.TrimPrefix(fname, "/")
		elem, rest, ok := strings.Cut(fname, "/")
		if !ok {
			d.m.Delete(fname)
			return
		}
		fname = rest
		v, ok := d.m.Load(elem)
		if !ok {
			return
		}
		e := v.(*entry)
		d = e.getDir()
		if d == nil {
			return
		}
	}
}

// FileInfo implements https://pkg.go.dev/io/fs#FileInfo.
type FileInfo struct {
	root  string
	fname string
	e     *entry
	fis   []FileInfo
}

func (fi FileInfo) Path() string {
	return makeFullpath(fi.root, fi.fname)
}

// Name is a base name of the file.
func (fi FileInfo) Name() string {
	return filepath.Base(fi.fname)
}

// Size is a size of the file.
func (fi FileInfo) Size() int64 {
	return fi.e.size
}

// Mode is a file mode of the file.
func (fi FileInfo) Mode() fs.FileMode {
	return fi.e.mode
}

// ModTime is a modification time of the file.
func (fi FileInfo) ModTime() time.Time {
	return fi.e.getMtime()
}

// UpdatedTime is a update time of the file.
// Usually it is the same with ModTime, but may differ for restat=1.
func (fi FileInfo) UpdatedTime() time.Time {
	return fi.e.getUpdatedTime()
}

// IsChanged returns true if file has been changed in the session.
func (fi FileInfo) IsChanged() bool {
	fi.e.mu.RLock()
	defer fi.e.mu.RUnlock()
	return fi.e.isChanged
}

// IsMissingChecked returns true if file has been checked existence
// for ForgetMissings.
func (fi FileInfo) IsMissingChecked() bool {
	fi.e.mu.RLock()
	defer fi.e.mu.RUnlock()
	return fi.e.isMissingChecked
}

// IsDir returns true if it is the directory.
func (fi FileInfo) IsDir() bool {
	return fi.e.mode.IsDir()
}

// Sys returns merkletree Entry of the file.
// For local file, digest may not be calculated yet.
// Use Entries to get correct merkletree.Entry.
func (fi FileInfo) Sys() any {
	d := fi.e.digest()
	return merkletree.Entry{
		Name:         fi.Path(),
		Data:         digest.NewData(fi.e.src, d),
		IsExecutable: fi.e.mode&0111 != 0,
		Target:       fi.e.target,
	}
}

// CmdHash returns a cmdhash that created the file.
func (fi FileInfo) CmdHash() []byte {
	return fi.e.cmdhash
}

// EdgeHash returns a edgehash that created the file.
func (fi FileInfo) EdgeHash() []byte {
	return fi.e.edgehash
}

// Action returns a digest of action that created the file.
func (fi FileInfo) Action() digest.Digest {
	return fi.e.action
}

// Target returns a symlink target of the file, or empty if it is not symlink.
func (fi FileInfo) Target() string {
	return fi.e.target
}

// Symlinks returns a symlink's FileInfo to resolve the file.
func (fi FileInfo) Symlinks() []FileInfo {
	return fi.fis
}

// DirEntry implements https://pkg.go.dev/io/fs#DirEntry.
type DirEntry struct {
	fi FileInfo
}

// Name is a base name in the directory.
func (de DirEntry) Name() string {
	return de.fi.Name()
}

// IsDir returns true if it is a directory.
func (de DirEntry) IsDir() bool {
	return de.fi.IsDir()
}

// Type returns a file type.
func (de DirEntry) Type() fs.FileMode {
	return de.fi.Mode().Type()
}

// Info returns a FileInfo.
func (de DirEntry) Info() (fs.FileInfo, error) {
	return de.fi, nil
}

// waitUntilModTime ensures mtime is past timestamp
// for clock stepback by NTP.
func waitUntilModTime(ctx context.Context, fullname string, mtime time.Time) error {
	now := time.Now()
	if !mtime.After(now) {
		return nil
	}
	if mtime.Sub(now) > 5*time.Second {
		return fmt.Errorf("future mtime on %s: mtime=%s now=%s", fullname, mtime, now)
	}
	// report error as it implies that broken time synchronization
	// so unreliable build.
	clog.Errorf(ctx, "waiting for future mtime on %s: mtime=%s now=%s", fullname, mtime, now)
	started := now
	for time.Since(started) < 5*time.Second {
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-time.After(min(5*time.Second, mtime.Sub(now))):
		}
		now = time.Now()
		if !mtime.After(now) {
			clog.Warningf(ctx, "future mtime corrected for %s in %s", fullname, time.Since(started))
			return nil
		}
	}
	return fmt.Errorf("future mtime on %s: mtime=%s now=%s %s", fullname, mtime, started, now)
}
