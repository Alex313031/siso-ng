// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package hashfs

import (
	"context"
	"runtime"
	"slices"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"go.chromium.org/build/hashigo/digest"

	"go.chromium.org/build/siso/blob"
	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/o11y/trace"
	"go.chromium.org/build/siso/sync/semaphore"
	"go.chromium.org/build/siso/toolsupport/abfsutil"
)

// localDigestSemaphore limits concurrent digest calculation on a local file
// digestSemaphore limits concurrent digest calculation on a local file
// system, where a digest reads and hashes the file: CPU- and disk-bound.
var localDigestSemaphore = semaphore.New("file-digest", runtime.GOMAXPROCS(0))

// abfsDigestSemaphore limits concurrent digest calculation on ABFS. There a
// digest is an HTTP call to the ABFS mount, which fetches the blob when its
// cache is cold: latency-bound, so GOMAXPROCS calls in flight leave the mount
// mostly idle, and two-phase caching's first checks wait for the digests of
// thousands of shared inputs (headers) at once.
// On ABFS, we can use batch get-rbe-digests, so fewer workers works
// better with batching.
var abfsDigestSemaphore = semaphore.New("file-digest", min(16, runtime.GOMAXPROCS(0)))

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

func localDigest(ctx context.Context, fn digest.Function, src blob.Source, fname string) (blob.Data, error) {
	ctx, span := trace.NewSpan(ctx, "local-digest")
	defer span.Close(nil)

	started := time.Now()
	d, err := blob.FromLocalFile(ctx, fn, src)
	if dur := time.Since(started); dur >= 10*time.Second {
		clog.Warningf(ctx, "too slow local digest %s %s in %s, err=%v", fname, d.Digest(), dur, err)
	}
	return d, err
}

// Batches > 1000 would hurt tail latency.
const maxDigestBatchSize = 512

type digestReq struct {
	id    string
	fname string
	e     *entry
}

type digester struct {
	fn        digest.Function
	abfs      *abfsutil.Client
	quitEarly bool
	sema      *semaphore.Semaphore
	q         chan digestReq
	wake      chan struct{}

	mu    sync.Mutex
	queue []digestReq

	quit chan struct{}
	done chan struct{}
}

func (d *digester) computeReq(ctx context.Context, req digestReq) {
	if req.id != "" {
		ctx = trace.NewContext(ctx, trace.New(ctx, req.id))
	}
	d.compute(ctx, req.fname, req.e)
}

func (d *digester) batchDigestForABFS(ctx context.Context, reqs []digestReq) {
	batchReqs := make([]digestReq, 0, len(reqs))
	for _, req := range reqs {
		req.e.mu.Lock()
		eErr := req.e.err
		src := req.e.src
		ed := req.e.d
		req.e.mu.Unlock()
		if eErr != nil || src == nil || !ed.IsZero() {
			continue
		}
		if !d.abfs.InDir(req.fname) {
			d.computeReq(ctx, req)
			continue
		}
		batchReqs = append(batchReqs, req)
	}
	if len(batchReqs) == 0 {
		return
	}
	started := time.Now()
	var digests []digest.Digest
	err := d.sema.Do(ctx, func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			clog.Warningf(ctx, "ignore batch digests: %v", context.Cause(ctx))
			return context.Cause(ctx)
		default:
		}
		// in waiting d.sema, req.e.d may have been computed
		// by other batch (lazy vs Entries).
		// If it is already computed, no need to ask again.
		fnames := make([]string, 0, len(batchReqs))
		reqs := make([]digestReq, 0, len(batchReqs))
		for _, req := range batchReqs {
			req.e.mu.Lock()
			eErr := req.e.err
			src := req.e.src
			ed := req.e.d
			req.e.mu.Unlock()
			if eErr != nil || src == nil || !ed.IsZero() {
				continue
			}
			fnames = append(fnames, req.fname)
			reqs = append(reqs, req)
		}
		if len(fnames) == 0 {
			return nil
		}
		batchReqs = reqs
		var err error
		digests, err = d.abfs.BatchDigests(ctx, fnames)
		return err
	})
	if err != nil {
		clog.Warningf(ctx, "abfs batch digests %d in %s: %v", len(batchReqs), time.Since(started), err)
		for _, req := range batchReqs {
			d.computeReq(ctx, req)
		}
		return
	}
	clog.Infof(ctx, "abfs batch digests %d in %s", len(batchReqs), time.Since(started))
	for i, dg := range digests {
		e := batchReqs[i].e
		e.mu.Lock()
		e.d = dg
		e.mu.Unlock()
	}
}

