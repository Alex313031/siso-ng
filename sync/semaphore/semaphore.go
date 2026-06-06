// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package semaphore provives semaphore.
package semaphore

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/status"

	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/o11y/trace"
)

var tidCounter atomic.Int64

func init() {
	// Perfetto does not show a row for thread id 0.
	tidCounter.Store(1)
}

// Monitorable is an interface for semaphore monitoring.
type Monitorable interface {
	Name() string
	Capacity() int
	NumServs() int
	NumWaits() int
	NumRequests() int
	Stat() (wait, serv Stat)
}

// Semaphore is a semaphore.
type Semaphore struct {
	name string
	ch   chan int

	waitSpanName string
	servSpanName string

	waits atomic.Int64
	reqs  atomic.Int64

	waitStat stat
	servStat stat
}

// New creates a new semaphore with name and capacity.
func New(name string, n int) *Semaphore {
	ch := make(chan int, n)
	startTID := int(tidCounter.Add(int64(n))) - n
	for i := range n {
		ch <- startTID + i // tid
	}
	s := &Semaphore{
		name:         fmt.Sprintf("%s/%d", name, n),
		ch:           ch,
		waitSpanName: fmt.Sprintf("wait:%s/%d", name, n),
		servSpanName: fmt.Sprintf("serv:%s/%d", name, n),
	}
	return s
}

// WaitAcquire acquires a semaphore.
// It returns a context for acquired semaphore and func to release it.
func (s *Semaphore) WaitAcquire(ctx context.Context) (context.Context, func(error), error) {
	_, span := trace.NewSpan(ctx, s.waitSpanName)
	s.waits.Add(1)
	defer span.Close(nil)
	defer s.waits.Add(-1)
	now := time.Now()
	select {
	case tid := <-s.ch:
		s.reqs.Add(1)
		ctx, span := trace.NewSpan(ctx, s.servSpanName)
		span.SetTid(tid)
		dur := time.Since(now)
		if dur > 1*time.Second {
			clog.Infof(ctx, "wait %s for %s", s.name, dur)
		}
		s.waitStat.update(dur, false)
		now = time.Now()
		return ctx, func(err error) {
			st, ok := status.FromError(err)
			if !ok {
				st = status.FromContextError(err)
			}
			span.Close(st.Proto())
			s.servStat.update(time.Since(now), err != nil)
			s.ch <- tid
		}, nil
	case <-ctx.Done():
		s.waitStat.update(time.Since(now), true)
		return ctx, func(error) {}, context.Cause(ctx)
	}
}

var errNotAvailable = errors.New("semaphore: not available")

// TryAcquire acquires a semaphore if available, or return error.
// It returns a context for acquired semaphore and func to release it.
func (s *Semaphore) TryAcquire(ctx context.Context) (context.Context, func(error), error) {
	now := time.Now()
	select {
	case tid := <-s.ch:
		s.reqs.Add(1)
		s.waitStat.update(time.Since(now), false)
		ctx, span := trace.NewSpan(ctx, s.servSpanName)
		span.SetTid(tid)
		now = time.Now()
		return ctx, func(err error) {
			st, ok := status.FromError(err)
			if !ok {
				st = status.FromContextError(err)
			}
			span.Close(st.Proto())
			s.servStat.update(time.Since(now), err != nil)
			s.ch <- tid
		}, nil
	default:
		return ctx, func(error) {}, errNotAvailable
	}
}

// Name returns name of the semaphore.
func (s *Semaphore) Name() string {
	return s.name
}

// Capacity returns capacity of the semaphore.
func (s *Semaphore) Capacity() int {
	return cap(s.ch)
}

// NumServs returns number of currently served.
func (s *Semaphore) NumServs() int {
	return cap(s.ch) - len(s.ch)
}

// NumWaits returns number of waiters.
func (s *Semaphore) NumWaits() int {
	return int(s.waits.Load())
}

// NumRequests returns total number of requests.
func (s *Semaphore) NumRequests() int {
	return int(s.reqs.Load())
}

// Do runs f under semaphore.
func (s *Semaphore) Do(ctx context.Context, f func(ctx context.Context) error) (err error) {
	var done func(error)
	ctx, done, err = s.WaitAcquire(ctx)
	if err != nil {
		return err
	}
	defer func() { done(err) }()
	err = f(ctx)
	return err
}

// Stat returns semaphore stat.
func (s *Semaphore) Stat() Stat {
	if s == nil {
		return Stat{}
	}
	return newStat(s.name, &s.waitStat, &s.servStat)
}
