// Copyright 2025 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package semaphore

import (
	"container/heap"
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/status"

	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/o11y/trace"
)

const (
	stateWaiting = iota
	stateAcquired
	stateCanceled
)

// request represents a pending request in the priority queue.
type request struct {
	weight int
	ready  chan int
	index  int // The index of the request in the heap.
	state  atomic.Int32
}

// priorityQueue implements heap.Interface and holds requests.
// For now, it's a placeholder.
type priorityQueue []*request

func (pq priorityQueue) Len() int { return len(pq) }

func (pq priorityQueue) Less(i, j int) bool {
	// Higher weight has higher priority.
	return pq[i].weight > pq[j].weight
}

func (pq priorityQueue) Swap(i, j int) {
	pq[i], pq[j] = pq[j], pq[i]
	pq[i].index = i
	pq[j].index = j
}

func (pq *priorityQueue) Push(x any) {
	n := len(*pq)
	req := x.(*request)
	req.index = n
	*pq = append(*pq, req)
}

func (pq *priorityQueue) Pop() any {
	old := *pq
	n := len(old)
	req := old[n-1]
	old[n-1] = nil // avoid memory leak
	req.index = -1 // for safety
	*pq = old[0 : n-1]
	return req
}

// Prioritized is a semaphore that prioritizes waiters by weight.
type Prioritized struct {
	name     string
	capacity int

	mu   sync.Mutex
	used int
	pq   priorityQueue

	waitSpanName string
	servSpanName string

	waits atomic.Int64
	reqs  atomic.Int64
}

// NewPrioritized creates a new priority semaphore with a name and capacity.
func NewPrioritized(name string, n int) *Prioritized {
	s := &Prioritized{
		name:         fmt.Sprintf("%s/%d", name, n),
		capacity:     n,
		waitSpanName: fmt.Sprintf("wait:%s/%d", name, n),
		servSpanName: fmt.Sprintf("serv:%s/%d", name, n),
	}
	heap.Init(&s.pq)
	return s
}

// WaitAcquire acquires a semaphore slot, waiting if necessary.
// Higher weight requests are prioritized.
func (s *Prioritized) WaitAcquire(ctx context.Context, weight int) (context.Context, func(error), error) {
	_, waitSpan := trace.NewSpan(ctx, s.waitSpanName)
	waitSpan.SetAttr("weight", weight)
	s.waits.Add(1)
	defer waitSpan.Close(nil)
	defer s.waits.Add(-1)
	now := time.Now()

	s.mu.Lock()
	// If there's capacity, acquire immediately.
	if s.used < s.capacity {
		tid := s.used
		s.used++
		s.mu.Unlock()
		s.reqs.Add(1)
		ctx, servSpan := trace.NewSpan(ctx, s.servSpanName)
		servSpan.SetAttr("tid", tid)
		servSpan.SetAttr("weight", weight)
		return ctx, s.onServeCompleteFunc(servSpan, tid), nil
	}

	// Otherwise, wait in the priority queue.
	req := &request{
		weight: weight,
		ready:  make(chan int),
	}
	heap.Push(&s.pq, req)
	s.mu.Unlock()

	select {
	case tid := <-req.ready:
		// Acquired the semaphore.
		s.reqs.Add(1)
		if dur := time.Since(now); dur > 1*time.Second {
			clog.Infof(ctx, "wait-priority %s for %s (weight: %d)", s.name, dur, weight)
		}
		ctx, servSpan := trace.NewSpan(ctx, s.servSpanName)
		servSpan.SetAttr("tid", tid)
		servSpan.SetAttr("weight", weight)
		return ctx, s.onServeCompleteFunc(servSpan, tid), nil
	case <-ctx.Done():
		oerr := context.Cause(ctx)
		// Attempt to atomically cancel the request.
		if !req.state.CompareAndSwap(stateWaiting, stateCanceled) {
			// The request was already acquired. We lost the race.
			// Block until the ready signal is sent to maintain invariants.
			tid := <-req.ready
			s.reqs.Add(1)
			ctx, servSpan := trace.NewSpan(ctx, s.servSpanName)
			servSpan.SetAttr("tid", tid)
			servSpan.SetAttr("weight", weight)
			return ctx, s.onServeCompleteFunc(servSpan, tid), nil
		}

		// We successfully canceled. Remove from the queue.
		s.mu.Lock()
		if req.index != -1 {
			heap.Remove(&s.pq, req.index)
		}
		s.mu.Unlock()
		return ctx, func(error) {}, oerr
	}
}

func (s *Prioritized) onServeCompleteFunc(servSpan *trace.Span, tid int) func(error) {
	return func(err error) {
		if servSpan != nil {
			st, ok := status.FromError(err)
			if !ok {
				st = status.FromContextError(err)
			}
			servSpan.Close(st.Proto())
		}
		s.privateRelease(tid)
	}
}

// privateRelease releases a semaphore slot.
func (s *Prioritized) privateRelease(tid int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for s.pq.Len() > 0 {
		req := heap.Pop(&s.pq).(*request)
		if req.state.CompareAndSwap(stateWaiting, stateAcquired) {
			// Successfully acquired, signal the waiter.
			req.ready <- tid
			return
		}
		// The request was canceled, try the next one.
	}
	// No other requests were waiting. Decrement `used` counter.
	s.used--
}

// Name returns name of the semaphore.
func (s *Prioritized) Name() string {
	return s.name
}

// Capacity returns capacity of the semaphore.
func (s *Prioritized) Capacity() int {
	return s.capacity
}

// NumServs returns number of currently served.
func (s *Prioritized) NumServs() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.used
}

// NumWaits returns number of waiters.
func (s *Prioritized) NumWaits() int {
	return int(s.waits.Load())
}

// NumRequests returns total number of requests.
func (s *Prioritized) NumRequests() int {
	return int(s.reqs.Load())
}

// Do runs f under the semaphore with the given weight.
func (s *Prioritized) Do(ctx context.Context, weight int, f func(ctx context.Context) error) (err error) {
	var done func(error)
	ctx, done, err = s.WaitAcquire(ctx, weight)
	if err != nil {
		return err
	}
	defer func() { done(err) }()

	// After acquiring, check if the context has been canceled.
	if ctx.Err() != nil {
		return ctx.Err()
	}

	err = f(ctx)
	return err
}
