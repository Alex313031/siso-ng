// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package osfs provides OS Filesystem access.
package osfs

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"runtime"
	"time"

	"github.com/pkg/xattr"

	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/o11y/iometrics"
	"go.chromium.org/build/siso/reapi/digest"
	"go.chromium.org/build/siso/runtimex"
	"go.chromium.org/build/siso/sync/semaphore"
)

// LstatSemaphore is a semaphore to control concurrent lstat,
// to protect from thread exhaustion. b/365856347
var LstatSemaphore = semaphore.New("osfs-lstat", runtimex.NumCPU()*2)

// defaultDigestXattr is default xattr for digest. http://shortn/_8GHggPD2vw
const defaultDigestXattr = "google.digest.sha256"

// OSFS provides OS Filesystem access.
// It counts metrics by iometrics.
// It would be an interface to communicate local filesystem server,
// in addition to local filesystem.
type OSFS struct {
	*iometrics.IOMetrics

	digestXattrName string
}

// Option is an option for osfs.
type Option struct {
	// DigestXattrName is xattr name for digest. When it is set, try to retrieve digest from the xattr.
	DigestXattrName string
}

func (o *Option) RegisterFlags(flagSet *flag.FlagSet) {
	var xattrname string
	if xattr.XATTR_SUPPORTED {
		xattrname = defaultDigestXattr
	}
	flagSet.StringVar(&o.DigestXattrName, "fs_digest_xattr", xattrname, "xatr for sha256 digest")
}

// New creates new OSFS.
func New(ctx context.Context, name string, opt Option) *OSFS {
	if !xattr.XATTR_SUPPORTED {
		opt.DigestXattrName = ""
	}
	if opt.DigestXattrName != "" {
		clog.Infof(ctx, "use xattr %s for file digest", opt.DigestXattrName)
	}
	return &OSFS{
		IOMetrics:       iometrics.New(name),
		digestXattrName: opt.DigestXattrName,
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
	// workaround for cog utimes bug. b/356987531
	_, _ = os.Stat(name)

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
func (ofs *OSFS) WriteFile(ctx context.Context, name string, data []byte, perm fs.FileMode) error {
	started := time.Now()
	err := writeFile(name, data, perm)
	ofs.WriteDone(len(data), err)
	if dur := time.Since(started); dur > 1*time.Minute {
		logSlow(ctx, name, dur, err)
	}
	return err
}

// WriteDigestData writes digest source into the named file.
func (ofs *OSFS) WriteDigestData(ctx context.Context, name string, src digest.Source, perm fs.FileMode) error {
	started := time.Now()
	var n int64
	err := func() error {
		r, err := src.Open(ctx)
		if err != nil {
			return err
		}
		defer r.Close()
		w, err := openForWrite(name, perm)
		if err != nil {
			return err
		}
		n, err = io.Copy(w, r)
		if err != nil {
			err = fmt.Errorf("failed to call io.Copy, read %d bytes in %s: %w", n, time.Since(started), err)
		}
		cerr := w.Close()
		if err == nil {
			err = cerr
		}
		return err
	}()
	ofs.WriteDone(int(n), err)
	if dur := time.Since(started); dur > 1*time.Minute {
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
