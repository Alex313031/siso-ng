// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package osfs provides OS Filesystem access.
package osfs

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/pkg/xattr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/o11y/iometrics"
	"go.chromium.org/build/siso/o11y/monitoring"
	"go.chromium.org/build/siso/reapi/digest"
	"go.chromium.org/build/siso/reapi/firstbyte"
	"go.chromium.org/build/siso/sync/semaphore"
	"go.chromium.org/build/siso/toolsupport/cartfsutil"
)

// LstatSemaphore is a semaphore to control concurrent lstat,
// to protect from thread exhaustion. b/365856347
var LstatSemaphore = semaphore.New("osfs-lstat", runtime.GOMAXPROCS(0)*4)

const writeBufSize = 96 * 1024

var bufWriterPool = sync.Pool{
	New: func() any {
		return bufio.NewWriterSize(nil, writeBufSize)
	},
}

// defaultDigestXattr is default xattr for digest. http://shortn/_8GHggPD2vw
const defaultDigestXattr = "google.digest.sha256"

// OSFS provides OS Filesystem access.
// It counts metrics by iometrics.
// It would be an interface to communicate local filesystem server,
// in addition to local filesystem.
type OSFS struct {
	*iometrics.IOMetrics

	digestXattrName string
	onCog           bool
}

// Option is an option for osfs.
type Option struct {
	// DigestXattrName is xattr name for digest. If empty, defaults
	// to google.digest.sha256 on Cog/CartFS
	// and stays empty elsewhere;
	// set explicitly to opt in on other filesystems that publish it.
	DigestXattrName string

	// OnCog indicates the exec root is on the Cog filesystem.
	// Enables a stat-before-utimes workaround for b/356987531.
	OnCog bool
	// CartFS is client of CartFS.
	// TODO(b/513044090): decide xattr or GetDigest API.
	CartFS *cartfsutil.Client
}

func (o *Option) RegisterFlags(flagSet *flag.FlagSet) {
	flagSet.StringVar(&o.DigestXattrName, "fs_digest_xattr", "", "xattr for sha256 digest; empty enables the default on Cog/CartFS only")
}

// New creates new OSFS.
func New(ctx context.Context, name string, opt Option) *OSFS {
	digestXattrName := opt.DigestXattrName
	if digestXattrName == "" && xattr.XATTR_SUPPORTED && (opt.OnCog || opt.CartFS != nil) {
		digestXattrName = defaultDigestXattr
	}
	if !xattr.XATTR_SUPPORTED {
		digestXattrName = ""
	}
	if digestXattrName != "" {
		clog.Infof(ctx, "use xattr %s for file digest", digestXattrName)
	}
	return &OSFS{
		IOMetrics:       iometrics.New(name),
		digestXattrName: digestXattrName,
		onCog:           opt.OnCog,
	}
}

func logSlow(ctx context.Context, name string, dur time.Duration, err error) {
	buf := make([]byte, 4*1024)
	n := runtime.Stack(buf, false)
	clog.Warningf(ctx, "slow op %s: %s %v\n%s", name, dur, err, buf[:n])
}

// Chmod changes the mode of the named file to mode.
func (ofs *OSFS) Chmod(ctx context.Context, name string, mode fs.FileMode) error {
	started := time.Now()
	err := os.Chmod(name, mode)
	ofs.OpsDone(err)
	if dur := time.Since(started); dur > 1*time.Minute {
		logSlow(ctx, name, dur, err)
	}
	return err
}

// Chtimes changes the access and modification times of the named file.
func (ofs *OSFS) Chtimes(ctx context.Context, name string, atime, mtime time.Time) error {
	started := time.Now()
	if ofs.onCog {
		// workaround for cog utimes bug. b/356987531
		_, _ = os.Stat(name)
	}

	err := os.Chtimes(name, atime, mtime)
	ofs.OpsDone(err)
	if dur := time.Since(started); dur > 1*time.Minute {
		logSlow(ctx, name, dur, err)
	}
	return err
}