// popQueue removes and pushes the oldest request in d.queue to d.q.
//
// It advances the slice instead of shifting it: with a cold cache the
// queue can hold hundreds of thousands of requests, and copying them
// on every pop was O(n) under d.mu. append reallocates when it reaches
// the end of the backing array and copies only the live requests, so
// this is amortized O(1).
//
// When abfs available, it processes up to maxDigestBatchSize requests in d.queue in a batch.
func (d *digester) popQueue(ctx context.Context) {
	if d.abfs != nil {
		d.mu.Lock()
		n := min(len(d.queue), maxDigestBatchSize)
		var dqueue []digestReq
		if n > 0 {
			dqueue = slices.Clone(d.queue[:n])
			clear(d.queue[:n])
			d.queue = d.queue[n:]
		}
		if len(d.queue) > 0 && d.q != nil {
			// trigger worker if still in queue.
			select {
			case d.wake <- struct{}{}:
			default:
			}
		}
		d.mu.Unlock()
		if len(dqueue) == 0 {
			return
		}
		d.batchDigestForABFS(ctx, dqueue)
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.queue) == 0 {
		return
	}
	select {
	case d.q <- d.queue[0]:
		d.queue[0] = digestReq{} // don't retain the entry in the backing array.
		d.queue = d.queue[1:]
	default:
	}
}

func (d *digester) start(ctx context.Context) {
	defer close(d.done)
	// one worker fewer than the semaphore's capacity, so a foreground
	// caller of compute (Entries, Flush, Copy, ...) never waits for a slot
	// behind background work alone.
	n := max(d.sema.Capacity()-1, 1)
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
		case <-d.wake:
			select {
			case <-d.quit:
				return
			default:
			}
			d.popQueue(ctx)
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

			d.popQueue(dctx)
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
	clog.Infof(ctx, "run pending digest chan:%d + queue:%d", len(q), len(d.queue))
	if d.quitEarly {
		clog.Infof(ctx, "finish digester early")
		return
	}
	eg, ctx := errgroup.WithContext(ctx)
	eg.SetLimit(runtime.GOMAXPROCS(0))
	if d.abfs != nil {
		for len(d.queue) > 0 {
			d.popQueue(ctx)
		}
	} else {
		for req := range q {
			eg.Go(func() error {
				d.computeReq(ctx, req)
				return nil
			})
		}
		for _, req := range d.queue {
			eg.Go(func() error {
				d.computeReq(ctx, req)
				return nil
			})
		}
	}
	eg.Wait()
	d.queue = nil
	clog.Infof(ctx, "finish digester")
}

func (d *digester) lazyCompute(ctx context.Context, fname string, e *entry) {
	e.mu.Lock()
	eErr := e.err
	src := e.src
	ed := e.d
	e.mu.Unlock()
	if eErr != nil || src == nil || !ed.IsZero() {
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
	if d.abfs != nil {
		d.queue = append(d.queue, req)
		// trigger worker
		select {
		case d.wake <- struct{}{}:
		default:
		}
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
	err := d.sema.Do(ctx, func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			clog.Warningf(ctx, "ignore compute %s: %v", fname, context.Cause(ctx))
			return context.Cause(ctx)
		default:
		}
		return e.compute(ctx, d.fn, fname)
	})
	if err != nil {
		clog.Warningf(ctx, "failed to compute digest %s: %v", fname, err)
	}
}

type batchDigester struct {
	hfs   *HashFS
	wg    sync.WaitGroup
	reqs  []digestReq
	nwait int
}

func (hfs *HashFS) batchDigester() *batchDigester {
	return &batchDigester{
		hfs: hfs,
	}
}

func (b *batchDigester) start(ctx context.Context, fname string, e *entry) {
	e.mu.Lock()
	eErr := e.err
	src := e.src
	ed := e.d
	e.mu.Unlock()
	if eErr != nil || src == nil || !ed.IsZero() {
		return
	}
	b.nwait++
	if b.hfs.opt.ABFS != nil {
		b.reqs = append(b.reqs, digestReq{
			id:    trace.ID(ctx),
			fname: fname,
			e:     e,
		})
		return
	}
	b.wg.Go(func() {
		b.hfs.digester.compute(ctx, fname, e)
	})
}

func (b *batchDigester) wait(ctx context.Context) int {
	if b.hfs.opt.ABFS != nil {
		for chunk := range slices.Chunk(b.reqs, maxDigestBatchSize) {
			b.hfs.digester.batchDigestForABFS(ctx, chunk)
		}
		return b.nwait
	}
	b.wg.Wait()
	return b.nwait
}
