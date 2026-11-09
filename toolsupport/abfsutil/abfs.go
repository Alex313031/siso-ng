// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package abfsutil

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	log "github.com/golang/glog"

	"go.chromium.org/build/hashigo/digest"

	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/reapi/merkletree"
)

const (
	defaultBatchDelay = 1 * time.Millisecond
	maxBatchSize      = 16
)

// Client is abfs client.
type Client struct {
	endpoint string
	dir      string
	client   *http.Client

	wg         sync.WaitGroup
	regCh      chan registerReq
	regWorkers int
	regDone    func()
}

type registerItem struct {
	stat rbePathStat
	ent  *Registration
}

type registerReq struct {
	items []registerItem
	resp  chan error
}

// New creates new abfs client mounted at dir.
func New(ctx context.Context, endpoint, dir string) (*Client, error) {
	numCPU := runtime.GOMAXPROCS(0)
	tr := &http.Transport{
		// no proxy
		Proxy: func(*http.Request) (*url.URL, error) {
			return nil, nil
		},
		MaxIdleConns:        numCPU,
		MaxIdleConnsPerHost: numCPU,
	}
	endpoint = strings.TrimSuffix(endpoint, "/")
	sockPath, ok := strings.CutPrefix(endpoint, "unix://")
	if ok {
		if !filepath.IsAbs(sockPath) {
			sockPath = filepath.Join(dir, sockPath)
		}
		tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sockPath)
		}
		// go's http requires http or https scheme.
		endpoint = "http://unix"
	}
	c := &Client{
		endpoint: endpoint,
		dir:      dir,
		client: &http.Client{
			Transport: tr,
		},
		regCh:      make(chan registerReq, numCPU),
		regWorkers: numCPU,
	}
	c.registerBatcher(ctx)
	return c, nil
}

// Close closes client to abfs server.
func (c *Client) Close() error {
	if c == nil || c.regDone == nil {
		return nil
	}
	c.regDone()
	c.wg.Wait()
	return nil
}

// Registration is a registration for a file.
type Registration struct {
	Entry merkletree.Entry

	// result of RegisterFiles
	Err error
}

// rbePathStat is json object used with ABFS.
type rbePathStat struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Mode   uint32 `json:"mode,omitzero"`
}

// getRBEDigestsReq is json object of get-rbe-digests request.
type getRBEDigestsReq struct {
	Paths []string `json:"paths"`
}

// setRBEDigestsReq is json object of set-rbe-digests request.
type setRBEDigestsReq struct {
	Digests []rbePathStat `json:"digests"`
}

// InDir reports whether fname is within the abfs mount directory.
func (c *Client) InDir(fname string) bool {
	if c == nil || c.dir == "" {
		return false
	}
	relpath, err := filepath.Rel(c.dir, fname)
	return err == nil && filepath.IsLocal(relpath)
}

// Digest returns digest of the file.
// file is absolute path.
func (c *Client) Digest(ctx context.Context, fname string) (digest.Digest, error) {
	if c == nil || c.endpoint == "" || c.dir == "" || c.client == nil {
		return digest.Digest{}, errors.ErrUnsupported
	}
	relpath, err := filepath.Rel(c.dir, fname)
	if err != nil {
		return digest.Digest{}, fmt.Errorf("abfs: get digest: out of dir: %q", fname)
	}
	if !filepath.IsLocal(relpath) {
		return digest.Digest{}, fmt.Errorf("abfs: get digest: out of dir: %q", fname)
	}
	relpath = filepath.ToSlash(relpath)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint+"/mnt/get-rbe-digest?path="+url.QueryEscape(relpath), nil)
	if err != nil {
		return digest.Digest{}, fmt.Errorf("abfs: get digest: new request: %w", err)
	}
	req.Header.Add("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return digest.Digest{}, fmt.Errorf("abfs: get digest: get request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return digest.Digest{}, fmt.Errorf("abfs: get digest: read resp: %w", err)
	}
	switch resp.StatusCode {
	case http.StatusOK: // 200
	case http.StatusNotFound: // 404
		return digest.Digest{}, fmt.Errorf("abfs: get digest: not found %q: %w", fname, fs.ErrNotExist)
	default:
		return digest.Digest{}, fmt.Errorf("abfs: get digest: error %d: %s", resp.StatusCode, body)
	}

	var result rbePathStat
	err = json.Unmarshal(body, &result)
	if err != nil {
		return digest.Digest{}, fmt.Errorf("abfs: get digest: parse resp %q: %w", body, err)
	}
	return digest.Digest{
		Hash:      result.SHA256,
		SizeBytes: result.Size,
	}, nil
}

