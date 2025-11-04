// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package hashfs

import (
	"context"
	"sync"
	"time"

	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/o11y/trace"
	"go.chromium.org/build/siso/reapi/digest"
	"go.chromium.org/build/siso/runtimex"
	"go.chromium.org/build/siso/sync/semaphore"
)

// DigestSemaphore is a semaphore to control concurrent digest calculation.
var DigestSemaphore = semaphore.New("file-digest", runtimex.NumCPU())

// Keep track what files are currently accessed for digest calculation.
// On Windows, it would fail with ERROR_SHARING_VIOLATION when it
// open the file and remove the same file.
// To prevent from the error, don't remove the file in flush
// while the file is accessed for digest calculation.
var (
	digestLock   sync.Mutex
	digestCond   = sync.NewCond(&digestLock)
	digestFnames = make(map[string]struct{})
)

var noLazyForTests map[string]bool

// SetNoLazyForTest sets filenames that would not calculate digest lazily
// for test.
func SetNoLazyForTest(fnames ...string) {
	if len(fnames) == 0 {
		noLazyForTests = nil
		return
	}
	noLazyForTests = make(map[string]bool)
	for _, fname := range fnames {
		noLazyForTests[fname] = true
	}
}

func localDigest(ctx context.Context, src digest.Source, fname string) (digest.Data, error) {
	ctx, span := trace.NewSpan(ctx, "local-digest")
	defer span.Close(nil)

	digestLock.Lock()
	digestFnames[fname] = struct{}{}
	digestLock.Unlock()

	defer func() {
		digestLock.Lock()
		delete(digestFnames, fname)
		digestCond.Broadcast()
		digestLock.Unlock()
	}()
	started := time.Now()
	d, err := digest.FromLocalFile(ctx, src)
	if dur := time.Since(started); dur >= 10*time.Second {
		clog.Warningf(ctx, "too slow local digest %s %s in %s, err=%v", fname, d.Digest(), dur, err)
	}
	return d, err
}

type digestReq struct {
	ctx   context.Context
	fname string
	e     *entry
}

type digester struct {
	q chan digestReq

	mu    sync.Mutex
	queue []digestReq

	quit chan struct{}
	done chan struct{}
}

func (d *digester) start() {
	defer close(d.done)
	n := runtimex.NumCPU() - 1
	if n == 0 {
		n = 1
	}
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.worker()
		}()
	}
	wg.Wait()
}

func (d *digester) worker() {
	for {
		select {
		case <-d.quit:
			return
		case req := <-d.q:
			d.compute(req.ctx, req.fname, req.e)
			d.mu.Lock()
			if len(d.queue) > 0 {
				select {
				case d.q <- d.queue[0]:
					copy(d.queue, d.queue[:len(d.queue)-1])
					d.queue[len(d.queue)-1] = digestReq{}
					d.queue = d.queue[:len(d.queue)-1]
				default:
				}
			}
			d.mu.Unlock()
		}
	}
}

func (d *digester) stop(ctx context.Context) {
	close(d.quit)
	clog.Infof(ctx, "wait for workers")
	<-d.done
	d.mu.Lock()
	q := d.q
	d.q = nil
	d.mu.Unlock()
	close(q)
	clog.Infof(ctx, "run pending digest chan:%d + queue:%d", len(d.q), len(d.queue))
	for req := range q {
		d.compute(req.ctx, req.fname, req.e)
	}
	for _, req := range d.queue {
		d.compute(req.ctx, req.fname, req.e)
	}
	d.queue = nil
	clog.Infof(ctx, "finish digester")
}

func (d *digester) lazyCompute(ctx context.Context, fname string, e *entry) {
	e.mu.Lock()
	ed := e.d
	e.mu.Unlock()
	if !ed.IsZero() {
		return
	}
	if noLazyForTests != nil && noLazyForTests[fname] {
		clog.Warningf(ctx, "no lazy for test: %s", fname)
		return
	}
	select {
	case <-ctx.Done():
		clog.Warningf(ctx, "ignore lazyCompute %s: %v", fname, context.Cause(ctx))
		return
	default:
	}
	req := digestReq{
		ctx:   ctx,
		fname: fname,
		e:     e,
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.q == nil {
		return
	}
	select {
	case d.q <- req:
	default:
		d.queue = append(d.queue, req)
	}
}

func (d *digester) compute(ctx context.Context, fname string, e *entry) {
	if e.err != nil || e.src == nil {
		return
	}
	e.mu.Lock()
	ed := e.d
	e.mu.Unlock()
	if !ed.IsZero() {
		return
	}
	err := DigestSemaphore.Do(ctx, func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			clog.Warningf(ctx, "ignore compute %s: %v", fname, context.Cause(ctx))
			return context.Cause(ctx)
		default:
		}
		return e.compute(ctx, fname)
	})
	if err != nil {
		clog.Warningf(ctx, "failed to compute digest %s: %v", fname, err)
	}
}
