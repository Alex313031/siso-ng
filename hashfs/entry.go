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

	"go.chromium.org/build/siso/hashfs/osfs"
	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/o11y/monitoring"
	"go.chromium.org/build/siso/reapi/digest"
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
	e.mu.Lock()
	err := e.err
	e.mu.Unlock()
	if err != nil {
		return fmt.Sprintf("err:%v", err)
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
	if e.dch != nil {
		// Another goroutine is already computing; wait for it.
		dch := e.dch
		e.mu.Unlock()
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-dch:
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		return e.err
	}
	// We're the first caller: claim the computation.
	e.dch = make(chan struct{})
	e.mu.Unlock()

	// Run in a goroutine so we can bail on context cancellation
	// without blocking on disk I/O.
	type result struct {
		data digest.Data
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		data, err := localDigest(ctx, e.src, fname)
		ch <- result{data, err}
	}()

	var err error
	var d digest.Digest
	select {
	case <-ctx.Done():
		err = context.Cause(ctx)
	case r := <-ch:
		err = r.err
		d = r.data.Digest()
	}

	// Only write e.err on the error path. ReadDir's Range callback
	// reads ee.err without holding e.mu, so an unconditional write
	// (even of nil) would race with that read.
	e.mu.Lock()
	if err != nil {
		e.err = err
	} else {
		e.d = d
	}
	e.entryErrLogged.Store(false)
	close(e.dch)
	e.mu.Unlock()
	return err
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
				_, err := hfs.stat(ctx, dname, name, false)
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

// flushRemove removes a file from disk, waiting for any in-progress
// digest calculation to finish first (Windows sharing violation guard).
func (e *entry) flushRemove(ctx context.Context, fname string, osfs *osfs.OSFS) error {
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
	if errors.Is(err, fs.ErrNotExist) {
		err = nil
	}
	return err
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
	err = osfs.MkdirAll(ctx, fname, 0755)
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
}

// flushRegularFile writes a regular file to disk from its data source,
// handling hardlinks, clonefile optimization, and mtime updates.
func (e *entry) flushRegularFile(ctx context.Context, fname string, osfs *osfs.OSFS, timeout time.Duration) error {
	started := time.Now()
	mtime := e.getMtime()

	var removeReason string
	fi, err := osfs.Lstat(ctx, fname)
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

	err = osfs.MkdirAll(ctx, filepath.Dir(fname), 0755)
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
		removeBeforeWrite()
		clog.Infof(ctx, "flush %s from embedded buf", fname)
		err = osfs.WriteFile(ctx, fname, e.buf, e.mode)
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

// matchesFileInfo reports whether fi has the same size and mtime as the entry.
func (e *entry) matchesFileInfo(fi os.FileInfo) bool {
	return fi.Size() == e.digest().SizeBytes && fi.ModTime().Equal(e.getMtime())
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
	ld, err := localDigest(ctx, src, fname)
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
	ctx, cancel := digest.ContextWithTimeout(ctx, e.d)
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
