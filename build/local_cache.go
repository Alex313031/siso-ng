// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"golang.org/x/sync/singleflight"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/o11y/iometrics"
	"go.chromium.org/build/siso/o11y/trace"
	"go.chromium.org/build/siso/reapi/digest"
	"go.chromium.org/build/siso/ui"
)

// LocalCache implements CacheStore interface with local files.
type LocalCache struct {
	dir string

	singleflight singleflight.Group
	m            *iometrics.IOMetrics
	timestamp    time.Time
}

// There is an upper bound on lifespan of 2 * TTL, since something that's
// expired may not actually be picked up again until the next garbage
// collection, which may not be for TTL.
const localCacheTTL = 7 * 24 * time.Hour

// NewLocalCache returns new local cache.
func NewLocalCache(dir string) (*LocalCache, error) {
	if dir == "" {
		return nil, errors.New("local cache is not configured")
	}
	return &LocalCache{
		dir: dir,
		m:   iometrics.New("local-cache"),
		// Use the same timestamp throughout the build. Makes things simpler.
		timestamp: time.Now(),
	}, nil
}

// IOMetrics returns io metrics of the local cache.
func (c *LocalCache) IOMetrics() *iometrics.IOMetrics {
	if c == nil {
		return nil
	}
	return c.m
}

func (c *LocalCache) actionCacheFilename(d digest.Digest) string {
	name := fmt.Sprintf("%s-%d", d.Hash, d.SizeBytes)
	return filepath.Join(c.dir, "actions", name[:2], name[2:])
}

func (c *LocalCache) contentCacheFilename(d digest.Digest) string {
	name := fmt.Sprintf("%s-%d.gz", d.Hash, d.SizeBytes)
	return filepath.Join(c.dir, "contents", name[:2], name[2:])
}

// GetActionResult gets the action result of the action identified by the digest.
func (c *LocalCache) GetActionResult(ctx context.Context, d digest.Digest) (*rpb.ActionResult, error) {
	if c == nil {
		return nil, status.Error(codes.NotFound, "cache is not configured")
	}
	fname := c.actionCacheFilename(d)
	b, err := os.ReadFile(fname)
	c.m.ReadDone(len(b), err)
	if errors.Is(err, os.ErrNotExist) {
		return nil, status.Errorf(codes.NotFound, "not found %s: %v", fname, err)
	}
	if err != nil {
		return nil, err
	}
	result := &rpb.ActionResult{}
	err = proto.Unmarshal(b, result)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal %s: %w", fname, err)
	}

	if err := os.Chtimes(fname, c.timestamp, c.timestamp); err != nil {
		clog.Warningf(ctx, "Failed to update mtime for %s: %v", fname, err)
	}
	return result, nil
}

// SetActionResult sets the action result of the action identified by the digest.
// If a failing action is provided, caching will be skipped.
func (c *LocalCache) SetActionResult(ctx context.Context, d digest.Digest, ar *rpb.ActionResult) error {
	// Don't cache failing actions, as RBE won't either.
	if c == nil || ar.ExitCode != 0 {
		return nil
	}
	b, err := proto.Marshal(ar)
	if err != nil {
		return nil
	}

	fname := c.actionCacheFilename(d)
	_, err, _ = c.singleflight.Do(fname, func() (any, error) {
		err := os.MkdirAll(filepath.Dir(fname), 0755)
		c.m.OpsDone(err)
		if err != nil {
			return nil, err
		}
		// Write to a temporary file first before renaming to perform an atomic
		// write.
		tmp := fname + ".tmp"
		err = os.WriteFile(tmp, b, 0644)
		c.m.WriteDone(len(b), err)
		if err != nil {
			c.m.OpsDone(os.Remove(tmp))
			return nil, err
		}
		err = os.Rename(tmp, fname)
		c.m.OpsDone(err)
		if err != nil {
			c.m.OpsDone(os.Remove(tmp))
			return nil, err
		}
		return nil, nil
	})
	return err
}

// GetContent returns content of the fname identified by the digest.
func (c *LocalCache) GetContent(ctx context.Context, d digest.Digest, _ string) ([]byte, error) {
	_, span := trace.NewSpan(ctx, "cache-get-content")
	defer span.Close(nil)
	cname := c.contentCacheFilename(d)
	r, err := os.Open(cname)
	if err != nil {
		c.m.ReadDone(0, err)
		return nil, err
	}
	defer r.Close()
	gr, err := gzip.NewReader(r)
	if err != nil {
		c.m.ReadDone(0, err)
		return nil, err
	}
	defer gr.Close()
	buf, err := io.ReadAll(gr)
	// TODO(b/274060507): local cache metric: iometrics uses compressed size or uncompressed size?
	c.m.ReadDone(len(buf), err)
	if err == nil {
		if err := os.Chtimes(cname, c.timestamp, c.timestamp); err != nil {
			clog.Warningf(ctx, "Failed to update mtime for %s: %v", cname, err)
		}
	}
	return buf, err
}