// AsFileSource asserts digest.Source value holds FileSource type,
// and return bool whether it holds or not.
func (*OSFS) AsFileSource(ds digest.Source) (FileSource, bool) {
	s, ok := ds.(FileSource)
	return s, ok
}

// FileSource creates new FileSource for name.
// For FileDigestFromXattr, if size is non-negative, it will be used.
// If size is negative, it will check file info.
func (ofs *OSFS) FileSource(name string, size int64) FileSource {
	return FileSource{Fname: name, size: size, fs: ofs}
}

// Lstat returns a FileInfo describing the named file.
func (ofs *OSFS) Lstat(ctx context.Context, fname string) (fs.FileInfo, error) {
	started := time.Now()
	var fi fs.FileInfo
	err := LstatSemaphore.Do(ctx, func(ctx context.Context) error {
		var err error
		fi, err = os.Lstat(fname)
		return err
	})
	ofs.OpsDone(err)
	if dur := time.Since(started); dur > 1*time.Minute {
		logSlow(ctx, fname, dur, err)
	}
	return fi, err
}

// MkdirAll creates a directory named path, along with any necessary parents.
func (ofs *OSFS) MkdirAll(ctx context.Context, dirname string, perm fs.FileMode) error {
	started := time.Now()
	err := os.MkdirAll(dirname, perm)
	ofs.OpsDone(err)
	if dur := time.Since(started); dur > 1*time.Minute {
		logSlow(ctx, dirname, dur, err)
	}
	return err
}

// Readlink returns the destination of the named symbolic link.
func (ofs *OSFS) Readlink(ctx context.Context, name string) (string, error) {
	started := time.Now()
	target, err := os.Readlink(name)
	ofs.OpsDone(err)
	if dur := time.Since(started); dur > 1*time.Minute {
		logSlow(ctx, name, dur, err)
	}
	return target, err
}

// Remove removes the named file or directory.
func (ofs *OSFS) Remove(ctx context.Context, name string) error {
	started := time.Now()
	err := os.Remove(name)
	ofs.OpsDone(err)
	if dur := time.Since(started); dur > 1*time.Minute {
		logSlow(ctx, name, dur, err)
	}
	return err
}

// Rename renames oldpath to newpath.
func (ofs *OSFS) Rename(ctx context.Context, oldpath, newpath string) error {
	started := time.Now()
	err := os.Rename(oldpath, newpath)
	ofs.OpsDone(err)
	if dur := time.Since(started); dur > 1*time.Minute {
		logSlow(ctx, newpath, dur, err)
	}
	return err
}

// Symlink creates newname as a symbolic link to oldname.
func (ofs *OSFS) Symlink(ctx context.Context, oldname, newname string) error {
	started := time.Now()
	err := os.Symlink(oldname, newname)
	ofs.OpsDone(err)
	if dur := time.Since(started); dur > 1*time.Minute {
		logSlow(ctx, newname, dur, err)
	}
	return err
}

// WriteFile writes data to the named file, creating it if necessary.
//
// Executables are written directly, even though the write-mode fd briefly
// makes them un-execable on Linux: ETXTBSY at exec time is handled by
// retry in localexec (see localexec.run), which also covers writers
// outside siso's control. https://github.com/golang/go/issues/22315
func (ofs *OSFS) WriteFile(ctx context.Context, name string, data []byte, perm fs.FileMode) error {
	started := time.Now()
	err := os.WriteFile(name, data, perm)
	ofs.WriteDone(len(data), err)
	if dur := time.Since(started); dur > 1*time.Minute {
		logSlow(ctx, name, dur, err)
	}
	return err
}

type measuringReader struct {
	r io.Reader

	mu    sync.Mutex
	ops   int64
	bytes int64
	dur   time.Duration
}

