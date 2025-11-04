// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package semaphore_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.chromium.org/build/siso/sync/semaphore"
)

func TestWaitAcquire(t *testing.T) {
	ctx := t.Context()
	sema := semaphore.New(t.Name(), 3)
	if n := sema.NumServs(); n != 0 {
		t.Errorf("NumServs=%d; want %d", n, 0)
	}
	if n := sema.NumWaits(); n != 0 {
		t.Errorf("NumWaits=%d; want %d", n, 0)
	}
	if n := sema.NumRequests(); n != 0 {
		t.Errorf("NumRequests=%d; want %d", n, 0)
	}

	var dones []func(error)
	for i := range 3 {
		_, done, err := sema.WaitAcquire(ctx)
		if err != nil {
			t.Fatalf("WaitAcquire %d: %v", i, err)
		}
		dones = append(dones, done)
		if n := sema.NumServs(); n != i+1 {
			t.Errorf("NumServs=%d; want %d", n, i+1)
		}
		if n := sema.NumWaits(); n != 0 {
			t.Errorf("NumWaits=%d; want %d", n, 0)
		}
		if n := sema.NumRequests(); n != i+1 {
			t.Errorf("NumRequests=%d; want %d", n, i+1)
		}
	}
	t.Logf("all acquired")
	func() {
		ctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancel()
		_, _, err := sema.WaitAcquire(ctx)
		if err == nil {
			t.Fatalf("WaitAcquire ok; want err")
		}
		if n := sema.NumServs(); n != 3 {
			t.Errorf("NumServs=%d; want %d", n, 3)
		}
		if n := sema.NumWaits(); n != 0 {
			t.Errorf("NumWaits=%d; want %d", n, 0)
		}
		if n := sema.NumRequests(); n != 3 {
			t.Errorf("NumRequests=%d; want %d", n, 3)
		}
	}()
	t.Logf("release first")
	dones[0](nil)
	if n := sema.NumServs(); n != 2 {
		t.Errorf("NumServs=%d; want %d", n, 2)
	}
	if n := sema.NumWaits(); n != 0 {
		t.Errorf("NumWaits=%d; want %d", n, 0)
	}
	if n := sema.NumRequests(); n != 3 {
		t.Errorf("NumRequests=%d; want %d", n, 3)
	}
	_, done, err := sema.WaitAcquire(ctx)
	if err != nil {
		t.Fatalf("WaitAcquire %v", err)
	}
	if n := sema.NumServs(); n != 3 {
		t.Errorf("NumServs=%d; want %d", n, 2)
	}
	if n := sema.NumWaits(); n != 0 {
		t.Errorf("NumWaits=%d; want %d", n, 0)
	}
	if n := sema.NumRequests(); n != 4 {
		t.Errorf("NumRequests=%d; want %d", n, 4)
	}
	dones[1](nil)
	dones[2](nil)
	done(nil)
	if n := sema.NumServs(); n != 0 {
		t.Errorf("NumServs=%d; want %d", n, 0)
	}
	if n := sema.NumWaits(); n != 0 {
		t.Errorf("NumWaits=%d; want %d", n, 0)
	}
	if n := sema.NumRequests(); n != 4 {
		t.Errorf("NumRequests=%d; want %d", n, 4)
	}
}

func TestTryAcquire(t *testing.T) {
	ctx := t.Context()
	sema := semaphore.New(t.Name(), 3)
	if n := sema.NumServs(); n != 0 {
		t.Errorf("NumServs=%d; want %d", n, 0)
	}
	if n := sema.NumWaits(); n != 0 {
		t.Errorf("NumWaits=%d; want %d", n, 0)
	}
	if n := sema.NumRequests(); n != 0 {
		t.Errorf("NumRequests=%d; want %d", n, 0)
	}

	var dones []func(error)
	for i := range 3 {
		_, done, err := sema.TryAcquire(ctx)
		if err != nil {
			t.Fatalf("TryAcquire %d: %v", i, err)
		}
		dones = append(dones, done)
		if n := sema.NumServs(); n != i+1 {
			t.Errorf("NumServs=%d; want %d", n, i+1)
		}
		if n := sema.NumWaits(); n != 0 {
			t.Errorf("NumWaits=%d; want %d", n, 0)
		}
		if n := sema.NumRequests(); n != i+1 {
			t.Errorf("NumRequests=%d; want %d", n, i+1)
		}
	}
	t.Logf("all acquired")
	_, _, err := sema.TryAcquire(ctx)
	if err == nil {
		t.Fatalf("TryAcquire ok; want err")
	}
	if n := sema.NumServs(); n != 3 {
		t.Errorf("NumServs=%d; want %d", n, 3)
	}
	if n := sema.NumWaits(); n != 0 {
		t.Errorf("NumWaits=%d; want %d", n, 0)
	}
	if n := sema.NumRequests(); n != 3 {
		t.Errorf("NumRequests=%d; want %d", n, 3)
	}
	t.Logf("release first")
	dones[0](nil)
	if n := sema.NumServs(); n != 2 {
		t.Errorf("NumServs=%d; want %d", n, 2)
	}
	if n := sema.NumWaits(); n != 0 {
		t.Errorf("NumWaits=%d; want %d", n, 0)
	}
	if n := sema.NumRequests(); n != 3 {
		t.Errorf("NumRequests=%d; want %d", n, 3)
	}
	_, done, err := sema.TryAcquire(ctx)
	if err != nil {
		t.Fatalf("TryAcquire %v", err)
	}
	if n := sema.NumServs(); n != 3 {
		t.Errorf("NumServs=%d; want %d", n, 2)
	}
	if n := sema.NumWaits(); n != 0 {
		t.Errorf("NumWaits=%d; want %d", n, 0)
	}
	if n := sema.NumRequests(); n != 4 {
		t.Errorf("NumRequests=%d; want %d", n, 4)
	}
	dones[1](nil)
	dones[2](nil)
	done(nil)
	if n := sema.NumServs(); n != 0 {
		t.Errorf("NumServs=%d; want %d", n, 0)
	}
	if n := sema.NumWaits(); n != 0 {
		t.Errorf("NumWaits=%d; want %d", n, 0)
	}
	if n := sema.NumRequests(); n != 4 {
		t.Errorf("NumRequests=%d; want %d", n, 4)
	}
}