// BatchDigests returns digests of the files.
// file is absolute path.
func (c *Client) BatchDigests(ctx context.Context, fnames []string) ([]digest.Digest, error) {
	if c == nil || c.endpoint == "" || c.dir == "" || c.client == nil {
		return nil, errors.ErrUnsupported
	}
	reqMsg := getRBEDigestsReq{
		Paths: make([]string, 0, len(fnames)),
	}
	for _, fname := range fnames {
		relpath, err := filepath.Rel(c.dir, fname)
		if err != nil {
			return nil, fmt.Errorf("abfs: get digests: out of dir: %q", fname)
		}
		if !filepath.IsLocal(relpath) {
			return nil, fmt.Errorf("abfs: get digests: out of dir: %q", fname)
		}
		relpath = filepath.ToSlash(relpath)
		reqMsg.Paths = append(reqMsg.Paths, relpath)
	}
	reqBuf, err := json.Marshal(reqMsg)
	if err != nil {
		return nil, fmt.Errorf("abfs: marshal get-rbe-digests request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/mnt/get-rbe-digests", bytes.NewReader(reqBuf))
	if err != nil {
		return nil, fmt.Errorf("abfs: get digests: new request: %w", err)
	}
	req.Header.Add("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("abfs: get digests: resp %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("abfs: get digests: read resp: %w", err)
	}
	switch resp.StatusCode {
	case http.StatusOK: // 200
	default:
		return nil, fmt.Errorf("abfs: get digests: error %d: %s", resp.StatusCode, body)
	}
	var result []rbePathStat
	err = json.Unmarshal(body, &result)
	if err != nil {
		return nil, fmt.Errorf("abfs: get digests: parse resp %q: %w", body, err)
	}
	m := make(map[string]digest.Digest)
	for _, r := range result {
		m[r.Path] = digest.Digest{
			Hash:      r.SHA256,
			SizeBytes: r.Size,
		}
	}
	digests := make([]digest.Digest, 0, len(reqMsg.Paths))
	for _, relpath := range reqMsg.Paths {
		digests = append(digests, m[relpath])
	}
	return digests, nil
}

// RegisterFiles registers entries under dir.
func (c *Client) RegisterFiles(ctx context.Context, dir string, entries []*Registration) error {
	if c == nil || c.endpoint == "" || c.dir == "" || c.client == nil || c.regCh == nil {
		return errors.ErrUnsupported
	}
	items := make([]registerItem, 0, len(entries))
	for _, ent := range entries {
		if ent == nil {
			continue
		}
		fullpath := filepath.Join(dir, string(ent.Entry.Name))
		relpath, err := filepath.Rel(c.dir, fullpath)
		if log.V(1) {
			clog.Infof(ctx, "abfs: entry %q %q -> %q: %v", dir, ent.Entry.Name, relpath, err)
		}
		if err != nil {
			ent.Err = fmt.Errorf("abfs: out of dir: %q", ent.Entry.Name)
			clog.Warningf(ctx, "%v", ent.Err)
			continue
		}
		if !filepath.IsLocal(relpath) {
			ent.Err = fmt.Errorf("abfs: out of dir: %q", ent.Entry.Name)
			clog.Warningf(ctx, "%v", ent.Err)
			continue
		}
		relpath = filepath.ToSlash(relpath)
		d := ent.Entry.Data.Digest()
		if d.IsZero() {
			ent.Err = fmt.Errorf("abfs: empty digest: %q", ent.Entry.Name)
			clog.Warningf(ctx, "%v", ent.Err)
			continue
		}
		mode := fs.FileMode(syscall.S_IFREG | 0o644)
		if ent.Entry.IsExecutable {
			mode |= 0o111
		}
		items = append(items, registerItem{
			stat: rbePathStat{
				Path:   relpath,
				SHA256: d.Hash,
				Size:   d.SizeBytes,
				Mode:   uint32(mode),
			},
			ent: ent,
		})
	}
	return c.registerFiles(ctx, items)
}

func (c *Client) registerFiles(ctx context.Context, items []registerItem) error {
	if len(items) == 0 {
		return nil
	}
	resp := make(chan error, 1)
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case c.regCh <- registerReq{items: items, resp: resp}:
	}
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case err := <-resp:
		return err
	}
}

