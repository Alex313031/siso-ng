// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package watchmanutil

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"go.chromium.org/build/siso/hashfs"
	pb "go.chromium.org/build/siso/hashfs/proto"
	"go.chromium.org/build/siso/o11y/clog"
)

// Watchman interacts with watchman.
// https://facebook.github.io/watchman/
type Watchman struct {
	watchmanPath string
	dir          string
}

// New creates watchman for dir by watchman binary at watchmanPath.
// User needs to install `watchman` in PATH,
// and run `watchman watch-project $dir` before using it.
func New(ctx context.Context, watchmanPath, dir string) (*Watchman, error) {
	w := &Watchman{
		watchmanPath: watchmanPath,
		dir:          dir,
	}
	err := w.check(ctx)
	if err != nil {
		return nil, err
	}
	return w, nil
}

type watchListData struct {
	Version string   `json:"version"`
	Roots   []string `json:"roots"`
}

func (w *Watchman) check(ctx context.Context) error {
	var wl watchListData
	buf, err := exec.CommandContext(ctx, w.watchmanPath, "watch-list").Output()
	if err != nil {
		return err
	}
	err = json.Unmarshal(buf, &wl)
	if err != nil {
		return fmt.Errorf("failed to parse watch-list %q: %w", buf, err)
	}
	clog.Infof(ctx, "watchman version: %s", wl.Version)
	dir := filepath.Clean(w.dir)
	for i := range wl.Roots {
		if filepath.Clean(wl.Roots[i]) == dir {
			return nil
		}
	}
	return fmt.Errorf("dir %s is not watched %q", dir, wl.Roots)
}

type watchClock struct {
	Version string `json:"version"`
	Clock   string `json:"clock"`
}

type watchSinceFile struct {
	Name   string `json:"name"`
	Exists bool   `json:"exists"`
	Size   int64  `json:"size"`
	Mode   int    `json:"mode"`
	Mtime  int64  `json:"mtime"`
	Nlink  int    `json:"nlink"`
}

type watchSince struct {
	Version string           `json:"version"`
	Clock   string           `json:"clock"`
	Files   []watchSinceFile `json:"files"`
}

// ClockToken returns watchman clock.
func (w *Watchman) ClockToken(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, w.watchmanPath, "clock", w.dir).Output()
	if err != nil {
		return "", err
	}
	var clk watchClock
	err = json.Unmarshal(out, &clk)
	if err != nil {
		return "", fmt.Errorf("failed to parse `watchman clock` output: %w", err)
	}
	return clk.Clock, nil
}

// Scan calls `watchman since` to finds all files that were modified since token.
func (w *Watchman) Scan(ctx context.Context, token string) (hashfs.FileInfoer, error) {
	ws := &WatchmanScan{
		w:     w,
		done:  make(chan struct{}),
		token: token,
	}
	go ws.scan(ctx)
	// Wait at most 200 msec before using FileInfo method.
	// For no-op build, `wathman scan` took <100 msec on windows/cloudtop,
	// so 200 msec would be sufficient to omit unnecessary os.Lstat call
	// in FileInfo.
	select {
	case <-time.After(200 * time.Millisecond):
		// still scanning
		return ws, nil
	case <-ws.done:
		// scan done
		return ws, nil
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	}
}

type WatchmanScan struct {
	w     *Watchman
	done  chan struct{}
	token string

	err error
	// list reported by `watchman since $dir $token`
	since watchSince
	m     map[string]*watchSinceFile
}

func (ws *WatchmanScan) scan(ctx context.Context) {
	defer close(ws.done)
	started := time.Now()
	// `watchman since` returns many files and may take time to parse the result.
	// we might want to exclude patterns that are known not to be used for build?
	out, err := exec.CommandContext(ctx, ws.w.watchmanPath, "since", ws.w.dir, ws.token).Output()
	if err != nil {
		ws.err = err
		return
	}
	err = json.Unmarshal(out, &ws.since)
	if err != nil {
		ws.err = fmt.Errorf("failed to parse watchman since: %w", err)
		return
	}
	clog.Infof(ctx, "watchman since %s->%s %d files in %s", ws.token, ws.since.Clock, len(ws.since.Files), time.Since(started))
	ws.m = make(map[string]*watchSinceFile)
	for i := range ws.since.Files {
		fname := filepath.ToSlash(filepath.Join(ws.w.dir, ws.since.Files[i].Name))
		ws.m[fname] = &ws.since.Files[i]
	}
}

type FileInfo struct {
	ent *pb.Entry
}

// Name returns base name of the file.
func (fi FileInfo) Name() string {
	return filepath.Base(fi.ent.Name)
}

// Size returns length in bytes.
func (fi FileInfo) Size() int64 {
	return fi.ent.GetDigest().GetSizeBytes()
}

// Mode returns file mode bits.
func (fi FileInfo) Mode() fs.FileMode {
	m := fs.FileMode(0644)
	if fi.ent.IsExecutable {
		m |= 0111
	}
	if fi.ent.Target != "" {
		m |= fs.ModeSymlink
	}
	if fi.IsDir() {
		m |= fs.ModeDir | 0111
	}
	return m
}

// ModTime returns modification time.
func (fi FileInfo) ModTime() time.Time {
	return time.Unix(0, fi.ent.GetId().GetModTime())
}

// IsDir returns abbreviation for Mode().IsDir().
func (fi FileInfo) IsDir() bool {
	return fi.ent.Target == "" && fi.ent.Digest.GetHash() == ""
}

// Sys returns underlying data source (*pb.Entry).
func (fi FileInfo) Sys() any {
	return fi.ent
}

// FileInfo returns file info for the entry.
func (ws *WatchmanScan) FileInfo(ctx context.Context, ent *pb.Entry) (fs.FileInfo, error) {
	select {
	case <-ws.done:
	default:
		// still scanning.
		return os.Lstat(ent.Name)
	}
	if ws.err != nil {
		// can't use scan's result
		return os.Lstat(ent.Name)
	}
	f, ok := ws.m[ent.Name]
	if !ok {
		// ent.Name is not modified since last build.
		// we can use the entry as is.
		return FileInfo{ent: ent}, nil
	}
	if !f.Exists {
		return FileInfo{}, fs.ErrNotExist
	}
	// watchman uses unix time for mtime, no subsecond granularity...
	return os.Lstat(ent.Name)
}
