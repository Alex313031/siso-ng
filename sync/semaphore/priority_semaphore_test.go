// Copyright 2025 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package semaphore_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.chromium.org/build/siso/sync/semaphore"
)

// TestPrioritized_BasicAcquireRelease tests the fundamental acquire and release logic.
func TestPrioritized_BasicAcquireRelease(t *testing.T) {
	ctx := t.Context()
	sema := semaphore.NewPrioritized(t.Name(), 2)

	// Acquire the first slot.
	_, done1, err := sema.WaitAcquire(ctx, 1)
	if err != nil {
		t.Fatalf("first WaitAcquire failed: %v", err)
	}
	if sema.NumServs() != 1 {
		t.Errorf("NumServs() = %d; want 1", sema.NumServs())
	}
	// Acquire the second slot.
	_, done2, err := sema.WaitAcquire(ctx, 1)
	if err != nil {
		t.Fatalf("second WaitAcquire failed: %v", err)
	}
	if sema.NumServs() != 2 {
		t.Errorf("NumServs() = %d; want 2", sema.NumServs())
	}

	// All slots are now taken. The next acquire should block.
	errCh := make(chan error, 1)
	go func() {
		_, _, err := sema.WaitAcquire(ctx, 1)
		errCh <- err
	}()

	select {
	case err := <-errCh:
		t.Fatalf("WaitAcquire should have blocked, but returned with: %v", err)
	case <-time.After(20 * time.Millisecond):
		// This is expected. The goroutine is waiting.
	}

	// Release the first slot. This should unblock the waiting goroutine.
	done1(nil)

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("blocked WaitAcquire returned an unexpected error: %v", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("WaitAcquire did not unblock after a slot was released")
	}

	if sema.NumServs() != 2 {
		t.Errorf("NumServs() after unblocking = %d; want 2", sema.NumServs())
	}

	// Release the second slot.
	done2(nil)
	if sema.NumServs() != 1 {
		t.Errorf("NumServs() after second release = %d; want 1", sema.NumServs())
	}
}