func (c *Client) registerBatcher(ctx context.Context) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	c.regDone = cancel
	type batch struct {
		items []registerItem
		resps []chan error
	}
	ch := make(chan batch, 1000)
	c.wg.Go(func() {
		for {
			var items []registerItem
			var resps []chan error
			select {
			case <-ctx.Done():
				return
			case req := <-c.regCh:
				items = append(items, req.items...)
				resps = append(resps, req.resp)
			}
			if len(items) >= maxBatchSize {
				select {
				case <-ctx.Done():
					for _, resp := range resps {
						resp <- context.Cause(ctx)
					}
					return
				case ch <- batch{items: items, resps: resps}:
				}
				continue
			}
			timer := time.After(defaultBatchDelay)
		batchLoop:
			for len(items) < maxBatchSize {
				select {
				case <-ctx.Done():
					for _, resp := range resps {
						resp <- context.Cause(ctx)
					}
					return
				case <-timer:
					break batchLoop
				case req := <-c.regCh:
					items = append(items, req.items...)
					resps = append(resps, req.resp)
				}
			}
			select {
			case <-ctx.Done():
				for _, resp := range resps {
					resp <- context.Cause(ctx)
				}
				return
			case ch <- batch{items: items, resps: resps}:
			}
		}

	})
	for range c.regWorkers {
		c.wg.Go(func() {
			for {
				select {
				case <-ctx.Done():
					return
				case b := <-ch:
					err := c.sendRegisterBatch(ctx, b.items, len(b.resps))
					for _, resp := range b.resps {
						resp <- err
					}
				}
			}
		})
	}
}

func (c *Client) sendRegisterBatch(ctx context.Context, items []registerItem, numCalls int) error {
	started := time.Now()
	reqMsg := setRBEDigestsReq{
		Digests: make([]rbePathStat, len(items)),
	}
	for i, item := range items {
		reqMsg.Digests[i] = item.stat
	}
	reqBody, err := json.Marshal(reqMsg)
	if err != nil {
		return fmt.Errorf("abfs: set digests: marshal req: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/mnt/set-rbe-digests", bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("abfs: set digests: new request: %w", err)
	}
	req.Header.Add("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		clog.Warningf(ctx, "abfs set-rbe-digests %d files (%d calls) in %v: %v", len(items), numCalls, time.Since(started), err)
		return fmt.Errorf("abfs: set digests: post request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("abfs: set digests: read resp: %w", err)
	}
	switch resp.StatusCode {
	case http.StatusOK:
	default:
		return fmt.Errorf("abfs: set digests: error %d: %s", resp.StatusCode, body)
	}
	clog.Infof(ctx, "abfs set-rbe-digests %d files (%d calls) in %v", len(items), numCalls, time.Since(started))
	if len(body) == 0 {
		return nil
	}
	var result map[string]string
	err = json.Unmarshal(body, &result)
	if err != nil {
		return fmt.Errorf("abfs: set digests: parse resp %q: %w", body, err)
	}
	if len(result) == 0 {
		return nil
	}
	matched := make(map[string]bool, len(result))
	for _, item := range items {
		if v, ok := result[item.stat.Path]; ok {
			item.ent.Err = errors.New(v)
			matched[item.stat.Path] = true
		}
	}
	for k, v := range result {
		if !matched[k] {
			clog.Warningf(ctx, "abfs: set digests: resp unknown path %q: %s", k, v)
		}
	}
	return nil
}