// SetContent sets content of fname identified by the digest.
func (c *LocalCache) SetContent(ctx context.Context, d digest.Digest, fname string, buf []byte) error {
	cname := c.contentCacheFilename(d)
	_, err := os.Stat(cname)
	c.m.OpsDone(err)
	if err == nil {
		return nil
	}
	_, err, shared := c.singleflight.Do(cname, func() (any, error) {
		err = os.MkdirAll(filepath.Dir(cname), 0755)
		c.m.OpsDone(err)
		if err != nil {
			return nil, err
		}
		// Write to a temporary file first before renaming to perform an atomic
		// write.
		tmp := cname + ".tmp"
		w, err := os.Create(tmp)
		if err != nil {
			c.m.WriteDone(0, err)
			return nil, err
		}
		gw := gzip.NewWriter(w)
		_, err = gw.Write(buf)
		if err != nil {
			c.m.WriteDone(0, err)
			w.Close()
			os.Remove(tmp)
			return nil, err
		}
		err = gw.Close()
		if err != nil {
			c.m.WriteDone(0, err)
			w.Close()
			os.Remove(tmp)
			return nil, err
		}
		err = w.Close()
		if err != nil {
			c.m.WriteDone(0, err)
			os.Remove(tmp)
			return nil, err
		}
		err = os.Rename(tmp, cname)
		if err != nil {
			c.m.WriteDone(0, err)
			os.Remove(tmp)
			return nil, err
		}
		// TODO(b/274060507): local cache metric: iometrics uses compressed size or uncompressed size?
		c.m.WriteDone(len(buf), err)
		return nil, err
	})
	clog.Infof(ctx, "write cache content %s for %s shared:%t: %v", d, fname, shared, err)
	return err
}

type dataWriteCloser struct {
	wc    io.WriteCloser
	cname string
	f     *os.File
	m     *iometrics.IOMetrics
	n     int
	d     digest.Digest
}

func (w *dataWriteCloser) Write(buf []byte) (int, error) {
	n, err := w.wc.Write(buf)
	w.n += n
	return n, err
}

func (w *dataWriteCloser) Close() error {
	err := w.wc.Close()
	cerr := w.f.Close()
	if err == nil {
		err = cerr
	}
	if int64(w.n) != w.d.SizeBytes {
		err = fmt.Errorf("early EOF for %s (%d bytes): %w", w.d, w.n, err)
	}
	w.m.WriteDone(w.n, err)
	fname := w.f.Name()
	if err != nil {
		w.m.OpsDone(os.Remove(fname))
		return err
	}
	err = os.Rename(fname, w.cname)
	w.m.OpsDone(err)
	if err != nil {
		w.m.OpsDone(os.Remove(fname))
		// Consider losing a race as success
		_, err = os.Stat(w.cname)
		w.m.OpsDone(err)
	}
	return err
}

// ContentSink opens a temporary file for writing and renames it into place on close.
// If the target file already exists, returns (nil, nil).
func (c *LocalCache) ContentSink(ctx context.Context, d digest.Digest, fname string) (io.WriteCloser, error) {
	cname := c.contentCacheFilename(d)
	_, err := os.Stat(cname)
	c.m.OpsDone(err)
	if err == nil {
		return nil, nil
	}
	err = os.MkdirAll(filepath.Dir(cname), 0755)
	c.m.OpsDone(err)
	if err != nil {
		return nil, err
	}
	// Write to a temporary file first before renaming to perform an atomic
	// write.
	f, err := os.CreateTemp(filepath.Dir(cname), filepath.Base(cname))
	c.m.OpsDone(err)
	if err != nil {
		return nil, err
	}
	clog.Infof(ctx, "write cache content %s for %s", d, fname)
	gw := gzip.NewWriter(f)
	return &dataWriteCloser{wc: gw, f: f, cname: cname, m: c.m, d: d}, nil
}

// HasContent checks whether content of the digest exists in the local cache.
func (c *LocalCache) HasContent(ctx context.Context, d digest.Digest) bool {
	cname := c.contentCacheFilename(d)
	_, err := os.Stat(cname)
	c.m.OpsDone(err)
	return err == nil
}

func garbageCollect(ctx context.Context, dir string, threshold time.Time) (nFiles int, spaceReclaimed int64) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		clog.Warningf(ctx, "Failed to read %s: %v", dir, err)
		return 0, 0
	}

	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		if entry.IsDir() {
			n, s := garbageCollect(ctx, path, threshold)
			nFiles += n
			spaceReclaimed += s
		} else {
			// There's no OS-independent way to use atime, so we just use mtime and
			// ensure that when we read a file we also update the mtime.
			info, err := os.Stat(path)
			if err != nil {
				clog.Warningf(ctx, "Failed to stat file %s: %v", path, err)
				continue
			}
			if info.ModTime().Before(threshold) {
				if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
					clog.Warningf(ctx, "Failed to delete %s: %v", filepath.Join(dir, entry.Name()), err)
				}
				nFiles += 1
				spaceReclaimed += info.Size()
			}
		}
	}
	return nFiles, spaceReclaimed
}

