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
	stdpath "path"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/golang/glog"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"go.chromium.org/build/hashigo/digest"

	"go.chromium.org/build/siso/blob"
	"go.chromium.org/build/siso/hashfs/osfs"
	pb "go.chromium.org/build/siso/hashfs/proto"
	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/o11y/trace"
	"go.chromium.org/build/siso/path"
	"go.chromium.org/build/siso/reapi/fetch"
	"go.chromium.org/build/siso/reapi/merkletree"
	"go.chromium.org/build/siso/resource"
	"go.chromium.org/build/siso/sync/semaphore"
	"go.chromium.org/build/siso/toolsupport/abfsutil"
	"go.chromium.org/build/siso/toolsupport/cartfsutil"
)

// Linux imposes a limit of at most 40 symlinks in any one path lookup.
// see: https://lwn.net/Articles/650786/
const maxSymlinks = 40

// ForgetMissingsSemaphore is a semaphore to control concurrent ForgetMissings.
// os.Lstat in ForgetMissings would create lots of thread. b/325565625
var ForgetMissingsSemaphore = semaphore.New("fs-forget", runtime.GOMAXPROCS(0)*2)

// FlushSemaphore is the static gate on concurrent flushes. It is the
// default unless the adaptive-flush experiment installs an adaptive
// Option.FlushGate (see ActiveFlushGate).
var FlushSemaphore = semaphore.New("fs-flush", max(runtime.GOMAXPROCS(0)*8, 200))

// FlushGater is the read-only behaviour shared by the static
// FlushSemaphore and the adaptive resource.Network gate, used by the
// progress display and the resource-usage stats table.
type FlushGater interface {
	trace.Semaphore
	Stat() semaphore.Stat
}

// DigestSemaphore returns the semaphore that limits this HashFS's concurrent
// file digests.
func (hfs *HashFS) DigestSemaphore() *semaphore.Semaphore {
	return hfs.digester.sema
}

// ActiveFlushGate reports the flush admission mechanism in use: the
// adaptive Gradient2 gate when the adaptive-flush experiment set
// opt.FlushGate, otherwise the static FlushSemaphore.
func (hfs *HashFS) ActiveFlushGate() FlushGater {
	if hfs.opt.FlushGate != nil {
		return hfs.opt.FlushGate
	}
	return FlushSemaphore
}

// acquireFlush admits one flush through the active gate: the adaptive
// resource.Network when installed, otherwise the static FlushSemaphore.
// Both expose the same release(error) callback.
func acquireFlush(ctx context.Context, gate *resource.Network) (context.Context, func(error), error) {
	if gate != nil {
		return gate.Acquire(ctx)
	}
	return FlushSemaphore.WaitAcquire(ctx)
}

func isExecutable(fi fs.FileInfo, fname string, m map[path.Path]bool) bool {
	if fi.Mode()&0111 != 0 {
		return true
	}
	return m[path.Path(fname)]
}

// NotifyFunc is the type of the function to notify the filesystem changes.
type NotifyFunc func(context.Context, *FileInfo)

// HashFS is a filesystem for digest hash.
type HashFS struct {
	opt       Option
	directory *directory

	notifies []NotifyFunc

	// OS wraps of OS I/O operations in the HashFS.
	OS           *osfs.OSFS
	singleflight singleflight.Group

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
	previouslyGeneratedFiles []path.Path

	// holds tainted files
	taintedFiles []string

	executables map[path.Path]bool

	// writer for updated entries journal.
	journal journalWriter

	// trigger for SetState background goroutine finish.
	setStateCh chan error

	// records missing outputs to make hashfs non-clean when missing
	// outputs exists, so trigger rebuilds b/374179435
	missingOutputs sync.Map

	// dirInputCache memoizes expandDirInputs results within a build, keyed by
	// the directory's full path -> []string of its files.
	dirInputCache sync.Map
	// dirInputCached gates invalidation so builds with no directory inputs pay
	// only an atomic load.
	dirInputCached atomic.Bool

	// ledgerMu protects the build label tracking ledger (buildLabelDictionary and fileBuildLabels)
	// from concurrent read/writes during VFS evictions and state flushes.
	ledgerMu             sync.RWMutex
	buildLabelDictionary map[uint32]*pb.BuildLabelMetadata
	fileBuildLabels      map[string]uint64
}

// New creates a HashFS.
func New(ctx context.Context, opt Option) (*HashFS, error) {
	defer trace.Begin(ctx, "hashfs.New").End()
	if opt.OutputLocal == nil {
		opt.OutputLocal = func(context.Context, string) bool { return false }
	}
	if opt.Ignore == nil {
		opt.Ignore = func(context.Context, string) bool { return false }
	}
	if opt.DataSource == nil {
		opt.DataSource = noDataSource{}
	}
	if opt.DigestFunction.IsZero() {
		opt.DigestFunction = digest.SHA256
	}
	opt.OSFSOption.OnCog = opt.CogFS != nil
	opt.OSFSOption.CartFS = opt.CartFS
	opt.OSFSOption.ABFS = opt.ABFS
	opt.OSFSOption.DigestFunction = opt.DigestFunction
	digestSema := localDigestSemaphore
	if opt.ABFS != nil {
		digestSema = abfsDigestSemaphore
	}
	fsys := &HashFS{
		opt:       opt,
		directory: &directory{isRoot: true},
		OS:        osfs.New(ctx, "fs", opt.OSFSOption),

		digester: digester{
			fn:        opt.DigestFunction,
			abfs:      opt.ABFS,
			quitEarly: opt.DeferDigest,
			sema:      digestSema,
			q:         make(chan digestReq, 1000),
			wake:      make(chan struct{}, 1),
			quit:      make(chan struct{}),
			done:      make(chan struct{}),
		},
		buildLabelDictionary: make(map[uint32]*pb.BuildLabelMetadata),
		fileBuildLabels:      make(map[string]uint64),
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

		// If the build switched -reapi_digest_function since this out
		// dir was last built, discarding the state forces a cold
		// rehash/rebuild instead of trusting stale hashes (or fast
		// no-ops) under the new function.
		var discarded bool
		fstate, discarded = discardOnFunctionMismatch(ctx, opt.DigestFunction, fstate)

		// Recover last build updates from the journal if the previous
		// build didn't finish properly (journal file not removed).
		// Skip the journal when state was corrupted or discarded, since
		// we don't have a valid base state to apply it to.
		var reconciled bool
		if fsys.loadErr == nil && !discarded {
			reconciled = loadJournal(ctx, opt.DigestFunction, journalFile, fstate)
		}
		if err := fsys.SetState(ctx, fstate); err != nil {
			return nil, err
		}
		if reconciled {
			// Save fstate to make it base state for next journaling.
			err := Save(ctx, fstate, opt)
			if err != nil {
				clog.Errorf(ctx, "Failed to save reconciled fs state in %s: %v", opt.StateFile, err)
			}
		}
		err = os.Remove(journalFile)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			clog.Warningf(ctx, "Failed to remove journal: %v", err)
		}

		f, err := osfs.Create(journalFile)
		if err != nil {
			clog.Warningf(ctx, "Failed to create fs state journal: %v", err)
		} else if err := writeJournalHeader(f, opt.DigestFunction); err != nil {
			// Without the header a future load can't tell which digest
			// function produced the entries, so don't journal at all.
			clog.Warningf(ctx, "Failed to write fs state journal header: %v", err)
			f.Close()
		} else {
			fsys.journal.w = f
		}
	}
	go fsys.digester.start(ctx)
	return fsys, nil
}

