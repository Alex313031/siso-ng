// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package hashfs

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	log "github.com/golang/glog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"go.chromium.org/build/hashigo/digest"

	"go.chromium.org/build/siso/blob"
	"go.chromium.org/build/siso/hashfs/osfs"
	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/o11y/monitoring"
	"go.chromium.org/build/siso/path"
	"go.chromium.org/build/siso/reapi/fetch"
	"go.chromium.org/build/siso/reapi/retry"
)

// entry represents a file, directory, or symlink in hashfs.
//
// The type of entry is determined by the following invariants:
//   - directory: directory is not nil. target must be "" and d must be zero.
//   - symlink: target is not "". directory must be nil and d must be zero.
//   - file: directory is nil and target is "". d may be zero
//     (if digest has not been calculated yet).
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
	// diskMiss indicates this negative entry came from a local-disk probe
	// (init found the path absent), as opposed to an explicit removal.
	diskMiss bool
	// isChanged indicates the file is changed in the session.
	isChanged bool
	// isMissingChecked indicates the file is checked in ForgetMissings.
	isMissingChecked bool

	target string // symlink.

	src blob.Source
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
	e.mu.Lock()
	err := e.err
	e.mu.Unlock()
	if err != nil {
		return fmt.Sprintf("err:%v", err)
	}
	return fmt.Sprintf("size:%d mode:%s mtime:%s", e.size, e.mode, e.getMtime())
}

var errNotRegular = errors.New("unexpected filetype not regular")