// TestPrioritized_Prioritization verifies that higher-weight requests are processed first.
func TestPrioritized_Prioritization(t *testing.T) {
	ctx := t.Context()
	sema := semaphore.NewPrioritized(t.Name(), 1)

	// Acquire the only slot to force subsequent requests to wait.
	_, done, err := sema.WaitAcquire(ctx, 1)
	if err != nil {
		t.Fatalf("initial WaitAcquire failed: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	order := make(chan int, 2)

	// Start a low-priority waiter.
	go func() {
		defer wg.Done()
		sema.Do(ctx, 1, func(context.Context) error {
			order <- 1
			return nil
		})
	}()

	// Start a high-priority waiter.
	go func() {
		defer wg.Done()
		sema.Do(ctx, 10, func(context.Context) error {
			order <- 10
			return nil
		})
	}()

	// Poll until both waiters are in the queue.
	for sema.NumWaits() < 2 {
		time.Sleep(10 * time.Millisecond)
	}

	// Release the initial slot, which should trigger the waiters.
	done(nil)

	// Wait for both Do calls to complete.
	wg.Wait()
	close(order)

	// Check the execution order.
	if p := <-order; p != 10 {
		t.Fatalf("first executed task had priority %d; want 10", p)
	}
	if p := <-order; p != 1 {
		t.Fatalf("second executed task had priority %d; want 1", p)
	}

	// After all tasks are done, check the counters.
	if sema.NumServs() != 0 {
		t.Errorf("NumServs() = %d; want 0", sema.NumServs())
	}
	if sema.NumWaits() != 0 {
		t.Errorf("NumWaits() = %d; want 0", sema.NumWaits())
	}
	if sema.NumRequests() != 3 {
		t.Errorf("NumRequests() = %d; want 3", sema.NumRequests())
	}
}

// TestPrioritized_SkipCanceledRequest tests that a canceled request in the middle of the queue
// is correctly skipped in favor of the next-highest-priority non-canceled request.
func TestPrioritized_SkipCanceledRequest(t *testing.T) {
	ctx := t.Context()
	sema := semaphore.NewPrioritized(t.Name(), 1)

	// Acquire the only slot to force subsequent requests to wait.
	_, done, err := sema.WaitAcquire(ctx, 1)
	if err != nil {
		t.Fatalf("initial WaitAcquire failed: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(3)
	order := make(chan int, 2)
	errCh := make(chan error, 1)

	// Enqueue waiters in a shuffled order: Middle -> High -> Low.
	// 1. Middle-priority waiter (will be canceled)
	cancelCtx, cancel := context.WithCancel(ctx)
	go func() {
		defer wg.Done()
		_, _, err := sema.WaitAcquire(cancelCtx, 10)
		errCh <- err
	}()

	// 2. High-priority waiter (should run first)
	go func() {
		defer wg.Done()
		sema.Do(ctx, 20, func(context.Context) error {
			order <- 20
			return nil
		})
	}()

	// 3. Low-priority waiter (should run second)
	go func() {
		defer wg.Done()
		sema.Do(ctx, 1, func(context.Context) error {
			order <- 1
			return nil
		})
	}()

	// Poll until all 3 waiters are in the queue.
	for sema.NumWaits() < 3 {
		time.Sleep(10 * time.Millisecond)
	}

	// Cancel the middle-priority waiter.
	cancel()

	// Give a moment for the cancellation to be processed.
	time.Sleep(20 * time.Millisecond)

	// Release the initial slot. This should unblock the highest-priority (20) waiter.
	done(nil)

	// Check that the canceled waiter received the correct error.
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled WaitAcquire returned %v; want context.Canceled", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("canceled WaitAcquire did not return an error")
	}

	// The high-priority waiter should execute.
	if p := <-order; p != 20 {
		t.Fatalf("first executed task had priority %d; want 20", p)
	}

	// The low-priority waiter should execute next.
	if p := <-order; p != 1 {
		t.Fatalf("second executed task had priority %d; want 1", p)
	}

	// Wait for all goroutines to finish.
	wg.Wait()
	close(order)
	close(errCh)

	// Check final semaphore state.
	if sema.NumServs() != 0 {
		t.Errorf("NumServs() = %d; want 0", sema.NumServs())
	}
	if sema.NumWaits() != 0 {
		t.Errorf("NumWaits() = %d; want 0", sema.NumWaits())
	}
	if sema.NumRequests() != 3 {
		t.Errorf("NumRequests() = %d; want 3", sema.NumRequests())
	}
}

// TestPrioritized_MultiCapacityPrioritization verifies that prioritization works correctly
// when the semaphore has a capacity greater than 1.
func TestPrioritized_MultiCapacityPrioritization(t *testing.T) {
	ctx := t.Context()
	const capacity = 3
	sema := semaphore.NewPrioritized(t.Name(), capacity)

	// Acquire all initial slots to force subsequent requests to wait.
	var initialDones []func(error)
	for i := range capacity {
		_, done, err := sema.WaitAcquire(ctx, 1)
		if err != nil {
			t.Fatalf("initial WaitAcquire %d failed: %v", i, err)
		}
		initialDones = append(initialDones, done)
	}

	priorities := []int{10, 1, 50, 5, 20}
	expectedOrder := []int{50, 20, 10, 5, 1}

	var wg sync.WaitGroup
	wg.Add(len(priorities))
	order := make(chan int)

	for _, p := range priorities {
		go func() {
			defer wg.Done()
			sema.Do(ctx, p, func(context.Context) error {
				order <- p
				return nil
			})
		}()
	}

	// Poll until all waiters are in the queue to ensure the test is deterministic.
	for sema.NumWaits() < len(priorities) {
		time.Sleep(10 * time.Millisecond)
	}

	initialDones[0](nil)
	for i, expected := range expectedOrder {
		p := <-order
		if p != expected {
			t.Fatalf("after release %d, got priority %d; want %d", i+1, p, expected)
		}
	}
	for _, done := range initialDones[1:] {
		done(nil)
	}

	wg.Wait()
	close(order)

	// Check final semaphore state.
	if sema.NumServs() != 0 {
		t.Errorf("NumServs() = %d; want 0", sema.NumServs())
	}
	if sema.NumWaits() != 0 {
		t.Errorf("NumWaits() = %d; want 0", sema.NumWaits())
	}
	if sema.NumRequests() != capacity+len(priorities) {
		t.Errorf("NumRequests() = %d; want %d", sema.NumRequests(), capacity+len(priorities))
	}
}