func (r *measuringReader) Read(buf []byte) (int, error) {
	start := time.Now()
	n, err := r.r.Read(buf)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ops++
	r.bytes += int64(n)
	r.dur += time.Since(start)
	return n, err
}

func (r *measuringReader) opsPerSec() float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.dur == 0 {
		return 0
	}
	return float64(r.ops) / r.dur.Seconds()
}

func (r *measuringReader) bytesPerSec() float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.dur == 0 {
		return 0
	}
	return float64(r.bytes) / r.dur.Seconds()
}

func (r *measuringReader) stats() (int64, int64, time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ops, r.bytes, r.dur
}

type measuringWriter struct {
	w     io.Writer
	ops   int64
	bytes int64
	dur   time.Duration
}

func (w *measuringWriter) Write(buf []byte) (int, error) {
	start := time.Now()
	n, err := w.w.Write(buf)
	w.ops++
	w.bytes += int64(n)
	w.dur += time.Since(start)
	return n, err
}

func (w *measuringWriter) opsPerSec() float64 {
	if w.dur == 0 {
		return 0
	}
	return float64(w.ops) / w.dur.Seconds()
}

func (w *measuringWriter) bytesPerSec() float64 {
	if w.dur == 0 {
		return 0
	}
	return float64(w.bytes) / w.dur.Seconds()
}

type firstByteTimeoutCtxKey struct{}

// WithFirstByteTimeout opts a single WriteDigestData call into the
// pre-first-byte watchdog with the given duration. Without this on
// ctx, no watchdog runs.
func (*OSFS) WithFirstByteTimeout(ctx context.Context, d time.Duration) context.Context {
	return context.WithValue(ctx, firstByteTimeoutCtxKey{}, d)
}

func firstByteTimeoutFromCtx(ctx context.Context) (time.Duration, bool) {
	d, ok := ctx.Value(firstByteTimeoutCtxKey{}).(time.Duration)
	return d, ok
}

// WriteDigestData writes digest source into the named file.
func (ofs *OSFS) WriteDigestData(ctx context.Context, name string, src digest.Source, perm fs.FileMode, timeout time.Duration) error {
	started := time.Now()
	var n int64
	var rd measuringReader
	var wr measuringWriter
	// fbSig.Fired() closes on the gRPC InPayload event (true
	// wire-level first byte), so reads done inside Open() -- e.g.
	// newDecoder's zstd probe -- don't affect the watchdog. Non-gRPC
	// sources (local file, in-memory bytes) never fire but also never
	// stall long enough to matter.
	ctx, fbSig := firstbyte.WithSignal(ctx)
	// cancel() below cancels the derived ctx, not callerCtx, so
	// callerCtx.Err() != nil means the caller canceled, not a watchdog.
	callerCtx := ctx
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(context.Canceled)

	// Pre-first-byte watchdog only fires if the caller opted in via
	// WithFirstByteTimeout. Default is no watchdog.
	if fbt, ok := firstByteTimeoutFromCtx(ctx); ok && fbt > 0 {
		go func() {
			select {
			case <-fbSig.Fired():
				return
			case <-ctx.Done():
				return
			case <-time.After(fbt):
				monitoring.RecordCancellation(ctx, "bytestream-read", "pre_first_byte")
				cancel(status.Errorf(codes.Aborted, "no first byte in %s: %s", fbt, time.Since(started)))
			}
		}()
	}

	go func() {
		// watchdog for reader.
		// if no operations in timeout, abort the operations.
		var prevOps, prevBytes int64
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(timeout):
			}
			ops, bytes, dur := rd.stats()
			if prevOps == ops || prevBytes == bytes {
				monitoring.RecordCancellation(ctx, "bytestream-read", "no_ops")
				cancel(status.Errorf(codes.Aborted, "no ops in %s: ops=%d bytes=%d dur=%s %s", timeout, ops, bytes, dur, time.Since(started)))
				return
			}
			prevOps = ops
			prevBytes = bytes
		}
	}()
	err := func() error {
		r, err := src.Open(ctx)
		if err != nil {
			return err
		}
		defer r.Close()
		rd.r = r
		w, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
		if err != nil {
			return err
		}
		wr.w = w
		// for standard-pd, Write IOPS per GiB is 1.5 and Throughput
		// per GiB (MiBps) is 0.12.
		// If it uses max IOPS, we can write at most 0.12*1024/1.5 =
		// 81.3KB per IOPS.
		// Use 96KB buffer to reduce IOPS.
		bufw := bufWriterPool.Get().(*bufio.Writer)
		bufw.Reset(&wr)
		n, err = io.Copy(bufw, &rd)
		if err != nil {
			err = fmt.Errorf("failed to call io.Copy, read %d bytes in %s: %w", n, time.Since(started), err)
		}
		berr := bufw.Flush()
		if err == nil {
			err = berr
		}
		bufw.Reset(nil)
		bufWriterPool.Put(bufw)
		cerr := w.Close()
		if err == nil {
			err = cerr
		}
		return err
	}()
	// Caller cancellation must surface even on a successful copy, else
	// flushWrite renames/Chtimes after the caller gave up. A watchdog that
	// fired while the copy still completed is a success, not Aborted; only
	// surface its cause when io.Copy actually failed.
	switch {
	case callerCtx.Err() != nil:
		err = context.Cause(callerCtx)
	case err != nil && ctx.Err() != nil:
		err = context.Cause(ctx)
	}
	ofs.WriteDone(int(n), err)
	if dur := time.Since(started); dur > 1*time.Minute {
		name = fmt.Sprintf("%s r:%.02fop/s %.02fb/s w:%.02fop/s %.02fb/s", name, rd.opsPerSec(), rd.bytesPerSec(), wr.opsPerSec(), wr.bytesPerSec())
		logSlow(ctx, name, dur, err)
	}
	return err
}

