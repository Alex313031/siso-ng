// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package hashfs

import (
	"context"
	"runtime"
	"sync"
	"time"

	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/o11y/trace"
	"go.chromium.org/build/siso/reapi/digest"
	"go.chromium.org/build/siso/sync/semaphore"
)

// DigestSemaphore is a semaphore to control concurrent digest calculation.
var DigestSemaphore = semaphore.New("file-digest", runtime.GOMAXPROCS(0))

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
	id    string
	fname string
	e     *entry
}

type digester struct {
	quitEarly bool
	q         chan digestReq

	mu    sync.Mutex
	queue []digestReq

	quit chan struct{}
	done chan struct{}
}

func (d *digester) start(ctx context.Context) {
	defer close(d.done)
	n := runtime.GOMAXPROCS(0) - 1
	if n == 0 {
		n = 1
	}
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			d.worker(ctx)
		})
	}
	wg.Wait()
}

func (d *digester) worker(ctx context.Context) {
	for {
		select {
		case <-d.quit:
			return
		case req := <-d.q:
			dctx := ctx
			if req.id != "" {
				dctx = trace.NewContext(dctx, trace.New(dctx, req.id))
			}
			if d.quitEarly {
				dctx, cancel := context.WithCancel(dctx)
				go func() {
					d.compute(dctx, req.fname, req.e)
					cancel()
				}()
				select {
				case <-d.quit:
					cancel()
					return
				case <-dctx.Done():
					// cancel called after d.compute
				}
			} else {
				d.compute(dctx, req.fname, req.e)
			}

			d.mu.Lock()
			if len(d.queue) > 0 {
				select {
				case d.q <- d.queue[0]:
					copy(d.queue, d.queue[1:])
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
	if d.quitEarly {
		clog.Infof(ctx, "finish digester early")
		return
	}
	for req := range q {
		dctx := ctx
		if req.id != "" {
			dctx = trace.NewContext(dctx, trace.New(dctx, req.id))
		}
		d.compute(dctx, req.fname, req.e)
	}
	for _, req := range d.queue {
		dctx := ctx
		if req.id != "" {
			dctx = trace.NewContext(dctx, trace.New(dctx, req.id))
		}
		d.compute(dctx, req.fname, req.e)
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
		id:    trace.ID(ctx),
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
	e.mu.Lock()
	eErr := e.err
	src := e.src
	ed := e.d
	e.mu.Unlock()
	if eErr != nil || src == nil || !ed.IsZero() {
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
