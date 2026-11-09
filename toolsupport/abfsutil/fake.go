// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package abfsutil

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"

	"go.chromium.org/build/hashigo/digest"
)

// Fake is a fake ABFS server for testing.
type Fake struct {
	Dir string

	// Store maps digest to file content for set-rbe-digests.
	Store map[digest.Digest][]byte

	mu            sync.Mutex
	getRBEDigests map[string]int
	setRBEDigests map[string]int
}

func (f *Fake) recordGet(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getRBEDigests == nil {
		f.getRBEDigests = make(map[string]int)
	}
	f.getRBEDigests[path]++
}

func (f *Fake) recordSet(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setRBEDigests == nil {
		f.setRBEDigests = make(map[string]int)
	}
	f.setRBEDigests[path]++
}

// GetRBEDigests returns a copy of the recorded get-rbe-digest(s) counts per path.
func (f *Fake) GetRBEDigests() map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return maps.Clone(f.getRBEDigests)
}

// SetRBEDigests returns a copy of the recorded set-rbe-digests counts per path.
func (f *Fake) SetRBEDigests() map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return maps.Clone(f.setRBEDigests)
}

func (f *Fake) statPath(path string) (rbePathStat, error) {
	fullPath := filepath.Join(f.Dir, path)
	fi, err := os.Lstat(fullPath)
	if err != nil {
		return rbePathStat{}, err
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		target, err := os.Readlink(fullPath)
		if err != nil {
			return rbePathStat{}, err
		}
		h := sha256.Sum256([]byte(target))
		return rbePathStat{
			Path:   path,
			SHA256: hex.EncodeToString(h[:]),
			Size:   int64(len(target)),
			Mode:   uint32(syscall.S_IFLNK | 0777),
		}, nil
	}
	data, err := os.ReadFile(fullPath)
	if err != nil {
		return rbePathStat{}, err
	}
	h := sha256.Sum256(data)
	return rbePathStat{
		Path:   path,
		SHA256: hex.EncodeToString(h[:]),
		Size:   int64(len(data)),
		Mode:   uint32(fs.FileMode(syscall.S_IFREG) | fi.Mode()),
	}, nil
}

func (f *Fake) handleGetDigest(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, "wrong method", http.StatusMethodNotAllowed)
		return
	}
	path := req.URL.Query().Get("path")
	if path == "" {
		http.Error(w, "missing path parameter", http.StatusBadRequest)
		return
	}
	f.recordGet(path)
	stat, err := f.statPath(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, fmt.Sprintf("lstat: %v", err), http.StatusInternalServerError)
		return
	}
	buf, err := json.Marshal(stat)
	if err != nil {
		http.Error(w, fmt.Sprintf("marshal error: %v", err), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(buf)
}

func (f *Fake) handleGetDigests(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "wrong method", http.StatusMethodNotAllowed)
		return
	}
	reqBody, err := io.ReadAll(req.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("req body: %v", err), http.StatusInternalServerError)
		return
	}
	var reqMsg getRBEDigestsReq
	err = json.Unmarshal(reqBody, &reqMsg)
	if err != nil {
		http.Error(w, fmt.Sprintf("req parse: %v", err), http.StatusInternalServerError)
		return
	}
	var resp []rbePathStat
	for _, path := range reqMsg.Paths {
		f.recordGet(path)
		stat, err := f.statPath(path)
		if err != nil {
			continue
		}
		resp = append(resp, stat)
	}
	if resp == nil {
		resp = []rbePathStat{}
	}
	buf, err := json.Marshal(resp)
	if err != nil {
		http.Error(w, fmt.Sprintf("marshal error: %v", err), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(buf)
}

func (f *Fake) handleSetDigests(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "wrong method", http.StatusMethodNotAllowed)
		return
	}
	reqBody, err := io.ReadAll(req.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("req body: %v", err), http.StatusInternalServerError)
		return
	}
	var reqMsg setRBEDigestsReq
	err = json.Unmarshal(reqBody, &reqMsg)
	if err != nil {
		http.Error(w, fmt.Sprintf("req parse: %v", err), http.StatusInternalServerError)
		return
	}
	respMsg := make(map[string]string)
	for _, r := range reqMsg.Digests {
		f.recordSet(r.Path)
		d := digest.Digest{Hash: r.SHA256, SizeBytes: r.Size}
		f.mu.Lock()
		data, ok := f.Store[d]
		f.mu.Unlock()
		if !ok {
			respMsg[r.Path] = fmt.Sprintf("unknown digest %s/%d", r.SHA256, r.Size)
			continue
		}
		targetPath := filepath.Join(f.Dir, r.Path)
		err := os.MkdirAll(filepath.Dir(targetPath), 0755)
		if err == nil {
			err = os.WriteFile(targetPath, data, fs.FileMode(r.Mode)&fs.ModePerm)
		}
		if err != nil {
			respMsg[r.Path] = fmt.Sprintf("failed to write %q: %v", r.Path, err)
		}
	}
	respBody, err := json.Marshal(respMsg)
	if err != nil {
		http.Error(w, fmt.Sprintf("resp marshal: %v", err), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(respBody)
}

// Handler returns an http.Handler that handles ABFS endpoints.
func (f *Fake) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/mnt/get-rbe-digest", f.handleGetDigest)
	mux.HandleFunc("/mnt/get-rbe-digests", f.handleGetDigests)
	mux.HandleFunc("/mnt/set-rbe-digests", f.handleSetDigests)
	return mux
}

// Start starts the fake ABFS HTTP server on a unix domain socket at sockPath.
func (f *Fake) Start(ctx context.Context, t *testing.T, sockPath string) *Client {
	t.Helper()
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix %s: %v", sockPath, err)
	}
	srv := &httptest.Server{
		Listener: lis,
		Config:   &http.Server{Handler: f.Handler()},
	}
	srv.Start()
	client, err := New(ctx, "unix://"+sockPath, f.Dir)
	if err != nil {
		srv.Close()
		t.Fatalf("abfsutil.New: %v", err)
	}
	t.Cleanup(func() {
		client.Close()
		srv.Close()
	})
	return client
}