// DigestFunction returns the content digest function used by this filesystem.
func (hfs *HashFS) DigestFunction() digest.Function {
	return hfs.opt.DigestFunction
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
	defer trace.Begin(ctx, "hashfs.WaitReady").End()
	started := time.Now()
	select {
	case <-ctx.Done():
		clog.Warningf(ctx, "hashfs does not become ready %s: %v", time.Since(started), context.Cause(ctx))
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
func (hfs *HashFS) SetExecutables(ctx context.Context, m map[path.Path]bool) {
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

// shouldSkipSave reports whether the current state should not be
// persisted to disk. Each condition is a reason the in-memory state
// is unreliable or unchanged.
func (hfs *HashFS) shouldSkipSave() bool {
	// is .siso_fs_state doesn't exist, need to save.
	_, err := os.Stat(hfs.opt.StateFile)
	if errors.Is(err, fs.ErrNotExist) {
		return false
	}
	// State matches disk, nothing to write.
	if hfs.clean.Load() {
		return true
	}
	// State was never fully loaded, saving would lose data.
	if !hfs.loaded.Load() {
		return true
	}
	// Deferred-digest mode with no journal updates means no changes.
	if hfs.opt.DeferDigest && hfs.journal.n == 0 {
		return true
	}
	// Tainted files present: state may be corrupted.
	if len(hfs.taintedFiles) > 0 {
		return true
	}
	return false
}

// Close closes the HashFS.
// Persists current state in opt.StateFile.
func (hfs *HashFS) Close(ctx context.Context) error {
	clog.Infof(ctx, "fs close")
	hfs.digester.stop(ctx)
	if hfs.opt.CartFS != nil {
		cerr := hfs.opt.CartFS.Close()
		if cerr != nil {
			clog.Errorf(ctx, "close cartfs: %v", cerr)
		}
	}
	if hfs.opt.ABFS != nil {
		cerr := hfs.opt.ABFS.Close()
		if cerr != nil {
			clog.Errorf(ctx, "close abfs: %v", cerr)
		}
	}
	if hfs.opt.StateFile == "" {
		return nil
	}
	err := hfs.journal.Close()
	if err != nil {
		clog.Warningf(ctx, "Failed to close journal %v", err)
	}
	clog.Infof(ctx, "close journal")
	if hfs.shouldSkipSave() {
		clog.Warningf(ctx, "not save state clean=%t loaded=%t journal:%d tainted:%d", hfs.clean.Load(), hfs.loaded.Load(), hfs.journal.n, len(hfs.taintedFiles))
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
func (hfs *HashFS) PreviouslyGeneratedFiles() []path.Path {
	p := hfs.previouslyGeneratedFiles
	hfs.previouslyGeneratedFiles = nil
	return p
}

// TaintedFiles returns a list of manually modified generated files.
func (hfs *HashFS) TaintedFiles() []string {
	return hfs.taintedFiles
}

// AddMissingOutput adds a missing output.
func (hfs *HashFS) AddMissingOutput(ctx context.Context, root string, fname path.Path) {
	hfs.missingOutputs.Store(string(path.JoinRoot(root, fname)), true)
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

// OnCartFS returns whether it is on CartFS or not.
func (hfs *HashFS) OnCartFS() bool {
	return hfs.opt.CartFS != nil
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

func makeFullpath(root string, fname path.Path) path.Path {
	return path.JoinRoot(root, fname)
}

// MakeFullpath returns the absolute, slash-cleaned path NeedFlush hands the
// OutputLocal predicate, so callers that pre-compute predicate keys (the graph
// classifier) use the same form.
func MakeFullpath(root, fname string) string {
	return string(makeFullpath(root, path.New(fname)))
}

func (hfs *HashFS) dirLookup(ctx context.Context, root string, fname path.Path) (*entry, path.Path, *directory, bool) {
	if fname.IsAbs() {
		return hfs.directory.lookup(ctx, fname)
	}
	if needPathClean(root, string(fname)) {
		return hfs.directory.lookup(ctx, path.JoinRoot(root, fname))
	}
	e, _, _, ok := hfs.directory.lookup(ctx, path.Path(root))
	if !ok {
		return nil, fname, nil, false
	}
	if !e.isDirectory() {
		return nil, fname, nil, false
	}
	e, dir, resolved, ok := e.directory.lookupEntry(ctx, fname)
	if ok {
		return e, fname, dir, true
	}
	if resolved != "" {
		resolvedName := resolved
		if !resolved.IsAbs() {
			resolvedName = path.JoinRoot(root, resolved)
		}
		return hfs.directory.lookup(ctx, resolvedName)
	}
	return nil, fname, nil, false
}

// commitEntry persists a mutation: stores the entry in the directory
// tree, triggers digest computation, notifies observers, and journals.
func (hfs *HashFS) commitEntry(ctx context.Context, fname path.Path, e *entry) error {
	ee, err := hfs.directory.store(ctx, fname, e)
	if err != nil {
		return err
	}
	hfs.digester.lazyCompute(ctx, string(fname), ee)
	for _, f := range hfs.notifies {
		f(ctx, &FileInfo{fname: fname, e: ee})
	}
	hfs.journalEntry(ctx, string(fname), ee)
	return nil
}

// getOrCreateEntry looks up fname in the directory tree, creating and
// storing a new local entry from disk if not found.
// Returns the entry and the resolved fname (symlinks in intermediate
// path components followed).
func (hfs *HashFS) getOrCreateEntry(ctx context.Context, fname path.Path) (*entry, path.Path, error) {
	e, fname, _, ok := hfs.directory.lookup(ctx, fname)
	if ok {
		e.mu.Lock()
		err := e.err
		e.mu.Unlock()
		if err != nil {
			return nil, fname, err
		}
		return e, fname, nil
	}
	e = newLocalEntry()
	e.init(ctx, string(fname), hfs.executables, hfs.OS)
	if errors.Is(e.err, context.Canceled) {
		return nil, fname, e.err
	}
	e, err := hfs.directory.store(ctx, fname, e)
	if err != nil {
		clog.Warningf(ctx, "failed to store %s %s: %v", fname, e, err)
		return nil, fname, err
	}
	if e.err != nil {
		return nil, fname, e.err
	}
	clog.Infof(ctx, "stat new entry %s %s", fname, e)
	return e, fname, nil
}

// statOpts controls optional behaviors of HashFS.stat.
type statOpts struct {
	needCompute         bool // queue a digest compute for a new entry
	skipDirMtimeRefresh bool // skip dir cache-hit mtime-refresh Lstat (StatIfExists)
}

// Stat returns a FileInfo at root/fname.
func (hfs *HashFS) Stat(ctx context.Context, root string, fname path.Path) (FileInfo, error) {
	return hfs.stat(ctx, root, fname, statOpts{needCompute: true})
}

// StatIfExists is like Stat but skips the dir cache-hit mtime-refresh Lstat.
// Use when only existence/type is needed, not a fresh mtime/size.
func (hfs *HashFS) StatIfExists(ctx context.Context, root string, fname path.Path) (FileInfo, error) {
	return hfs.stat(ctx, root, fname, statOpts{needCompute: true, skipDirMtimeRefresh: true})
}

// statTracer tracks the execution time of each step in HashFS.stat.
// It uses fixed-size arrays to avoid heap allocations in the fast path (when total time < 200ms),
// preventing regressions in allocation tests like TestStatAllocs.
type statTracer struct {
	start     time.Time
	stepStart time.Time
	names     [10]string
	durs      [10]time.Duration
	count     int
}

func (t *statTracer) record(name string) {
	if t.count < len(t.names) {
		t.names[t.count] = name
		t.durs[t.count] = time.Since(t.stepStart)
		t.count++
	}
	t.stepStart = time.Now()
}

// StatMtime is like Stat but does not queue a digest for new entries.
// Use when only mtime, size, cmdhash, or isChanged is needed: a queued
// digest would race a later Update that replaces the entry, forcing a
// second read on the replacement.
// In -fs_defer_digest mode it queues the digest anyway, since reload repairs missing digests only in non-defer mode and a digestless input disables fast-nop.
func (hfs *HashFS) StatMtime(ctx context.Context, root string, fname path.Path) (FileInfo, error) {
	return hfs.stat(ctx, root, fname, statOpts{needCompute: hfs.opt.DeferDigest})
}

// stat looks up or creates the hashfs entry for root/fname.
// See statOpts for the optional directory-Lstat short-circuits.
func (hfs *HashFS) stat(ctx context.Context, root string, fname path.Path, opts statOpts) (FileInfo, error) {
	var tracer statTracer
	tracer.start = time.Now()
	tracer.stepStart = tracer.start
	defer func(t *statTracer) {
		total := time.Since(t.start)
		if total > 200*time.Millisecond {
			var parts []string
			for i := range t.count {
				parts = append(parts, fmt.Sprintf("%s:%s", t.names[i], t.durs[i]))
			}
			clog.Infof(ctx, "stat slow %s/%s: total %s, steps: %s", root, fname, total, strings.Join(parts, ", "))
		}
	}(&tracer)
	if log.V(1) {
		clog.Infof(ctx, "stat @%s %s", root, fname)
	}
	// fast path
	e, fname, _, ok := hfs.dirLookup(ctx, root, fname)
	tracer.record("dirLookup")
	if log.V(1) {
		clog.Infof(ctx, "stat @%s -> %s", root, fname)
	}
	if ok {
		e, err := hfs.statHit(ctx, root, fname, e, opts, &tracer)
		if err != nil {
			return FileInfo{}, err
		}
		return FileInfo{root: root, fname: fname, e: e}, nil
	}
	// slow path
	fullname := makeFullpath(root, fname)
	for {
		ev, err, _ := hfs.singleflight.Do(string(fullname), func() (any, error) {
			return hfs.statNewEntry(ctx, root, fname, fullname, opts, &tracer)
		})
		if ctx.Err() == nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
			// if singleflight one was canceled, but current ctx is still active, retry.
			clog.Warningf(ctx, "retry statNewEntry %q: %v", fullname, err)
			continue
		}
		if err != nil {
			return FileInfo{}, err
		}
		e = ev.(*entry)
		break
	}
	return FileInfo{root: root, fname: fname, e: e}, nil
}

func (hfs *HashFS) statHit(ctx context.Context, root string, fname path.Path, e *entry, opts statOpts, tracer *statTracer) (*entry, error) {
	e.mu.Lock()
	err := e.err
	e.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if e.isDirectory() {
		if opts.skipDirMtimeRefresh {
			// Skip the refresh Lstat: existence-only caller (StatIfExists).
			return e, nil
		}
		// directory's mtime has been updated locally
		// where hashfs doesn't know. e.g. add new file
		// in the directory by local run.
		fullname := makeFullpath(root, fname)
		lfi, err := hfs.OS.Lstat(ctx, string(fullname))
		tracer.record("lstat")
		switch {
		case errors.Is(err, fs.ErrNotExist):
			// A build-without-bytes directory output (recorded from an action,
			// not local) lives in CAS, not on disk, so report it present. A
			// directory with no action digest stays reported missing.
			e.mu.Lock()
			buildWithoutBytes := !e.local && len(e.cmdhash) > 0 && !e.action.IsZero()
			e.mu.Unlock()
			if buildWithoutBytes {
				return e, nil
			}
			// virtually created dir in hashfs, so no need to update mtime.
			clog.Infof(ctx, "stat hashfs dir %s. doesn't exist in local", fullname)
			return e, err
		case errors.Is(err, context.Canceled):
			// Context canceled is normal during shutdown
			// or when a racing goroutine loses; not
			// unexpected.
			return e, err
		case err != nil:
			clog.Warningf(ctx, "unexpected dir stat fail %s: %v", fullname, err)
			return e, err
		default:
			mtime := lfi.ModTime()
			// adjust for clock stepback by NTP
			err = waitUntilModTime(ctx, string(fullname), mtime)
			tracer.record("waitUntilModTime")
			if err != nil {
				return e, err
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
	return e, nil
}

func (hfs *HashFS) statNewEntry(ctx context.Context, root string, fname, fullname path.Path, opts statOpts, tracer *statTracer) (*entry, error) {
	// check again for racing singlefight statNewEntry.
	e, fname, dir, ok := hfs.dirLookup(ctx, root, fname)
	tracer.record("dirLookup")
	if log.V(1) {
		clog.Infof(ctx, "stat @%s -> %s", root, fname)
	}
	if ok {
		return hfs.statHit(ctx, root, fname, e, opts, tracer)
	}
	e = newLocalEntry()
	e.init(ctx, string(fullname), hfs.executables, hfs.OS)
	tracer.record("init")
	if log.V(1) {
		clog.Infof(ctx, "stat new entry %s %s", fullname, e)
	}
	if errors.Is(e.err, context.Canceled) {
		return e, e.err
	}
	var err error
	if dir != nil {
		e, err = dir.store(ctx, path.Path(filepath.Base(string(fullname))), e)
		if errors.Is(err, errRootSymlink) {
			e, err = hfs.directory.store(ctx, fullname, e)
		}
	} else {
		e, err = hfs.directory.store(ctx, fullname, e)
	}
	tracer.record("store")
	if err != nil {
		clog.Warningf(ctx, "failed to store %s %s in %s: %v", fullname, e, dir, err)
		return e, err
	}
	if e.err != nil {
		return e, e.err
	}
	if opts.needCompute {
		hfs.digester.lazyCompute(ctx, string(fullname), e)
		tracer.record("lazyCompute")
	}
	return e, nil
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
func (hfs *HashFS) ReadDir(ctx context.Context, root string, name path.Path) (dents []DirEntry, err error) {
	ctx, span := trace.NewSpan(ctx, "read-dir")
	defer span.Close(nil)
	if log.V(1) {
		clog.Infof(ctx, "readdir @%s %s", root, name)
		defer func() {
			clog.Infof(ctx, "readdir @%s %s -> %d %v", root, name, len(dents), err)
		}()
	}
	dname := makeFullpath(root, name)
	e, dname, err := hfs.getOrCreateEntry(ctx, dname)
	if err != nil {
		return nil, fmt.Errorf("read dir %s: %w", dname, err)
	}
	if e.isSymlink() {
		relDname, err := filepath.Rel(root, string(dname))
		if err != nil || !filepath.IsLocal(relDname) {
			clog.Warningf(ctx, "read dir: symlink rel root %q: %v", dname, err)
			return nil, SymlinkError{Path: string(dname), Target: e.target}
		}
		return nil, SymlinkError{Path: relDname, Target: e.target}
	}
	if !e.isDirectory() {
		return nil, fmt.Errorf("read dir %s: not dir: %w", dname, os.ErrPermission)
	}
	// TODO(ukai): fix race in updateDir -> store.
	names := e.updateDir(ctx, hfs, string(dname))
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
				fname: dname.Join(name),
				e:     ee,
			},
		})
		return true
	})
	return ents, nil
}

// probeAtEOF reports whether rd is exhausted: a one-byte read returns 0 bytes at io.EOF (a byte with io.EOF means data remains).
func probeAtEOF(rd io.Reader) bool {
	var b [1]byte
	n, err := rd.Read(b[:])
	return n == 0 && err == io.EOF
}

// ReadFile reads a contents of root/fname.
func (hfs *HashFS) ReadFile(ctx context.Context, root string, fname path.Path) ([]byte, error) {
	ctx, span := trace.NewSpan(ctx, "read-file")
	defer span.Close(nil)
	if log.V(1) {
		clog.Infof(ctx, "readfile @%s %s", root, fname)
	}
	fullname := makeFullpath(root, fname)
	span.SetAttr("fname", string(fullname))
	e, fullname, err := hfs.getOrCreateEntry(ctx, fullname)
	if err != nil {
		return nil, fmt.Errorf("read file %s: %w", fullname, err)
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
		lfi, err := hfs.OS.Lstat(ctx, string(fullname))
		// check it is already flushed to disk or not.
		if err != nil || !e.getMtime().Equal(lfi.ModTime()) || ed.SizeBytes != lfi.Size() {
			// not yet flushed, read from CAS
			buf, err := blob.DataToBytes(ctx, blob.NewData(e.src, ed))
			if log.V(1) {
				clog.Infof(ctx, "readfile(%s) %s: %v", ed, fullname, err)
			}
			return buf, err
		}
		// already flushed. reading from local disk is faster.
	}
	if e.isSymlink() {
		relFname, err := filepath.Rel(root, string(fullname))
		if err != nil || !filepath.IsLocal(relFname) {
			clog.Warningf(ctx, "readfile: symlink rel root %q: %v", fullname, err)
			return nil, SymlinkError{Path: string(fullname), Target: e.target}
		}
		return nil, SymlinkError{Path: relFname, Target: e.target}
	}
	if e.src == nil {
		return nil, fmt.Errorf("readfile %s: no src", fullname)
	}
	src := hfs.OS.FileSource(string(fullname), -1)
	rd, err := src.Open(ctx)
	if err != nil {
		return nil, fmt.Errorf("readfile %s: %w", fullname, err)
	}
	defer rd.Close()
	size := max(e.size, 0)
	buf := make([]byte, size)
	_, err = io.ReadFull(rd, buf)
	if log.V(1) {
		clog.Infof(ctx, "readfile(disk) %s: %v", fullname, err)
	}
	// If buf is the whole file (probeAtEOF), digest it now to save the async
	// reopen+reread; a stale e.size leaves more on disk, so defer to lazyCompute.
	// noLazyForTests is honored here too, matching lazyCompute.
	if err == nil && (noLazyForTests == nil || !noLazyForTests[string(fullname)]) && probeAtEOF(rd) {
		e.mu.RLock()
		needDigest := e.d.IsZero()
		e.mu.RUnlock()
		if needDigest {
			d := blob.FromBytes(hfs.opt.DigestFunction, string(fullname), buf).Digest()
			e.mu.Lock()
			if e.d.IsZero() {
				e.d = d
			}
			e.mu.Unlock()
		}
		return buf, err
	}
	hfs.digester.lazyCompute(ctx, string(fullname), e)
	return buf, err
}

// WriteFile writes a contents in root/fname with mtime and cmdhash, edgehash.
func (hfs *HashFS) WriteFile(ctx context.Context, root string, fname path.Path, b []byte, isExecutable bool, mtime time.Time, cmdhash, edgehash []byte) error {
	ctx, span := trace.NewSpan(ctx, "write-file")
	defer span.Close(nil)
	if log.V(1) {
		clog.Infof(ctx, "writefile @%s %s x:%t mtime:%s", root, fname, isExecutable, mtime)
	}
	hfs.clean.Store(false)
	data := blob.FromBytes(hfs.opt.DigestFunction, string(fname), b)
	fullname := makeFullpath(root, fname)
	span.SetAttr("fname", string(fullname))
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
	err := hfs.commitEntry(ctx, fullname, e)
	if err == nil {
		hfs.invalidateDirInputCache(string(fullname))
	}
	clog.Infof(ctx, "writefile %s x:%t mtime:%s: %v", fullname, isExecutable, mtime, err)
	return err
}

// Symlink creates a symlink to target at root/linkpath with mtime and cmdhash, edgehash.
func (hfs *HashFS) Symlink(ctx context.Context, root, target string, linkpath path.Path, mtime time.Time, cmdhash, edgehash []byte) error {
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
	err := hfs.commitEntry(ctx, linkfname, e)
	if err == nil {
		hfs.invalidateDirInputCache(string(linkfname))
	}
	clog.Infof(ctx, "symlink @%s %s -> %s: %v", root, linkpath, target, err)
	return err
}

// Copy copies a file from root/src to root/dst with mtime and cmdhash, edgehash.
// if src is dir, returns error.
func (hfs *HashFS) Copy(ctx context.Context, root string, src, dst path.Path, mtime time.Time, cmdhash, edgehash []byte) error {
	if log.V(1) {
		clog.Infof(ctx, "copy @%s %s to %s", root, src, dst)
	}
	hfs.clean.Store(false)
	srcfname := makeFullpath(root, src)
	dstfname := makeFullpath(root, dst)
	e, _, err := hfs.getOrCreateEntry(ctx, srcfname)
	if err != nil {
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
	if !e.isSymlink() {
		hfs.digester.compute(ctx, string(srcfname), e)
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
	err = hfs.commitEntry(ctx, dstfname, newEnt)
	if err == nil {
		hfs.invalidateDirInputCache(string(dstfname))
	}
	clog.Infof(ctx, "copy %s to %s: %v", srcfname, dstfname, err)
	return err
}

// Mkdir makes a directory at root/dirname.
func (hfs *HashFS) Mkdir(ctx context.Context, root string, dirname path.Path, cmdhash, edgehash []byte) error {
	if log.V(1) {
		clog.Infof(ctx, "mkdir @%s %s", root, dirname)
	}
	hfs.clean.Store(false)
	dirname = makeFullpath(root, dirname)
	fi, err := hfs.OS.Lstat(ctx, string(dirname))
	mtime := time.Now()
	if err == nil && fi.IsDir() {
		err := hfs.OS.Chtimes(ctx, string(dirname), time.Time{}, mtime)
		if err != nil {
			clog.Warningf(ctx, "failed to set dir mtime %s: %v: %v", dirname, mtime, err)
		}
	} else {
		err := hfs.OS.MkdirAll(ctx, string(dirname), 0755)
		if err != nil {
			return err
		}
		fi, err := hfs.OS.Lstat(ctx, string(dirname))
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
	ee, err := hfs.directory.store(ctx, dirname, e)
	if err == nil {
		hfs.digester.lazyCompute(ctx, string(dirname), ee)
		for _, f := range hfs.notifies {
			f(ctx, &FileInfo{fname: dirname, e: ee})
		}
	}
	if serr, ok := errors.AsType[storeRaceError](err); ok {
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
	hfs.invalidateDirInputCache(string(dirname))
	if len(cmdhash) > 0 {
		hfs.journalEntry(ctx, string(dirname), e)
	}
	return nil
}

// Remove removes a file at root/fname.
func (hfs *HashFS) Remove(ctx context.Context, root string, fname path.Path) error {
	if log.V(1) {
		clog.Infof(ctx, "remove @%s %s", root, fname)
	}
	hfs.clean.Store(false)
	fname = makeFullpath(root, fname)
	fnameStr := string(fname)
	hfs.WithBuildLabelTrackingLockDo(func() {
		delete(hfs.fileBuildLabels, fnameStr)
	})
	lready := make(chan bool, 1)
	lready <- true
	e := &entry{
		lready: lready,
		err:    fs.ErrNotExist,
	}
	_, err := hfs.directory.store(ctx, fname, e)
	hfs.invalidateDirInputCache(string(fname))
	clog.Infof(ctx, "remove %s: %v", fname, err)
	return err
}

// WithBuildLabelTrackingLockDo grabs the ledgerMu and executes callback
// if and only if a build label was requested for this invocation.
func (hfs *HashFS) WithBuildLabelTrackingLockDo(f func()) {
	if hfs.opt.BuildLabel == "" {
		return
	}
	hfs.ledgerMu.Lock()
	defer hfs.ledgerMu.Unlock()
	f()
}

// RemoveAll removes all files under root/name.
// Also removes from the disk at the same time.
func (hfs *HashFS) RemoveAll(ctx context.Context, root string, name path.Path) error {
	return hfs.removeAll(ctx, root, name, false)
}

// removeAll removes root/name from disk and reconciles the records.
// forget drops the record on success instead of recording the path absent: a
// not-exist entry is authoritative and would shadow a recreated tree.
func (hfs *HashFS) removeAll(ctx context.Context, root string, name path.Path, forget bool) error {
	if log.V(1) {
		clog.Infof(ctx, "removeAll @%s %s", root, name)
	}
	hfs.clean.Store(false)
	name = makeFullpath(root, name)
	nameStr := string(name)
	prefix := nameStr + "/"
	hfs.WithBuildLabelTrackingLockDo(func() {
		for k := range hfs.fileBuildLabels {
			if k == nameStr || strings.HasPrefix(k, prefix) {
				delete(hfs.fileBuildLabels, k)
			}
		}
	})
	// Route through OSFS.RemoveAll for its metrics and slow-operation logging.
	if err := hfs.OS.RemoveAll(ctx, string(name)); err != nil {
		// A failed RemoveAll always leaves the root on disk, so keep the
		// recorded entry: dropping it would drop the path from saved state,
		// and clean-dead would never retry. Children RemoveAll did delete
		// must go: a stale entry would satisfy Stat for a missing path.
		hfs.dropRemovedChildren(ctx, name)
		hfs.invalidateDirInputCache(string(name))
		clog.Warningf(ctx, "removeAll %q: %v", name, err)
		return err
	}
	if forget {
		hfs.directory.deleteForce(ctx, name)
		hfs.invalidateDirInputCache(string(name))
		clog.Infof(ctx, "removeAll %q: forget", name)
		return nil
	}
	lready := make(chan bool, 1)
	lready <- true
	e := &entry{
		lready: lready,
		err:    fs.ErrNotExist,
	}
	_, storeErr := hfs.directory.store(ctx, name, e)
	hfs.invalidateDirInputCache(string(name))
	clog.Infof(ctx, "removeAll %q", name)
	return storeErr
}

// dropRemovedChildren drops recorded entries under name whose on-disk path a
// partially failed RemoveAll already deleted. Only a path proven absent by
// Lstat is dropped (with its whole subtree); anything still on disk, or that
// cannot be checked, stays recorded.
func (hfs *HashFS) dropRemovedChildren(ctx context.Context, name path.Path) {
	e, _, _, ok := hfs.directory.lookup(ctx, name)
	if !ok || e == nil {
		return
	}
	hfs.dropRemovedChildrenIn(ctx, name, e.getDir())
}

func (hfs *HashFS) dropRemovedChildrenIn(ctx context.Context, dirname path.Path, d *directory) {
	if d == nil {
		return
	}
	d.m.Range(func(k, v any) bool {
		base := k.(string)
		child := v.(*entry)
		fname := dirname.Join(base)
		_, err := hfs.OS.Lstat(ctx, string(fname))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			// Use CompareAndDelete so a concurrent re-store is never clobbered.
			d.m.CompareAndDelete(base, child)
			clog.Infof(ctx, "removeAll: drop removed child %q", fname)
		case err == nil:
			hfs.dropRemovedChildrenIn(ctx, fname, child.getDir())
		}
		return true
	})
}

// ResetDirOutput removes a directory output (disk tree and recorded subtree)
// before its step runs, so a rerun with fewer files leaves no stale members.
// Only the parent is ensured; contents are re-recorded from disk after the
// step runs. Reconciliation is removeAll's, so a wipe cannot drift from a
// clean-dead one.
func (hfs *HashFS) ResetDirOutput(ctx context.Context, root, name string) error {
	target := path.New(name)
	if err := hfs.removeAll(ctx, root, target, true); err != nil {
		return err
	}
	fullpath := makeFullpath(root, target)
	return hfs.OS.MkdirAll(ctx, filepath.Dir(string(fullpath)), 0755)
}

// RemoveStaleDirMembers removes on-disk entries under the directory output dir
// that hashfs does not record as members of it. The remote/cache path records a
// fresh tree (ForgetOutputs + Update) and flushes it, but a flush only adds
// files; it never removes members the new tree dropped. So a directory output
// that shrank across builds, materialized locally, would keep the prior build's
// extra files on disk. Local execution avoids this with ResetDirOutput's pre-run
// wipe; this is its remote/cache counterpart.
//
// A member is recorded with dir's own cmdhash, stamped on the directory, each
// subdirectory and each file. Ownership rather than mere presence is the test
// because a ReadDir walk picks up still-on-disk files into hashfs (updateDir)
// as unowned records, without a cmdhash, so a stale member that a walk
// already picked up is still pruned.
// Nesting a produced output under a directory output is a graph-load error
// (validateDirOutputs), so nothing else can legitimately live under dir.
//
// A stale member may still carry an unowned hashfs record, so each
// removal goes through removeAll, which drops the records of whatever it
// deleted, even on a partial failure, and invalidates the affected
// directory-input expansions. Prunes only stale entries (no wholesale wipe +
// re-flush), so an unchanged tree pays one walk.
//
// Deletion failures are logged and ignored; traversal errors other than
// missing paths are returned to the caller. Deletion is best-effort like
// cleandead: a member that can't be removed (e.g. held open on Windows) is left
// behind rather than failing the step. A traversal error means part of the tree
// could not be checked at all, so the prune cannot vouch for the directory and
// fails instead, as ResetDirOutput's wipe would on the local exec path.
func (hfs *HashFS) RemoveStaleDirMembers(ctx context.Context, root, dir string) error {
	dirFull := makeFullpath(root, path.New(dir))
	de, _, _, ok := hfs.directory.lookup(ctx, dirFull)
	if !ok || de == nil || len(de.cmdhash) == 0 {
		// Not a recorded directory output: nothing authoritative to prune against.
		return nil
	}
	cmdhash := de.cmdhash
	return filepath.WalkDir(string(dirFull), func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, fs.ErrNotExist) {
				return nil // not materialized yet: nothing to prune
			}
			return walkErr
		}
		fname := path.New(p)
		if fname == dirFull || hfs.ownedBy(ctx, fname, cmdhash) {
			return nil // the directory root, or a member of the new tree
		}
		// A stale entry: remove it. removeAll covers a file, a symlink (the link,
		// not its target), or a whole stale subtree; skip descending a removed dir.
		// forget drops the record rather than recording the path absent, as
		// ResetDirOutput does: a later build may recreate it.
		if rmErr := hfs.removeAll(ctx, root, fname, true); rmErr != nil {
			clog.Warningf(ctx, "prune stale dir member %s: %v", p, rmErr)
		}
		if d.IsDir() {
			return fs.SkipDir
		}
		return nil
	})
}

// ownedBy reports whether hashfs records fname as produced by cmdhash.
func (hfs *HashFS) ownedBy(ctx context.Context, fname path.Path, cmdhash []byte) bool {
	e, _, _, ok := hfs.directory.lookup(ctx, fname)
	if !ok || e == nil || e.err != nil {
		return false
	}
	return bytes.Equal(e.cmdhash, cmdhash)
}

// ClearStaleFileForDirOutput removes name (and its hashfs subtree) when a
// previous build left a non-directory where a directory output is now
// declared, so the action can create its directory there.
//
// The reverse (a directory where a file output is now declared) is NOT handled:
// a directory-valued output produced without the trailing-slash convention is
// also recorded as a directory, so a directory entry under a non-slash target
// is ambiguous; clearing it is the producing rule's responsibility.
func (hfs *HashFS) ClearStaleFileForDirOutput(ctx context.Context, root, name string) error {
	fullpath := makeFullpath(root, path.New(name))
	stale := false
	if e, _, _, ok := hfs.directory.lookup(ctx, fullpath); ok {
		e.mu.Lock()
		recorded := e.err == nil
		isDir := recorded && e.isDirectory()
		e.mu.Unlock()
		switch {
		case isDir:
			return nil
		case recorded:
			stale = true
		}
	}
	if !stale {
		// No usable in-memory record (reset state, version bump, or a file
		// produced outside siso): a non-directory on disk is still stale and
		// would make MkdirAll fail ENOTDIR.
		if fi, err := hfs.OS.Lstat(ctx, string(fullpath)); err == nil && !fi.IsDir() {
			stale = true
		}
	}
	if !stale {
		return nil
	}
	clog.Warningf(ctx, "directory output %s was a file in a previous build; removing stale file", name)
	return hfs.RemoveAll(ctx, root, path.New(name))
}

// Forget forgets cached entry for inputs under root.
func (hfs *HashFS) Forget(ctx context.Context, root string, inputs []path.Path) {
	hfs.WithBuildLabelTrackingLockDo(func() {
		for _, fname := range inputs {
			fullname := makeFullpath(root, fname)
			fullnameStr := string(fullname)
			delete(hfs.fileBuildLabels, fullnameStr)
		}
	})

	for _, fname := range inputs {
		fullname := makeFullpath(root, fname)
		hfs.directory.delete(ctx, fullname)
		hfs.invalidateDirInputCache(string(fullname))
	}
}

// ForgetOutputs forgets cached entries for exact outputs under root.
// Unlike Forget, it drops directory subtrees because output directories are
// owned by the step being invalidated.
func (hfs *HashFS) ForgetOutputs(ctx context.Context, root string, outputs []path.Path) {
	if len(outputs) == 0 {
		return
	}

	// Build a lookup map of normalized output paths.
	outputMap := make(map[string]bool, len(outputs))
	for _, fname := range outputs {
		fullname := makeFullpath(root, fname)
		fullnameStr := string(fullname)
		outputMap[fullnameStr] = true
	}

	hfs.WithBuildLabelTrackingLockDo(func() {
		// Scan fileBuildLabels exactly once for all outputs.
		for k := range hfs.fileBuildLabels {
			p := k
			for {
				if outputMap[p] {
					delete(hfs.fileBuildLabels, k)
					break
				}
				dir := stdpath.Dir(p)
				if dir == p || dir == "." || dir == "/" {
					break
				}
				p = dir
			}
		}
	})

	for _, fname := range outputs {
		fullname := makeFullpath(root, fname)
		hfs.directory.deleteForce(ctx, fullname)
	}
}

// ForgetMissingsInDir forgets cached entry under root/dir if it isn't
// generated files/dirs by any steps and doesn't exist on local disk.
// It is used for a step that removes files under a dir. b/350662100
func (hfs *HashFS) ForgetMissingsInDir(ctx context.Context, root string, dir path.Path) {
	hfs.invalidateDirInputCache(string(makeFullpath(root, dir)))
	inputs := []path.Path{dir}
	var needCheck []path.Path
	for len(inputs) > 0 {
		fname := inputs[0]
		copy(inputs, inputs[1:])
		inputs = inputs[:len(inputs)-1]
		fi, err := hfs.Stat(ctx, root, fname)
		if errors.Is(err, fs.ErrNotExist) {
			// Stat reports the path gone on local disk. If it is a cached
			// directory, Stat does not drop children, so prune any stale
			// non-generated leaves while keeping directory nodes anchored for
			// concurrent child stores.
			fullname := makeFullpath(root, fname)
			hfs.directory.deleteNotGenerated(ctx, fullname)
			continue
		}
		if err == nil {
			if fi.IsDir() {
				// Descend even into generated directories: a generated output dir
				// (cmdhash-stamped) can still hold children the step removed on
				// disk, which this reconcile must prune (b/350662100).
				dents, err := hfs.ReadDir(ctx, root, fname)
				if err != nil {
					clog.Warningf(ctx, "readdir failed for %q: %v", fname, err)
					needCheck = append(needCheck, fname)
					continue
				}
				for _, dent := range dents {
					inputs = append(inputs, fname.Join(dent.Name()))
				}
			}
			if fi.e.isGenerated() {
				// The entry itself is generated (owned by a step); keep it (don't
				// add to the prune list). Its non-generated removed children were
				// still queued above.
				continue
			}
		}
		needCheck = append(needCheck, fname)
	}
	err := ForgetMissingsSemaphore.Do(ctx, func(ctx context.Context) error {
		for _, fname := range needCheck {
			fullname := makeFullpath(root, fname)
			_, err := hfs.OS.Lstat(ctx, string(fullname))
			if errors.Is(err, fs.ErrNotExist) {
				clog.Infof(ctx, "forget missing %s", fullname)
				// Proven gone on disk (Lstat ErrNotExist): prune stale
				// non-generated entries, but keep directory nodes anchored for
				// concurrent child stores.
				hfs.directory.deleteNotGenerated(ctx, fullname)
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
func (hfs *HashFS) ForgetMissings(ctx context.Context, root string, inputs []path.Path) []path.Path {
	availables := make([]path.Path, 0, len(inputs))
	needCheck := make([]path.Path, 0, len(inputs))
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
			_, err := hfs.OS.Lstat(ctx, string(fullname))
			if errors.Is(err, fs.ErrNotExist) {
				clog.Infof(ctx, "forget missing %s", fullname)
				// Proven gone on disk (Lstat ErrNotExist): drop the whole stale
				// subtree. The child-preserving delete never evicts a directory
				// node, so it would leave a removed populated dir's children
				// reachable.
				hfs.directory.deleteForce(ctx, fullname)
				hfs.invalidateDirInputCache(string(fullname))
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
func (hfs *HashFS) Availables(ctx context.Context, root string, inputs []path.Path) []path.Path {
	availables := make([]path.Path, 0, len(inputs))
	for _, fname := range inputs {
		if ctx.Err() != nil {
			// Context canceled; return what we have so far
			// rather than logging warnings for every
			// remaining input.
			return availables
		}
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

// escapesRoot reports whether path lies outside the workspace rooted
// at root.
func escapesRoot(root, path string) bool {
	return !strings.HasPrefix(path, root+"/")
}

// resolveEscapingSymlink follows a symlink chain that escapes root,
// resolving through external targets (e.g. ../.cipd/pkgs/..), and
// returns me updated with the final resolved data. The caller must
// verify that e is a symlink whose first hop escapes root.
func (hfs *HashFS) resolveEscapingSymlink(ctx context.Context, root string, fname path.Path, e *entry, me merkletree.Entry) (merkletree.Entry, error) {
	name := path.JoinRoot(root, fname)
	elink := e
	for range maxSymlinks {
		tname := makeFullpath(filepath.Dir(string(name)), path.Path(elink.target))
		if log.V(1) {
			clog.Infof(ctx, "symlink %s -> %s", name, tname)
		}
		if !escapesRoot(root, string(tname)) {
			break
		}
		// symlink to outside of workspace (e.g. ../.cipd/pkgs/..)
		name = tname
		var ok bool
		elink, _, _, ok = hfs.directory.lookup(ctx, name)
		if ok {
			if log.V(2) {
				clog.Infof(ctx, "tree cache hit %s", name)
			}
		} else {
			elink = newLocalEntry()
			elink.init(ctx, string(name), hfs.executables, hfs.OS)
			if log.V(1) {
				clog.Infof(ctx, "tree new entry %s", name)
			}
			var err error
			elink, err = hfs.directory.store(ctx, name, elink)
			if err != nil {
				return merkletree.Entry{}, err
			}
			hfs.digester.lazyCompute(ctx, string(name), elink)
		}
		if elink.err != nil || !elink.isSymlink() {
			break
		}
	}
	clog.Infof(ctx, "resolve symlink %s to %s", fname, name)
	hfs.digester.compute(ctx, string(name), elink)
	d := elink.digest()
	me.Data = blob.NewData(elink.src, d)
	me.IsExecutable = elink.mode&0111 != 0
	me.Target = elink.target
	return me, nil
}

// Entries gets merkletree entries for inputs at root.
// it won't return entries symlink escaped from root.
// root can be an empty string "" when inputs are absolute paths.
func (hfs *HashFS) Entries(ctx context.Context, root string, inputs []path.Path) ([]merkletree.Entry, error) {
	ctx, span := trace.NewSpan(ctx, "fs-entries")
	defer span.Close(nil)
	return hfs.entries(ctx, root, inputs, true)
}

// RawEntries gets merkletree entries for inputs at root.
// it will return symlink entries as is, even if it escaped from root.
// root can be an empty string "" when inputs are absolute paths.
func (hfs *HashFS) RawEntries(ctx context.Context, root string, inputs []path.Path) ([]merkletree.Entry, error) {
	ctx, span := trace.NewSpan(ctx, "fs-raw-entries")
	defer span.Close(nil)
	return hfs.entries(ctx, root, inputs, false)
}

func (hfs *HashFS) entries(ctx context.Context, root string, inputs []path.Path, resolveEscapedSymlinks bool) ([]merkletree.Entry, error) {
	if log.V(1) {
		clog.Infof(ctx, "entries resolveEscapedSymlinks=%t", resolveEscapedSymlinks)
	}
	inputs = path.Paths(hfs.expandDirInputs(ctx, root, path.Strings(inputs)))
	ents, err := hfs.resolveInputEntries(ctx, root, inputs)
	if err != nil {
		return nil, err
	}
	return hfs.buildMerkletreeEntries(ctx, root, inputs, ents, resolveEscapedSymlinks)
}

// expandDirInputs replaces each trailing-slash directory-target input with the
// files it contains, recursively, because REAPI input trees are built from
// files. Inputs without a trailing slash are returned unchanged.
func (hfs *HashFS) expandDirInputs(ctx context.Context, root string, inputs []string) []string {
	hasDir := false
	for _, in := range inputs {
		if strings.HasSuffix(in, "/") {
			hasDir = true
			break
		}
	}
	if !hasDir {
		return inputs
	}
	fsys := hfs.FileSystem(ctx, root)
	expanded := make([]string, 0, len(inputs))
	for _, input := range inputs {
		if !strings.HasSuffix(input, "/") {
			expanded = append(expanded, input)
			continue
		}
		dir := strings.TrimSuffix(input, "/")
		cacheKey := string(makeFullpath(root, path.New(dir)))
		if v, ok := hfs.dirInputCache.Load(cacheKey); ok {
			expanded = append(expanded, v.([]string)...)
			continue
		}
		var files, subdirs []string
		expandDirInputsWalks.Add(1)
		err := fs.WalkDir(fsys, dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if p != dir {
					subdirs = append(subdirs, p)
				}
				return nil
			}
			files = append(files, p)
			return nil
		})
		if err != nil {
			// Fall back to the directory path so resolveInputEntries surfaces
			// the missing/erroring entry.
			clog.Warningf(ctx, "expand dir input %s: %v", input, err)
			expanded = append(expanded, dir)
			continue
		}
		if len(files) == 0 {
			// Keep the bare dir so an empty directory input is still
			// represented in the input tree. Not cached: re-walking is cheap.
			expanded = append(expanded, dir)
			continue
		}
		// Also represent empty subdirectories (those with no file beneath
		// them) so a remote action's input tree matches local disk; the output
		// side preserves empty dirs too (see dirOutputTree). Mark every
		// directory that holds a file, then keep the rest.
		if len(subdirs) > 0 {
			withFile := make(map[string]bool, len(files))
			for _, f := range files {
				for pd := stdpath.Dir(f); len(pd) > len(dir); pd = stdpath.Dir(pd) {
					if withFile[pd] {
						break
					}
					withFile[pd] = true
				}
			}
			for _, sd := range subdirs {
				if !withFile[sd] {
					files = append(files, sd)
				}
			}
		}
		// files is built fresh per walk and never mutated, so it is safe to
		// share from the cache.
		hfs.dirInputCache.Store(cacheKey, files)
		hfs.dirInputCached.Store(true)
		expanded = append(expanded, files...)
	}
	return expanded
}

// expandDirInputsWalks counts directory-input tree walks done by
// expandDirInputs; it backs the tests that verify the once-per-build walk.
var expandDirInputsWalks atomic.Int64

// invalidateDirInputCache drops cached directory-input expansions whose
// directory overlaps any of the mutated full paths.
func (hfs *HashFS) invalidateDirInputCache(fullpaths ...string) {
	if !hfs.dirInputCached.Load() {
		return
	}
	hfs.dirInputCache.Range(func(k, _ any) bool {
		dir := k.(string)
		for _, p := range fullpaths {
			if p == dir || pathHasPrefix(p, dir) || pathHasPrefix(dir, p) {
				hfs.dirInputCache.Delete(dir)
				break
			}
		}
		return true
	})
}

// pathHasPrefix reports whether p is at or under the directory prefix
// (component-aware: "a/b" is under "a", "a/bc" is not).
func pathHasPrefix(p, prefix string) bool {
	return strings.HasPrefix(p, prefix+"/")
}

// resolveInputEntries looks up or creates entries for each input,
// kicks off concurrent digest computation for regular files that
// need it, and waits for all digests to finish before returning.
func (hfs *HashFS) resolveInputEntries(ctx context.Context, root string, inputs []path.Path) ([]*entry, error) {
	ents := make([]*entry, 0, len(inputs))
	batch := hfs.batchDigester()
	for _, input := range inputs {
		fname := makeFullpath(root, input)
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
					batch.start(ctx, string(fname), e)
				}
			}
			continue
		}
		e = newLocalEntry()
		e.init(ctx, string(fname), hfs.executables, hfs.OS)
		if errors.Is(e.err, context.Canceled) {
			return nil, e.err
		}
		if log.V(1) {
			clog.Infof(ctx, "tree new entry %s", fname)
		}
		ee, err := hfs.directory.store(ctx, fname, e)
		if err != nil {
			// store may fail for missing dir. b/468142748
			clog.Warningf(ctx, "entry %s %v: %v", fname, e.err, err)
			ents = append(ents, e)
			continue
		}
		ents = append(ents, ee)
		batch.start(ctx, string(fname), ee)
	}
	_, wspan := trace.NewSpan(ctx, "fs-entries-wait")
	nwait := batch.wait(ctx)
	wspan.SetAttr("waits", nwait)
	wspan.Close(nil)
	return ents, nil
}

// buildMerkletreeEntries converts resolved entries to merkletree format,
// filtering entries with errors and resolving symlinks that escape root
// if resolveEscapedSymlinks is true.
// inputs are the original relative paths (used for merkletree entry names).
func (hfs *HashFS) buildMerkletreeEntries(ctx context.Context, root string, inputs []path.Path, ents []*entry, resolveEscapedSymlinks bool) ([]merkletree.Entry, error) {
	entries := make([]merkletree.Entry, 0, len(inputs))
	for i, e := range ents {
		fname := inputs[i]
		d := e.digest()
		if e.err != nil || (d.IsZero() && !e.isSymlink() && !e.isDirectory()) {
			// TODO(b/435555841): hard fail instead
			if e.entryErrLogged.CompareAndSwap(false, true) {
				clog.Warningf(ctx, "missing %s data:%v target:%q: %v", fname, e.d, e.target, e.err)
			}
			continue
		}
		me := merkletree.Entry{
			Name:         fname,
			Data:         blob.NewData(e.src, d),
			IsExecutable: e.mode&0111 != 0,
			Target:       e.target,
		}
		if resolveEscapedSymlinks && e.isSymlink() {
			name := path.JoinRoot(root, fname)
			tname := makeFullpath(filepath.Dir(string(name)), path.Path(e.target))
			if escapesRoot(root, string(tname)) {
				var err error
				me, err = hfs.resolveEscapingSymlink(ctx, root, fname, e, me)
				if err != nil {
					return nil, err
				}
			}
		}
		entries = append(entries, me)
	}
	return entries, nil
}

// UpdateEntry is an entry for Update.
type UpdateEntry struct {
	Name path.Path

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

// newEntryFromUpdate creates an entry from an UpdateEntry that has
// a non-nil Entry (i.e. from remote execution, not local disk).
func newEntryFromUpdate(ent UpdateEntry) *entry {
	lready := make(chan bool, 1)
	if ent.IsLocal {
		close(lready)
	} else {
		lready <- true
	}
	e := &entry{
		lready:      lready,
		mtime:       ent.ModTime,
		mode:        ent.Mode,
		cmdhash:     ent.CmdHash,
		edgehash:    ent.EdgeHash,
		action:      ent.Action,
		updatedTime: ent.UpdatedTime,
		isChanged:   ent.IsChanged,
	}
	switch {
	case !ent.Entry.Data.IsZero():
		if ent.Entry.IsExecutable {
			e.mode |= 0111
		}
		e.size = ent.Entry.Data.Digest().SizeBytes
		e.local = ent.IsLocal
		e.src = ent.Entry.Data
		e.d = ent.Entry.Data.Digest()
	case ent.Entry.Target != "":
		e.mode |= fs.ModeSymlink
		e.local = true
		e.target = ent.Entry.Target
	default: // directory
		e.mode |= fs.ModeDir
		// Respect IsLocal like the regular-file case: a remote dir output
		// (output_local=false) lives only in CAS and must reload as
		// build-without-bytes, not as a missing output-needed file.
		e.local = ent.IsLocal
		e.directory = &directory{}
	}
	return e
}

// Update updates cache information for entries under workspaceRoot.
func (hfs *HashFS) Update(ctx context.Context, workspaceRoot string, entries []UpdateEntry) error {
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

	if hfs.opt.CartFS != nil {
		hfs.cartfsRegister(ctx, workspaceRoot, entries, cartfsutil.UrgencyOnAccess)
	}
	if hfs.opt.ABFS != nil {
		hfs.abfsRegister(ctx, workspaceRoot, entries)
	}

	committed := make([]string, 0, len(entries))
	for _, ent := range entries {
		clog.Infof(ctx, "update %v", ent)
		fname := path.JoinRoot(workspaceRoot, ent.Name)
		e, err := hfs.resolveUpdateEntry(ctx, workspaceRoot, fname, ent)
		if e == nil {
			if err != nil {
				return err
			}
			continue // warning already logged
		}
		if err = hfs.commitEntry(ctx, fname, e); err != nil {
			return err
		}
		if err := hfs.updateMtimeIfNeeded(ctx, string(fname), e, ent); err != nil {
			return err
		}
		committed = append(committed, string(fname))
	}
	// A committed output can add a file under a cached directory input; drop
	// the stale expansions. Batched over all committed paths so a many-output
	// action does not rescan the cache per file.
	hfs.invalidateDirInputCache(committed...)
	return nil
}

// cartfsRegister registers file entries into Cartfs.
// marking successfully inserted entries as local.
func (hfs *HashFS) cartfsRegister(ctx context.Context, workspaceRoot string, entries []UpdateEntry, urgency cartfsutil.Urgency) {
	start := time.Now()
	var numUpdates int
	defer func() {
		clog.Infof(ctx, "cartfsRegister took %v for %d entries (registered=%d, urgency=%v)",
			time.Since(start), len(entries), numUpdates, urgency)
	}()

	// TODO: pass UpdateEntry so artfs can set mtime?
	var updates []*cartfsutil.Registration
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
		updates = append(updates, &cartfsutil.Registration{
			Entry:   *ent.Entry,
			Urgency: urgency,
		})
	}
	if len(updates) > 0 {
		numUpdates = len(updates)
		err := hfs.opt.CartFS.RegisterFiles(ctx, workspaceRoot, updates)
		if err != nil {
			clog.Warningf(ctx, "cartfs register %d under %s: %v", len(updates), workspaceRoot, err)
		} else {
			clog.Infof(ctx, "cartfs register %d under %s", len(updates), workspaceRoot)
			// cartfsfs registered the update, so we can assume
			// these files exist locally.
			for ui, ei := range updateIdx {
				if updates[ui].Err != nil {
					clog.Warningf(ctx, "cartfs register %q: %v", updates[ui].Entry.Name, updates[ui].Err)
					continue
				}
				entries[ei].IsLocal = true
			}
		}
	} else {
		clog.Warningf(ctx, "cartfs register 0 from_local=%d not_file=%d", nFromLocals, nNonFiles)
	}
}

// abfsRegister registers file entries into ABFS.
// making successfully inserted entries as local.
func (hfs *HashFS) abfsRegister(ctx context.Context, workspaceRoot string, entries []UpdateEntry) {
	start := time.Now()
	var numUpdates int
	defer func() {
		clog.Infof(ctx, "abfsRegister took %v for %d entries (registered=%d)",
			time.Since(start), len(entries), numUpdates)
	}()

	var updates []*abfsutil.Registration
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
		updates = append(updates, &abfsutil.Registration{
			Entry: *ent.Entry,
		})
	}
	if len(updates) > 0 {
		numUpdates = len(updates)
		err := hfs.opt.ABFS.RegisterFiles(ctx, workspaceRoot, updates)
		if err != nil {
			clog.Warningf(ctx, "abfs register %d under %s: %v", len(updates), workspaceRoot, err)
		} else {
			clog.Infof(ctx, "abfs register %d under %s", len(updates), workspaceRoot)
			// abfs registered the update, so we can assume
			// these files exist locally.
			for ui, ei := range updateIdx {
				if updates[ui].Err != nil {
					clog.Warningf(ctx, "abfs register %q: %v", updates[ui].Entry.Name, updates[ui].Err)
					continue
				}
				entries[ei].IsLocal = true
			}
		}
	} else {
		clog.Warningf(ctx, "abfs register 0 from_local=%d not_file=%d", nFromLocals, nNonFiles)
	}
}

// resolveUpdateEntry returns the entry to store for an UpdateEntry.
// Returns (nil, nil) if the entry was not found (warning already logged).
// Returns (nil, err) on fatal errors like context cancellation.
func (hfs *HashFS) resolveUpdateEntry(ctx context.Context, workspaceRoot string, fname path.Path, ent UpdateEntry) (*entry, error) {
	if ent.Entry != nil {
		return newEntryFromUpdate(ent), nil
	}
	// Entry was captured by RetrieveUpdateEntriesFromLocal,
	// so look up the existing entry.
	e, _, _, ok := hfs.dirLookup(ctx, workspaceRoot, ent.Name)
	if !ok {
		clog.Warningf(ctx, "failed to update: no entry %s", ent.Name)
		return nil, nil
	}
	if e.getMtime().Equal(ent.ModTime) || e.getDir() != nil {
		// Mtime unchanged or directory. Reuse existing entry,
		// including mode from the update.
		e.mu.Lock()
		e.applyUpdateMetadata(ent)
		e.mode = ent.Mode
		e.mu.Unlock()
	} else {
		// Mtime changed on a non-directory, re-init from disk.
		// Mode comes from init(), not the update.
		// No lock needed: entry is freshly created, not yet visible.
		e = newLocalEntry()
		e.init(ctx, string(fname), hfs.executables, hfs.OS)
		e.applyUpdateMetadata(ent)
	}
	if errors.Is(e.err, context.Canceled) {
		return nil, e.err
	}
	return e, nil
}

// updateMtimeIfNeeded updates the mtime on disk after storing an entry.
// Directories always get mtime updated. Regular local changed files
// get mtime updated. Symlinks are skipped since os.Chtimes would update
// the target's mtime, invalidating it in .siso_fs_state.
func (hfs *HashFS) updateMtimeIfNeeded(ctx context.Context, fname string, e *entry, ent UpdateEntry) error {
	if e.isDirectory() {
		err := hfs.OS.Chtimes(ctx, fname, time.Time{}, ent.ModTime)
		if err != nil {
			clog.Warningf(ctx, "failed to update dir mtime %s: %v", fname, err)
		}
		return nil
	}
	if !ent.IsLocal || !e.isChanged || e.isSymlink() {
		return nil
	}
	err := hfs.OS.Chtimes(ctx, fname, time.Time{}, e.getMtime())
	if errors.Is(err, fs.ErrNotExist) {
		clog.Warningf(ctx, "failed to update mtime of %s: %v", fname, err)
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to update mtime of %s: %w", fname, err)
	}
	return nil
}

// RetrieveUpdateEntries gets UpdateEntry for fnames at root.
//
// It skips only fnames that are absent from BOTH the hashFS cache and local
// disk. Such a name contributes nothing to the snapshot anyway (Entries drops
// entries whose stat errored), and statting+storing it here would install a
// negative (ErrNotExist) entry in the shared hashFS. In racing mode that races
// a concurrent local racer producing the same output and poisons its entry — in
// particular the depfile, which carries no cmdhash and so is unprotected in
// shouldKeep — surfacing as a spurious "failed to get depfile". A name hashFS
// already has an entry for (e.g. a remote output left in hashFS/CAS but not
// materialized locally) is kept, so its cached digest stays in the snapshot that
// RecordPreOutputs feeds to restat_content (used by the racing remote winner in
// runRacing, which records outputs even though SkipRecordOutputs is set).
func (hfs *HashFS) RetrieveUpdateEntries(ctx context.Context, root string, fnames []path.Path) []UpdateEntry {
	ctx, span := trace.NewSpan(ctx, "fs-update-entries")
	defer span.Close(nil)
	existing := make([]path.Path, 0, len(fnames))
	for _, fname := range fnames {
		fullname := makeFullpath(root, fname)
		if _, _, _, ok := hfs.directory.lookup(ctx, fullname); ok {
			// cached in hashFS (maybe a remote output not on local disk).
			existing = append(existing, fname)
			continue
		}
		if _, err := hfs.OS.Lstat(ctx, string(fullname)); err == nil {
			existing = append(existing, fname)
		}
	}
	ents, err := hfs.Entries(ctx, root, existing)
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
func (hfs *HashFS) RetrieveUpdateEntriesFromLocal(ctx context.Context, root string, fnames []path.Path) []UpdateEntry {
	ctx, span := trace.NewSpan(ctx, "fs-update-entries-from-local")
	defer span.Close(nil)

	ents := make([]UpdateEntry, 0, len(fnames))
	// invalidate hashfs cache for all fnames and its missing parents.
	// Keep track of visited parent directories to avoid redundant Lstat/Stat/delete operations.
	visitedDirs := make(map[path.Path]struct{})
	for _, fname := range fnames {
		// Check context before modifying hashfs.  In racing mode
		// the context may be canceled when the remote side wins,
		// and proceeding with a canceled context would delete
		// hashfs entries (via directory.delete below) without being
		// able to re-create them (Lstat/Stat will fail on the
		// canceled context), orphaning entries from concurrent steps.
		if ctx.Err() != nil {
			return ents
		}
		fullname := makeFullpath(root, fname)
		lfi, err := hfs.OS.Lstat(ctx, string(fullname))
		if errors.Is(err, fs.ErrNotExist) {
			clog.Warningf(ctx, "missing local %s: %v", fname, err)
			// The path itself is gone on disk, so any cached subtree under it is
			// stale and must go - force-remove it. The child-preserving delete is
			// only correct for ancestor negative-cache clearing below (where the
			// parent still exists on disk); here it would early-return on a
			// populated directory and leave deleted children reachable.
			hfs.directory.deleteForce(ctx, fullname)
			continue
		} else if err != nil {
			// Lstat failed for a reason other than ErrNotExist
			// (e.g. context canceled). We can't determine the
			// on-disk state, so skip this file without modifying
			// hashfs to avoid orphaning entries from concurrent
			// steps that share the same parent directory.
			clog.Warningf(ctx, "failed to access local %s: %v", fname, err)
			continue
		}
		if !lfi.IsDir() {
			// The path is now a regular file. Force-remove any stale entry,
			// including a cached directory subtree, so it can be re-recorded as
			// the file; the child-preserving delete would leave the stale
			// directory behind (ReadFile would then fail with "no src").
			hfs.directory.deleteForce(ctx, fullname)
		}
		// clear negative cache in parent directories
		pathname := path.Path(filepath.ToSlash(filepath.Dir(string(fullname))))
		for {
			// Skip if the directory has already been processed in this call.
			if _, ok := visitedDirs[pathname]; ok {
				break
			}
			_, lerr := hfs.OS.Lstat(ctx, string(pathname))
			if lerr != nil {
				// Can't determine on-disk state (context
				// canceled, permission error, etc). Stop
				// clearing to avoid incorrectly deleting
				// hashfs directory entries, which would
				// orphan children from concurrent steps.
				break
			}
			_, err = hfs.Stat(ctx, "", pathname)
			visitedDirs[pathname] = struct{}{}
			if errors.Is(err, lerr) {
				// if err matches with local err,
				// no need to invalidate hashfs dir.
				break
			}
			// Clearing a parent's stale negative cache; delete never evicts a
			// directory node (a concurrent step may have recorded, or be recording,
			// a sibling under it), only a non-directory (negative) entry.
			hfs.directory.delete(ctx, pathname)
			parent := path.Path(filepath.ToSlash(filepath.Dir(string(pathname))))
			if parent == pathname || parent == "/" || parent == "" || parent == "." {
				// nothing to do more.
				break
			}
			pathname = parent
		}
		ent := UpdateEntry{
			Name:    fname,
			Mode:    lfi.Mode(),
			ModTime: lfi.ModTime(),
			IsLocal: true,
		}
		ents = append(ents, ent)
	}
	// capture hashfs for all fnames after all missing entries, parents
	// are invalidated in the above loop.
	//
	// needCompute=false: only cmdhash/edgehash/action/updatedTime/
	// isChanged are read below, none needing a digest. The following
	// Update replaces this entry, so digesting it here would just be a
	// duplicate open+read of a file Update digests again.
	for i, ent := range ents {
		fi, err := hfs.stat(ctx, root, ent.Name, statOpts{needCompute: false})
		if err != nil {
			clog.Warningf(ctx, "failed to stat after invalidate %s: %v", ent.Name, err)
		} else {
			ent.CmdHash = fi.CmdHash()
			ent.EdgeHash = fi.EdgeHash()
			ent.Action = fi.Action()
			ent.UpdatedTime = fi.UpdatedTime()
			ent.IsChanged = fi.IsChanged()
			ents[i] = ent
		}
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

func (noDataSource) Source(_ context.Context, d digest.Digest, fname string) blob.Source {
	return noSource{fname}
}

// NeedFlush returns whether the fname need to be flushed based on OutputLocal option.
func (hfs *HashFS) NeedFlush(ctx context.Context, workspaceRoot string, fname path.Path) bool {
	return hfs.opt.OutputLocal(ctx, string(makeFullpath(workspaceRoot, fname)))
}

// expandFlushDirs expands each declared directory artifact (a path with a
// trailing slash) into the directory plus all its descendants, so flushing a
// directory artifact materializes its whole tree to disk (flushDir alone only
// creates the directory node).
//
// Only trailing-slash paths are expanded. A path that resolves to a directory
// but was declared as a plain file output (e.g. a legacy "copy" of a directory
// tree) is flushed as-is: its descendants stay in hashfs and are not written to
// local disk, preserving build-without-the-bytes for those outputs.
func (hfs *HashFS) expandFlushDirs(ctx context.Context, workspaceRoot string, files []string) []string {
	var expanded []string
	// Dedup: a duplicate path would block the Flush loop on an already-drained
	// e.lready channel (and can trigger a redundant second flush).
	seen := make(map[string]bool)
	add := func(p string) {
		if seen[p] {
			return
		}
		seen[p] = true
		expanded = append(expanded, p)
	}
	// Walk the in-memory hashfs tree, not fs.WalkDir over FileSystem: the
	// directory being flushed may not exist on local disk yet, so WalkDir's
	// initial stat would fail to recognize it and never descend.
	var walk func(rel string)
	walk = func(rel string) {
		if seen[rel] {
			// Guards against unbounded recursion if the in-memory tree ever
			// presents a cycle, and skips redundant re-walks.
			return
		}
		add(rel)
		dents, err := hfs.ReadDir(ctx, workspaceRoot, path.New(rel))
		if err != nil {
			clog.Warningf(ctx, "expand flush dir %s: %v", rel, err)
			return
		}
		for _, de := range dents {
			child := filepath.ToSlash(filepath.Join(rel, de.Name()))
			if de.IsDir() {
				walk(child)
			} else {
				add(child)
			}
		}
	}
	for _, file := range files {
		if !strings.HasSuffix(file, "/") {
			// Not a declared directory artifact. Flush as-is, even if it happens
			// to resolve to a directory on disk.
			add(file)
			continue
		}
		dir := strings.TrimSuffix(file, "/")
		fname := makeFullpath(workspaceRoot, path.New(dir))
		e, _, _, ok := hfs.directory.lookup(ctx, fname)
		if !ok || !e.isDirectory() {
			add(dir)
			continue
		}
		walk(dir)
	}
	// Sort so an ancestor directory always precedes its members: Flush
	// materializes directories in slice order, before launching member flushes.
	// Returning expanded (not files) also dedups a caller's duplicate paths.
	slices.Sort(expanded)
	return expanded
}

// flushedDirEntry pairs a materialized directory with its entry so Flush can
// restore the directory's mtime after all of its members are written.
type flushedDirEntry struct {
	fname string
	e     *entry
}

// Flush flushes cached information for files under workspaceRoot to local disk.
func (hfs *HashFS) Flush(ctx context.Context, workspaceRoot string, files []path.Path) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ctx, span := trace.NewSpan(ctx, "flush")
	defer span.Close(nil)
	files = path.Paths(hfs.expandFlushDirs(ctx, workspaceRoot, path.Strings(files)))
	// errgroup.WithContext cancels its derived ctx once eg.Wait returns; keep
	// the pre-errgroup ctx for the post-flush directory mtime restore below.
	flushCtx := ctx
	eg, ctx := errgroup.WithContext(ctx)
	var flushedDirs []flushedDirEntry
	for _, file := range files {
		fname := makeFullpath(workspaceRoot, file)
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
				// TODO: support for abfs too if abfs provides trigger downloading.
				// flush will be called when it will be used
				// for local action.
				// better for performance to trigger downloading
				// when flush (in batch), rather than let them
				// download on access (serially).
				// it didn't work well with gitws. b/558938598

				if e.mtimeUpdated && !e.isSymlink() {
					// mtime was updated after entry sets mtime from the local disk.
					// Don't update mtime for symlink,
					// since os.Chtimes updates the mtime of target
					// and it makes the target invalidated
					// in .siso_fs_state since mtime doesn't match.
					err := hfs.OS.Chtimes(ctx, string(fname), time.Time{}, e.mtime)
					if errors.Is(err, fs.ErrNotExist) {
						e.mu.Unlock()
						return fmt.Errorf("flush %s local-ready: %w", fname, err)
					}
					clog.Infof(ctx, "flush %s local ready mtime update: %v", fname, err)
					if err == nil {
						e.mtimeUpdated = false
					}
				}
				err := e.err
				generatedDir := e.isDirectory() && len(e.cmdhash) > 0
				e.mu.Unlock()
				if errors.Is(err, fs.ErrNotExist) || errors.Is(err, errNotRegular) {
					clog.Warningf(ctx, "flush %s local-ready: %v", fname, err)
					continue
				}
				if err != nil {
					return fmt.Errorf("flush %s local-ready: %w", fname, err)
				}
				if generatedDir {
					// Members flushed below may still be written into this
					// local-ready generated directory, re-bumping its on-disk
					// mtime; record it for the mtime restore after eg.Wait. A
					// source directory (no cmdhash) is left alone: rewriting
					// its mtime would hide a user's mid-build change from
					// updateDir.
					flushedDirs = append(flushedDirs, flushedDirEntry{fname: string(fname), e: e})
				}
				continue
			}
		case <-ctx.Done():
			return fmt.Errorf("flush wait local-ready %s: %w", fname, context.Cause(ctx))
		}
		hfs.digester.compute(ctx, string(fname), e)
		ctx, done, err := acquireFlush(ctx, hfs.opt.FlushGate)
		if err != nil {
			// flush failed, so may need to flush again.
			select {
			case e.lready <- true:
			default:
			}
			return fmt.Errorf("flush admission %s: %w", fname, err)
		}
		if e.isDirectory() {
			// Materialize a directory synchronously: a member's flush MkdirAll's
			// its parent, so any stale file/symlink at the directory's path must
			// be cleared before member flushes launch.
			err = e.flush(ctx, string(fname), hfs.OS, max(fetch.Timeout(e.d), hfs.opt.MinFlushTimeout))
			done(err)
			if err != nil {
				// Cancel and drain member flushes already launched, so no
				// goroutine keeps mutating outputs after Flush reports failure.
				cancel()
				if werr := eg.Wait(); werr != nil {
					clog.Warningf(ctx, "flush: drained pending flushes: %v", werr)
				}
				return fmt.Errorf("flush dir %s: %w", fname, err)
			}
			// flushDir set this directory's mtime, but writing members into
			// it afterward re-bumps it on disk; record it for the restore below.
			flushedDirs = append(flushedDirs, flushedDirEntry{fname: string(fname), e: e})
			continue
		}
		eg.Go(func() (err error) {
			defer func() { done(err) }()
			err = e.flush(ctx, string(fname), hfs.OS, max(fetch.Timeout(e.d), hfs.opt.MinFlushTimeout))
			// flush should not fail with cas not found error.
			// but if it failed, current recorded digest should
			// be wrong, so should delete from the hashfs.
			if code := status.Code(err); code == codes.NotFound {
				clog.Warningf(ctx, "flush failed. delete %s from hashfs: %v", fname, err)
				hfs.directory.delete(ctx, fname)
			}
			return err
		})
	}
	if err := eg.Wait(); err != nil {
		return err
	}
	// flushDir sets each directory's mtime to its recorded value, but writing
	// members into a directory re-bumps it on disk afterward. Restore every
	// flushed directory's mtime now that all members are written, so it stays
	// consistent with .siso_fs_state and isn't invalidated on reload. This
	// mirrors the file mtime handling in flushRegularFile.
	for _, d := range flushedDirs {
		mtime := d.e.getMtime()
		if err := hfs.OS.Chtimes(flushCtx, d.fname, time.Time{}, mtime); err != nil {
			clog.Warningf(flushCtx, "flush restore dir mtime %q: %v", d.fname, err)
		}
	}
	return nil
}

// Refresh refreshes cached file entries.
func (hfs *HashFS) Refresh(ctx context.Context) error {
	defer trace.Begin(ctx, "hashfs.Refresh").End()
	// TODO: optimize?
	state := hfs.State(ctx)
	// reset loaded as it reset entry data.
	hfs.loaded.Store(false)
	hfs.directory = &directory{isRoot: true}
	// The tree is rebuilt from state, so any memoized directory-input
	// expansion is detached from it; drop the cache and its gate.
	hfs.dirInputCache.Clear()
	hfs.dirInputCached.Store(false)
	err := hfs.SetState(ctx, state)
	werr := hfs.WaitReady(ctx)
	if err != nil {
		return err
	}
	return werr
}

// FileInfo implements https://pkg.go.dev/io/fs#FileInfo.
type FileInfo struct {
	root  string
	fname path.Path
	e     *entry
	fis   []FileInfo
}

func (fi FileInfo) Path() path.Path {
	return path.JoinRoot(fi.root, fi.fname)
}

// Name is a base name of the file.
func (fi FileInfo) Name() string {
	return filepath.Base(string(fi.fname))
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
		Data:         blob.NewData(fi.e.src, d),
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