// TestDo tests the Do function.
func TestDo(t *testing.T) {
	ctx := t.Context()
	sema := semaphore.New(t.Name(), 3)
	if n := sema.NumServs(); n != 0 {
		t.Errorf("NumServs=%d; want %d", n, 0)
	}
	if n := sema.NumWaits(); n != 0 {
		t.Errorf("NumWaits=%d; want %d", n, 0)
	}
	if n := sema.NumRequests(); n != 0 {
		t.Errorf("NumRequests=%d; want %d", n, 0)
	}

	var called atomic.Int32
	const count = 50
	var wg sync.WaitGroup
	for i := range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := sema.Do(ctx, func(context.Context) error {
				called.Add(1)
				return nil
			})
			if err != nil {
				t.Errorf("Do %d: %v", i, err)
			}
		}()
	}
	wg.Wait()
	if n := sema.NumServs(); n != 0 {
		t.Errorf("NumServs=%d; want %d", n, 0)
	}
	if n := sema.NumWaits(); n != 0 {
		t.Errorf("NumWaits=%d; want %d", n, 0)
	}
	if n := sema.NumRequests(); n != count {
		t.Errorf("NumRequests=%d; want %d", n, count)
	}
	if n := called.Load(); int(n) != count {
		t.Errorf("called=%d; want %d", n, count)
	}
}

func TestDo_err(t *testing.T) {
	ctx := t.Context()
	sema := semaphore.New(t.Name(), 3)
	if n := sema.NumServs(); n != 0 {
		t.Errorf("NumServs=%d; want %d", n, 0)
	}
	if n := sema.NumWaits(); n != 0 {
		t.Errorf("NumWaits=%d; want %d", n, 0)
	}
	if n := sema.NumRequests(); n != 0 {
		t.Errorf("NumRequests=%d; want %d", n, 0)
	}

	var wantErr = errors.New("error")

	f := func(ctx context.Context) error {
		return wantErr
	}

	err := sema.Do(ctx, f)
	if !errors.Is(err, wantErr) {
		t.Errorf("Do %v; want %v", err, wantErr)
	}
}

func TestDo_timeout(t *testing.T) {
	started := time.Now()
	ctx := t.Context()
	const count = 3
	sema := semaphore.New(t.Name(), count)

	done := make(chan struct{})
	f := func(ctx context.Context) error {
		t.Logf("%s f called", time.Since(started))
		select {
		case <-ctx.Done():
		case <-done:
		}
		t.Logf("%s f return %v", time.Since(started), ctx.Err())
		return ctx.Err()
	}
	var wg sync.WaitGroup
	for i := range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := sema.Do(ctx, f)
			if err != nil {
				t.Logf("%s Do %d: %v", time.Since(started), i, err)
			}
		}()
	}

	if n := sema.Capacity(); n != count {
		t.Errorf("Capacity=%d; want %d", n, count)
	}
	t.Logf("wait until semaphore becomes busy")
	waitStart := time.Now()
	for time.Since(waitStart) < 1*time.Second {
		if n := sema.NumServs(); n != count {
			t.Logf("%s check servs=%d", time.Since(started), n)
			time.Sleep(1 * time.Millisecond)
		}
	}
	if n := sema.NumServs(); n != count {
		t.Fatalf("%s NumServs=%d; want %d", time.Since(started), n, count)
	}
	t.Logf("now semaphore is busy")
	{
		ctx, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
		defer cancel()
		err := sema.Do(ctx, func(ctx context.Context) error {
			return nil
		})
		t.Logf("%s Do timeout %v", time.Since(started), err)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("%s Do timeout %v; want %v", time.Since(started), err, context.DeadlineExceeded)
		}
	}
	close(done)
	wg.Wait()
}