func (e *entry) init(ctx context.Context, fname string, executables map[path.Path]bool, osfs *osfs.OSFS) {
	fi, err := osfs.Lstat(ctx, fname)
	if errors.Is(err, fs.ErrNotExist) {
		if log.V(1) {
			clog.Infof(ctx, "not exist %s", fname)
		}
		e.err = err
		e.diskMiss = true
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

func (e *entry) compute(ctx context.Context, fn digest.Function, fname string) error {
	// Fast path: already done, errored, or nothing to compute.
	e.mu.Lock()
	if e.err != nil {
		err := e.err
		e.mu.Unlock()
		return err
	}
	if !e.d.IsZero() || e.src == nil {
		e.mu.Unlock()
		return nil
	}
	dch := e.dch
	if dch == nil {
		// We're the first caller: claim the computation.
		dch = make(chan struct{})
		e.dch = dch
		src := e.src
		e.mu.Unlock()

		// Run in a goroutine so callers can bail on context cancellation
		// without blocking on disk I/O. Detach from the first caller's
		// cancellation so a canceled caller does not poison e.err with
		// context.Canceled or fail concurrent/subsequent waiters.
		dctx := context.WithoutCancel(ctx)
		go func() {
			data, err := localDigest(dctx, fn, src, fname)
			// Only write e.err on the error path. ReadDir's Range callback
			// reads ee.err without holding e.mu, so an unconditional write
			// (even of nil) would race with that read.
			e.mu.Lock()
			if err != nil {
				e.err = err
			} else {
				e.d = data.Digest()
			}
			e.entryErrLogged.Store(false)
			close(dch)
			e.mu.Unlock()
		}()
	} else {
		e.mu.Unlock()
	}

	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-dch:
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.err
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
			if strings.HasSuffix(name, ".tmp") || strings.HasPrefix(name, ".tempfile.") || strings.HasSuffix(name, ".siso_tmp") {
				continue
			}
			if hfs.opt.Ignore(ctx, filepath.Join(dname, name)) {
				continue
			}
			wg.Go(func() {
				// update entry in e.directory.
				_, err := hfs.stat(ctx, dname, path.Path(name), statOpts{})
				if err != nil {
					clog.Warningf(ctx, "updateDir stat %s: %v", name, err)
				}
			})
		}
		wg.Wait()
	} else {
		for _, name := range names {
			// don't scan temporary file by readdir.
			// it may cause race on windows.
			// b/294318963 b/381947692
			if strings.HasSuffix(name, ".tmp") || strings.HasPrefix(name, ".tempfile.") || strings.HasSuffix(name, ".siso_tmp") {
				continue
			}
			if hfs.opt.Ignore(ctx, filepath.Join(dname, name)) {
				continue
			}
			// update entry in e.directory.
			_, err := hfs.stat(ctx, dname, path.Path(name), statOpts{})
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

// isGenerated reports whether this entry is owned by a build step.
func (e *entry) isGenerated() bool {
	if e == nil {
		return false
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.isChanged || len(e.cmdhash) > 0 || len(e.edgehash) > 0 || !e.action.IsZero()
}

// applyUpdateMetadata sets the metadata fields from an UpdateEntry.
// Does not set e.mode. Callers that reuse an existing entry should
// set it explicitly; callers that reinit from disk get it from init().
//
// Caller must hold e.mu or ensure no concurrent access (e.g. newly created entry).
func (e *entry) applyUpdateMetadata(ent UpdateEntry) {
	e.mtime = ent.ModTime
	e.cmdhash = ent.CmdHash
	e.edgehash = ent.EdgeHash
	e.action = ent.Action
	e.local = ent.IsLocal
	e.updatedTime = ent.UpdatedTime
	e.isChanged = ent.IsChanged
	e.entryErrLogged.Store(false)
}

// isDirectory returns whether the entry is a directory.
func (e *entry) isDirectory() bool {
	return e.directory != nil
}

// isSymlink returns whether the entry is a symlink.
func (e *entry) isSymlink() bool {
	return e.target != ""
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

	e.mu.Lock()
	err := e.err
	e.mu.Unlock()
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return e.flushRemove(ctx, fname, osfs)
	case e.isDirectory():
		return e.flushDir(ctx, fname, osfs)
	case e.isSymlink():
		return e.flushSymlink(ctx, fname, osfs)
	default:
		return e.flushRegularFile(ctx, fname, osfs, timeout)
	}
}

// flushRemove removes a file from disk. Removal is safe even while a digest
// read holds the file open: openRead shares FILE_SHARE_DELETE on Windows.
func (e *entry) flushRemove(ctx context.Context, fname string, osfs *osfs.OSFS) error {
	err := osfs.Remove(ctx, fname)
	clog.Infof(ctx, "flush remove %s: %v", fname, err)
	if errors.Is(err, fs.ErrNotExist) {
		err = nil
	}
	return err
}

// removeStaleForFlush removes an on-disk entry whose type no longer matches
// the recorded output. RemoveAll, not Remove: it may be a non-empty directory.
func removeStaleForFlush(ctx context.Context, fname, desc string, osfs *osfs.OSFS) error {
	clog.Warningf(ctx, "flush %s: was %s in a previous build; removing stale entry", fname, desc)
	if err := osfs.RemoveAll(ctx, fname); err != nil {
		return fmt.Errorf("flush %s: remove stale %s: %w", fname, desc, err)
	}
	return nil
}

// staleAncestorMu serializes stale-ancestor removal across concurrent
// flushes. Two outputs below the same stale file must not both act on their
// own Lstat: one would recreate the directory and materialize its output,
// and the other would then RemoveAll that directory (with the sibling's
// fresh output inside) based on its stale view.
var staleAncestorMu sync.Mutex

// mkdirAllForFlush ensures dir exists for an output being flushed below it,
// removing a stale non-directory ancestor: the recorded entry proves every
// component must be a directory, so anything else on the path is a leftover
// from a previous build. A symlink that resolves to a directory is a valid
// component and is kept.
func mkdirAllForFlush(ctx context.Context, dir string, osfs *osfs.OSFS) error {
	err := osfs.MkdirAll(ctx, dir, 0755)
	if err == nil {
		return nil
	}
	staleAncestorMu.Lock()
	defer staleAncestorMu.Unlock()
	// A concurrent flush may have removed the stale ancestor while we waited
	// for the lock; the walk below then acts on the current on-disk state.
	if merr := osfs.MkdirAll(ctx, dir, 0755); merr == nil {
		return nil
	}
	// Walk up to the first existing component. At most one non-directory can
	// exist on the path (nothing can exist below it), so one removal is
	// enough.
	for p := dir; ; {
		fi, lerr := osfs.Lstat(ctx, p)
		if lerr != nil {
			// The component doesn't exist or is unreachable below a
			// non-directory ancestor (ENOTDIR); keep walking up.
			parent := filepath.Dir(p)
			if parent == p {
				return err
			}
			p = parent
			continue
		}
		if fi.IsDir() {
			// MkdirAll failed for some other reason.
			return err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			sfi, serr := os.Stat(p)
			if serr == nil && sfi.IsDir() {
				// A symlink to a directory is a valid component, not what
				// made MkdirAll fail.
				return err
			}
			// A symlink is stale only on positive proof: it resolves to a
			// non-directory, or it is dangling (ENOENT/ENOTDIR). One with an
			// unstatable target (EACCES, EIO, ...) may be intentional;
			// removing it would divert outputs from the symlink's target.
			if serr != nil && !errors.Is(serr, fs.ErrNotExist) && !errors.Is(serr, syscall.ENOTDIR) {
				return serr
			}
		}
		if rerr := removeStaleForFlush(ctx, p, "a non-directory ancestor", osfs); rerr != nil {
			return rerr
		}
		return osfs.MkdirAll(ctx, dir, 0755)
	}
}

// flushDir ensures a directory exists on disk with the correct mtime.
func (e *entry) flushDir(ctx context.Context, fname string, osfs *osfs.OSFS) error {
	mtime := e.getMtime()
	fi, err := osfs.Lstat(ctx, fname)
	if err == nil && fi.IsDir() && fi.ModTime().Equal(mtime) {
		if log.V(1) {
			clog.Infof(ctx, "flush dir %s: already exist", fname)
		}
		return nil
	}
	if err == nil && !fi.IsDir() {
		// A stale file/symlink from a previous build is in the way;
		// MkdirAll can't replace it, so remove it first.
		if rerr := removeStaleForFlush(ctx, fname, "a non-directory", osfs); rerr != nil {
			return rerr
		}
	}
	err = mkdirAllForFlush(ctx, fname, osfs)
	if err != nil {
		clog.Infof(ctx, "flush dir %s: %v", fname, err)
	} else {
		err = osfs.Chtimes(ctx, fname, time.Time{}, mtime)
		clog.Infof(ctx, "flush dir chtime %s %v: %v", fname, mtime, err)
	}
	return err
}

// flushSymlink ensures a symlink exists on disk pointing to the correct target.
func (e *entry) flushSymlink(ctx context.Context, fname string, osfs *osfs.OSFS) error {
	target, err := osfs.Readlink(ctx, fname)
	if err == nil && e.target == target {
		return nil
	}
	// Clear whatever occupies the path before creating the symlink, outside
	// e.mu so readers of the entry aren't blocked behind a slow tree removal.
	// Probe with Lstat rather than keying on the Symlink error: on Windows a
	// directory in the way fails with ERROR_ACCESS_DENIED, not ErrExist.
	if _, lerr := osfs.Lstat(ctx, fname); lerr == nil {
		if rerr := removeStaleForFlush(ctx, fname, "a stale entry", osfs); rerr != nil {
			return rerr
		}
	} else {
		// needed for SISO_EXPERIMENTS=disable-ensure-output-dirs,
		// since the parent directory of symlink may not exist yet
		// and symlink below would fail.
		err = mkdirAllForFlush(ctx, filepath.Dir(fname), osfs)
		if err != nil {
			clog.Warningf(ctx, "flush %s: mkdir: %v", fname, err)
			return fmt.Errorf("failed to create directory for %s: %w", fname, err)
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	err = osfs.Symlink(ctx, e.target, fname)
	if errors.Is(err, fs.ErrExist) {
		// The path appeared between the Lstat probe and the Symlink (an
		// external writer racing the flush); clear it and retry once.
		if rerr := removeStaleForFlush(ctx, fname, "a raced entry", osfs); rerr != nil {
			return rerr
		}
		err = osfs.Symlink(ctx, e.target, fname)
	}
	clog.Infof(ctx, "flush symlink %s -> %s: %v", fname, e.target, err)
	// don't change mtimes. it fails if target doesn't exist.
	return err
}

// flushRegularFile writes a regular file to disk from its data source,
// handling hardlinks, clonefile optimization, and mtime updates.
func (e *entry) flushRegularFile(ctx context.Context, fname string, osfs *osfs.OSFS, timeout time.Duration) error {
	started := time.Now()
	mtime := e.getMtime()

	var removeReason string
	fi, err := osfs.Lstat(ctx, fname)
	if err == nil && fi.IsDir() {
		// A stale directory from a previous build is in the way; remove
		// the tree and treat the path as absent.
		if rerr := removeStaleForFlush(ctx, fname, "a directory", osfs); rerr != nil {
			return rerr
		}
		fi, err = nil, fs.ErrNotExist
	}
	if err == nil {
		if e.matchesFileInfo(fi) {
			// TODO: check hash, mode?
			clog.Infof(ctx, "flush %s: already exist", fname)
			return nil
		}
		removeReason = flushRemoveReason(fi)
		if removeReason == "" {
			if skip, err := e.flushSkipMatchingDigest(ctx, fname, fi, osfs); skip {
				return err
			}
			if !isWritable(fi) {
				// need to be writable. otherwise os.WriteFile fails with permission denied.
				err = osfs.Chmod(ctx, fname, fi.Mode()|0200)
				clog.Warningf(ctx, "flush %s: not writable? %s: %v", fname, fi.Mode(), err)
			}
		}
	}

	err = mkdirAllForFlush(ctx, filepath.Dir(fname), osfs)
	if err != nil {
		clog.Warningf(ctx, "flush %s: mkdir: %v", fname, err)
		return fmt.Errorf("failed to create directory for %s: %w", fname, err)
	}

	d := e.digest()
	removeBeforeWrite := func() {
		if removeReason != "" {
			if err := osfs.Remove(ctx, fname); err != nil {
				clog.Warningf(ctx, "flush %s: remove %s: %v", fname, removeReason, err)
			} else {
				clog.Infof(ctx, "flush %s: remove %s", fname, removeReason)
			}
		}
	}
	switch {
	case d.SizeBytes == 0:
		removeBeforeWrite()
		clog.Infof(ctx, "flush %s: empty file", fname)
		err = osfs.WriteFile(ctx, fname, nil, 0644)
	case len(e.buf) > 0:
		clog.Infof(ctx, "flush %s from embedded buf", fname)
		err = writeFileAtomic(ctx, fname, osfs, e.buf, e.mode, removeReason)
	case d.IsZero():
		return fmt.Errorf("no data: retrieve %s: ", fname)
	case removeReason == "" && e.isSourceFile(osfs, fname):
		// Source is the target and the file is a regular file (not a hard
		// link), so just fix permissions.
		err = osfs.Chmod(ctx, fname, e.mode)
	default:
		err = e.flushWrite(ctx, fname, osfs, started, timeout, removeReason)
	}
	if err != nil {
		return err
	}
	return osfs.Chtimes(ctx, fname, time.Time{}, mtime)
}

// isWritable reports whether the file has owner write permission.
func isWritable(fi os.FileInfo) bool {
	return fi.Mode()&0200 != 0
}

// matchesFileInfo reports whether fi is a regular file with the same size and
// mtime as the entry. The type check rejects a stale symlink whose
// link-string length and mtime coincide with the recorded file.
func (e *entry) matchesFileInfo(fi os.FileInfo) bool {
	return fi.Mode().IsRegular() && fi.Size() == e.digest().SizeBytes && fi.ModTime().Equal(e.getMtime())
}

// flushSkipMatchingDigest checks whether the file on disk already has the
// right content (same size, matching digest). Returns (true, err) if the
// flush can be skipped (with an optional chtimes fix), (false, nil) otherwise.
// Caller must ensure fi is a regular file and not a hard link.
func (e *entry) flushSkipMatchingDigest(ctx context.Context, fname string, fi os.FileInfo, osfs *osfs.OSFS) (bool, error) {
	d := e.digest()
	if fi.Size() != d.SizeBytes {
		return false, nil
	}
	src := osfs.FileSource(fname, fi.Size())
	ld, err := localDigest(ctx, osfs.DigestFunction(), src, fname)
	if err != nil {
		clog.Warningf(ctx, "flush %s: digest error: %v", fname, err)
		return false, nil
	}
	fileDigest := ld.Digest()
	if fileDigest != d {
		mtime := e.getMtime()
		clog.Warningf(ctx, "flush %s: exists but mismatch size:%d!=%d mtime:%s!=%s d:%v!=%v", fname, fi.Size(), d.SizeBytes, fi.ModTime(), mtime, fileDigest, d)
		return false, nil
	}
	if log.V(1) {
		clog.Infof(ctx, "flush %s: already exist - hash match", fname)
	}
	mtime := e.getMtime()
	if !fi.ModTime().Equal(mtime) {
		err = osfs.Chtimes(ctx, fname, time.Time{}, mtime)
	}
	return true, err
}

// flushRemoveReason returns why the existing file must be removed before
// writing, or "" if no removal is needed.
func flushRemoveReason(fi os.FileInfo) string {
	switch {
	case isHardlink(fi):
		return "hardlink"
	case !fi.Mode().IsRegular():
		return fmt.Sprintf("non-regular file %s", fi.Mode())
	default:
		return ""
	}
}

// isSourceFile reports whether e.src is a local file at fname,
// meaning the target already has the right content.
func (e *entry) isSourceFile(osfs *osfs.OSFS, fname string) bool {
	lsrc, ok := osfs.AsFileSource(e.src)
	return ok && lsrc.Fname == fname
}

// writeFileAtomic writes data to fname via a temp file and rename, so a
// concurrent reader or an interrupted flush never observes a truncated or
// partially-written file. rename atomically replaces an existing regular file
// (including a hardlink). A non-regular dst (removeReason != "") is removed
// first, mirroring flushWrite, since rename can't replace a directory.
func writeFileAtomic(ctx context.Context, fname string, osfs *osfs.OSFS, data []byte, perm fs.FileMode, removeReason string) error {
	tmpname := filepath.Join(filepath.Dir(fname), "."+filepath.Base(fname)+".siso_tmp")
	if err := osfs.WriteFile(ctx, tmpname, data, perm); err != nil {
		return err
	}
	if removeReason != "" {
		if err := osfs.Remove(ctx, fname); err != nil {
			clog.Warningf(ctx, "flush %s: remove %s: %v", fname, removeReason, err)
		} else {
			clog.Infof(ctx, "flush %s: remove %s", fname, removeReason)
		}
	}
	return osfs.Rename(ctx, tmpname, fname)
}

// flushWrite writes file data to fname, trying clone first if supported,
// falling back to tmp+rename.
func (e *entry) flushWrite(ctx context.Context, fname string, osfs *osfs.OSFS, started time.Time, timeout time.Duration, removeReason string) error {
	d := e.digest()
	lsrc, ok := osfs.AsFileSource(e.src)

	// Try clone if the OS supports it and the source is a local file.
	// Skip when source == target: Clonefile requires distinct paths, and
	// the tmp+rename path below correctly breaks any hardlink by writing
	// tmp before removing fname.
	type clonefiler interface {
		Clonefile(context.Context, string, string) error
	}
	if osfsc, cok := (any)(osfs).(clonefiler); ok && cok && lsrc.Fname != fname {
		if removeReason != "" {
			if err := osfs.Remove(ctx, fname); err != nil {
				clog.Warningf(ctx, "flush %s: remove %s: %v", fname, removeReason, err)
			} else {
				clog.Infof(ctx, "flush %s: remove %s", fname, removeReason)
			}
		}
		clog.Infof(ctx, "flush %s %s clone from source %s", fname, d, lsrc.Fname)
		err := osfsc.Clonefile(ctx, lsrc.Fname, fname)
		if err == nil {
			return osfs.Chmod(ctx, fname, e.mode)
		}
		clog.Warningf(ctx, "clonefile failed: %v", err)
	}

	// Copy via tmp file and rename.
	// Write tmp before removing, since e.src may read from fname.
	var srcname string
	if ok {
		srcname = lsrc.Fname
	}
	tmpname := filepath.Join(filepath.Dir(fname), "."+filepath.Base(fname)+".siso_tmp")
	ctx, cancel := fetch.ContextWithTimeout(ctx, e.d)
	defer cancel()
	// Skip the first-byte watchdog for local FileSource: no gRPC
	// InPayload event can fire and a slow local copy would be falsely
	// aborted. Otherwise cancel only attempt 0; retry attempts skip
	// the watchdog so a persistently slow source isn't thrashed.
	_, srcIsLocal := osfs.AsFileSource(e.src)
	attempt := 0
	// Record retry duration only for a retry that follows a watchdog
	// cancellation (codes.Aborted on the prior attempt). A plain retryable
	// gRPC error (e.g. Unavailable) before the watchdog fires would otherwise
	// be miscounted as a bytestream-read first-byte stall.
	retryAfterWatchdog := false
	err := retry.Do(ctx, func() error {
		callCtx := ctx
		if attempt == 0 && !srcIsLocal {
			callCtx = osfs.WithFirstByteTimeout(ctx, 5*time.Second)
		}
		attempt++
		start := time.Now()
		werr := osfs.WriteDigestData(callCtx, tmpname, e.src, e.mode, timeout)
		if retryAfterWatchdog {
			monitoring.RecordRetryDuration(ctx, "bytestream-read", time.Since(start), werr)
		}
		retryAfterWatchdog = status.Code(werr) == codes.Aborted
		return werr
	})
	if err != nil {
		return fmt.Errorf("flush tmp %s size=%d %s: %w", tmpname, d.SizeBytes, time.Since(started), err)
	}
	if removeReason != "" {
		if err := osfs.Remove(ctx, fname); err != nil {
			clog.Warningf(ctx, "flush %s: remove %s: %v", fname, removeReason, err)
		} else {
			clog.Infof(ctx, "flush %s: remove %s", fname, removeReason)
		}
	}
	clog.Infof(ctx, "flush %s %s from source %s", fname, d, srcname)
	return osfs.Rename(ctx, tmpname, fname)
}