// FileDigestFromXattr returns file's digest via xattr if possible.
func (ofs *OSFS) FileDigestFromXattr(ctx context.Context, name string, size int64) (digest.Digest, error) {
	if ofs.digestXattrName == "" {
		return digest.Digest{}, errors.ErrUnsupported
	}
	d, err := xattr.LGet(name, ofs.digestXattrName)
	ofs.OpsDone(err)
	if err != nil {
		return digest.Digest{}, err
	}
	if size < 0 {
		fi, err := os.Lstat(name)
		ofs.OpsDone(err)
		size = fi.Size()
	}
	return digest.Digest{
		Hash:      string(d),
		SizeBytes: size,
	}, nil
}

// FileSource is a file source.
type FileSource struct {
	Fname string
	size  int64
	fs    *OSFS
}

// IsLocal indicates FileSource is local file source.
func (FileSource) IsLocal() {}

// Open opens the named file for reading.
func (fsc FileSource) Open(ctx context.Context) (io.ReadCloser, error) {
	r, err := os.Open(fsc.Fname)
	return &file{ctx: ctx, file: r, started: time.Now(), fs: fsc.fs}, err
}

func (fsc FileSource) String() string {
	return fmt.Sprintf("file://%s", fsc.Fname)
}

// FileDigestFromXattr returns file's digest via xattr if possible.
func (fsc FileSource) FileDigestFromXattr(ctx context.Context) (digest.Digest, error) {
	return fsc.fs.FileDigestFromXattr(ctx, fsc.Fname, fsc.size)
}

type file struct {
	ctx     context.Context
	file    *os.File
	started time.Time
	fs      *OSFS
	n       int
}

func (f *file) Read(buf []byte) (int, error) {
	n, err := f.file.Read(buf)
	f.n += n
	return n, err
}

func (f *file) Close() error {
	name := f.file.Name()
	err := f.file.Close()
	f.fs.ReadDone(f.n, err)
	if dur := time.Since(f.started); dur > 1*time.Minute {
		logSlow(f.ctx, name, dur, err)
	}
	return err
}