func (c *LocalCache) needsGarbageCollection(ttl time.Duration) bool {
	if c == nil {
		return false
	}
	bytes, err := os.ReadFile(filepath.Join(c.dir, "lastgc"))
	if err != nil {
		if _, err := os.Stat(c.dir); os.IsNotExist(err) {
			return false
		}
		return true
	}
	lastgc, err := strconv.ParseInt(string(bytes), 10, 64)
	if err != nil {
		return true
	}
	return c.timestamp.After(time.Unix(0, lastgc).Add(ttl))
}

func (c *LocalCache) garbageCollect(ctx context.Context, ttl time.Duration) {
	if c == nil {
		return
	}
	spin := ui.Default.NewSpinner()
	spin.Start("Performing garbage collection on the local cache")

	clog.Infof(ctx, "Performing garbage collection on the local cache")
	// God this is gross. time.Sub only takes in times, and time.Add only takes
	// in durations.
	threshold := c.timestamp.Add(-ttl)
	nFiles, spaceReclaimed := garbageCollect(ctx, filepath.Join(c.dir, "contents"), threshold)
	nActions, sActions := garbageCollect(ctx, filepath.Join(c.dir, "actions"), threshold)
	nFiles += nActions
	spaceReclaimed += sActions
	if nFiles > 0 {
		clog.Infof(ctx, "Garbage collected local cache: Removed %d files totalling %d MB", nFiles, spaceReclaimed/1000000)
	}

	if err := os.WriteFile(filepath.Join(c.dir, "lastgc"), fmt.Append(nil, c.timestamp.UnixNano()), 0644); err != nil {
		clog.Warningf(ctx, "Failed to record last garbage collection event: %v", err)
	}
	spin.Stop(nil)
}

// GarbageCollectIfRequired performs garbage collection if it has not been performed within localCacheTTL.
func (c *LocalCache) GarbageCollectIfRequired(ctx context.Context) {
	if c.needsGarbageCollection(localCacheTTL) {
		c.garbageCollect(ctx, localCacheTTL)
	}
}

// Source returns digest source for fname identified by the digest.
func (c *LocalCache) Source(_ context.Context, d digest.Digest, fname string) digest.Source {
	return dataSource{c: c, d: d, fname: fname, m: c.IOMetrics()}
}

type dataReadCloser struct {
	io.ReadCloser
	f io.Closer
	m *iometrics.IOMetrics
	n int
}

func (r *dataReadCloser) Read(buf []byte) (int, error) {
	n, err := r.ReadCloser.Read(buf)
	r.n += n
	return n, err
}

func (r *dataReadCloser) Close() error {
	err := r.ReadCloser.Close()
	if r.f != nil {
		cerr := r.f.Close()
		if err == nil {
			err = cerr
		}
	}
	r.m.ReadDone(r.n, err)
	return err
}

type dataSource struct {
	c     *LocalCache
	d     digest.Digest
	fname string
	m     *iometrics.IOMetrics
}

func (s dataSource) Open(ctx context.Context) (io.ReadCloser, error) {
	if s.d.SizeBytes == 0 {
		return io.NopCloser(bytes.NewReader(nil)), nil
	}
	if s.c == nil || s.c.dir == "" {
		return nil, errors.New("cache is not configured")
	}
	name := fmt.Sprintf("%s-%d.gz", s.d.Hash, s.d.SizeBytes)
	cname := filepath.Join(s.c.dir, "contents", name[:2], name[2:])
	r, err := os.Open(cname)
	if err != nil {
		var err2 error
		r, err2 = os.Open(s.fname)
		if err2 != nil {
			clog.Warningf(ctx, "failed to open cached-digest data %s for %s: %v %v", s.d, s.fname, err, err2)
			s.m.ReadDone(0, err)
			return nil, err
		}
		clog.Infof(ctx, "use %s (failed to open cached-digest data %s: %v)", s.fname, s.d, err)
		return &dataReadCloser{ReadCloser: r, m: s.m}, nil
	}
	gr, err := gzip.NewReader(r)
	if err != nil {
		r.Close()
		clog.Warningf(ctx, "failed to gunzip cached-digest data %s for %s: %v", s.d, s.fname, err)
		s.m.ReadDone(0, err)
		return nil, err
	}
	return &dataReadCloser{ReadCloser: gr, f: r, m: s.m}, nil
}

func (s dataSource) String() string {
	return fmt.Sprintf("cache %s for %s", s.d, s.fname)
}
