// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package reapi

import (
	"context"
	"runtime"
	"sync"

	"go.chromium.org/build/hashigo/digest"

	"go.chromium.org/build/siso/blob"
	"go.chromium.org/build/siso/o11y/trace"
	"go.chromium.org/build/siso/sync/semaphore"
)

// DefaultUploadLimit is the default number of UploadAll requests in flight
// at once (Option.UploadLimit). It matches remoteexec's digest semaphore
// (10*GOMAXPROCS), which already bounds how many steps can be in
// UploadAll at once, so live requests keep today's parallelism while
// requests abandoned by their requester (e.g. racing losers) queue behind
// them instead of piling up.
func DefaultUploadLimit() int {
	return runtime.GOMAXPROCS(0) * 10
}

// uploadResp is the result of an upload request.
type uploadResp struct {
	numUploaded int
	err         error
}

// uploadRequest is one upload operation: the set of blobs a single
// UploadAll call found to be new (not yet known, not yet pending), to be
// processed independently of the requester's context.
type uploadRequest struct {
	ds   *blob.Store
	ops  map[digest.Digest]*uploadOp
	done chan uploadResp // buffered(1); sent exactly once.
}

// wait waits for the request to finish, or ctx to be canceled.
// The request keeps running even if ctx is canceled.
func (r *uploadRequest) wait(ctx context.Context) (int, error) {
	select {
	case <-ctx.Done():
		return 0, context.Cause(ctx)
	case resp := <-r.done:
		return resp.numUploaded, resp.err
	}
}

// fail completes the request and all of its ops with err.
func (r *uploadRequest) fail(err error) {
	for _, uop := range r.ops {
		uop.done(err)
	}
	r.done <- uploadResp{err: err}
}

// uploader runs CAS upload requests independently of the steps that
// submit them, with the Client's lifetime context, under the
// "reapi-upload" semaphore that bounds how many are in flight.
//
// Each request runs on its own goroutine, which waits for the semaphore
// and then processes the request; the semaphore's waiters are the queue.
// submit therefore never blocks the requester, and every accepted request
// is answered: by processing, or with the close cause when the uploader
// is closed while the request is waiting or in flight.
type uploader struct {
	c      *Client
	ctx    context.Context
	cancel context.CancelCauseFunc
	sema   *semaphore.Semaphore

	// mu guards closed and serializes wg.Go against Close's wg.Wait.
	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
}

func newUploader(ctx context.Context, c *Client, limit int) *uploader {
	if limit <= 0 {
		limit = DefaultUploadLimit()
	}
	ctx, cancel := context.WithCancelCause(ctx)
	return &uploader{
		c:      c,
		ctx:    ctx,
		cancel: cancel,
		sema:   semaphore.New("reapi-upload", limit),
	}
}

// submit hands newBlobs to the uploader as one request and returns
// without waiting for it. Every op in newBlobs is guaranteed to be
// completed eventually.
func (u *uploader) submit(ds *blob.Store, newBlobs map[digest.Digest]*uploadOp) *uploadRequest {
	req := &uploadRequest{
		ds:   ds,
		ops:  newBlobs,
		done: make(chan uploadResp, 1),
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed {
		req.fail(context.Cause(u.ctx))
		return req
	}
	u.wg.Go(func() {
		err := u.sema.Do(u.ctx, func(ctx context.Context) error {
			u.process(ctx, req)
			return nil
		})
		if err != nil {
			// Closed while waiting for the semaphore.
			req.fail(err)
		}
	})
	return req
}

// process runs one upload request.
func (u *uploader) process(ctx context.Context, req *uploadRequest) {
	ctx, span := trace.NewSpan(ctx, "upload-op")
	defer span.Close(nil)
	span.SetAttr("upload", len(req.ops))
	var resp uploadResp
	resp.numUploaded, resp.err = u.c.uploadNewBlobs(ctx, req.ds, req.ops, span)
	if resp.err != nil && u.ctx.Err() != nil {
		// The upload failed because the uploader was closed (or its
		// lifetime ctx canceled); report that rather than the RPC error.
		resp.err = context.Cause(u.ctx)
	}
	req.done <- resp
}

// close cancels waiting and in-flight requests and waits for them to
// finish.
func (u *uploader) close(cause error) {
	u.cancel(cause)
	u.mu.Lock()
	u.closed = true
	u.mu.Unlock()
	u.wg.Wait()
}
