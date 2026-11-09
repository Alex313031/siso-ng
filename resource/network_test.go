// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package resource

import (
	"bytes"
	"container/list"
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNetwork_AcquireReleaseBasic(t *testing.T) {
	g := newNetworkNoStart("test", 4, 2, 8)
	ctx := t.Context()

	_, rel1, err := g.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	_, rel2, err := g.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire 2: %v", err)
	}
	if got := g.NumServs(); got != 2 {
		t.Errorf("NumServs: got %d want 2", got)
	}

	rel1(nil)
	rel2(nil)

	if got := g.NumServs(); got != 0 {
		t.Errorf("NumServs after release: got %d want 0", got)
	}
}

func TestNetwork_StatAccumulates(t *testing.T) {
	g := newNetworkNoStart("flush", 4, 2, 8)
	ctx := t.Context()

	_, rel1, err := g.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	rel1(nil)
	_, rel2, err := g.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire 2: %v", err)
	}
	rel2(errors.New("boom"))

	s := g.Stat()
	if s.Name != g.Name() {
		t.Errorf("Stat().Name = %q, want %q (g.Name())", s.Name, g.Name())
	}
	if s.N != 2 {
		t.Errorf("Stat().N = %d, want 2", s.N)
	}
	if s.NErr != 1 {
		t.Errorf("Stat().NErr = %d, want 1 (one release reported an error)", s.NErr)
	}
}

func TestNetwork_AcquireBlocksAtCap(t *testing.T) {
	g := newNetworkNoStart("test", 1, 1, 1)
	ctx := t.Context()

	_, rel, err := g.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	ch := make(chan error, 1)
	go func() {
		_, rel2, err := g.Acquire(ctx)
		if err == nil {
			rel2(nil)
		}
		ch <- err
	}()

	select {
	case <-ch:
		t.Fatal("second Acquire did not block at cap=1")
	case <-time.After(50 * time.Millisecond):
	}

	rel(nil)

	select {
	case err := <-ch:
		if err != nil {
			t.Fatalf("blocked acquire returned error: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("blocked acquire did not wake after release")
	}
}

func TestNetwork_AcquireRespectsCancel(t *testing.T) {
	g := newNetworkNoStart("test", 1, 1, 1)
	ctx, cancel := context.WithCancel(t.Context())

	_, rel, err := g.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer rel(nil)

	done := make(chan error, 1)
	go func() {
		_, _, err := g.Acquire(ctx)
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected cancel error, got nil")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err: got %v want context.Canceled", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("blocked acquire did not return after cancel")
	}
}

func TestNetwork_ObserveTTFB(t *testing.T) {
	g := newNetworkNoStart("test", 16, 4, 16)
	g.observeTTFB(50 * time.Millisecond)
	g.observeTTFB(100 * time.Millisecond)

	count, median := g.windowTTFBHist.snapshotAndReset()
	if count != 2 {
		t.Errorf("ttfb count: got %d want 2", count)
	}
	if median == 0 {
		t.Errorf("median should be nonzero after observations")
	}
}

func TestNetwork_TickGrowsUnderHealthyLoad(t *testing.T) {
	g := newNetworkNoStart("test", 128, 16, 2048)
	// Run enough healthy ticks to accumulate a meaningful grow under the
	// Gradient2 + smoothing + queueSize update rule.
	prev := g.Capacity()
	for range 20 {
		for range 100 {
			g.observeTTFB(100 * time.Millisecond)
		}
		g.windowInFlightPk.Store(int64(g.Capacity()))
		g.tick()
	}
	if g.Capacity() <= prev {
		t.Fatalf("cap did not grow under healthy load: %d -> %d", prev, g.Capacity())
	}
}

func TestNetwork_TickShrinksOnInflation(t *testing.T) {
	g := newNetworkNoStart("test", 128, 16, 2048)
	// Seed the longRtt baseline with 15 samples at 100ms.
	for range 15 {
		g.ctrl.longRtt.add(float64(100 * time.Millisecond))
	}
	prev := g.Capacity()
	// Inflated window: p50 ~= 4x baseline.
	for range 100 {
		g.observeTTFB(400 * time.Millisecond)
	}
	g.windowInFlightPk.Store(int64(g.Capacity()))
	g.tick()
	if g.Capacity() >= prev {
		t.Fatalf("cap did not shrink: prev=%d new=%d", prev, g.Capacity())
	}
}

func TestNetwork_TraceCSVFormat(t *testing.T) {
	g := newNetworkNoStart("flush", 128, 16, 2048)
	var buf bytes.Buffer
	g.SetTraceWriter(&buf)

	for range 100 {
		g.observeTTFB(50 * time.Millisecond)
	}
	g.windowInFlightPk.Store(128)
	g.tick()

	// ms,N,in_flight_peak,ttfb_p50_ms,baseline_ms,samples,decision
	fields := strings.Split(strings.TrimSpace(buf.String()), ",")
	if len(fields) != 7 {
		t.Fatalf("expected 7 fields, got %d: %q", len(fields), buf.String())
	}
	if fields[6] != "grow" {
		t.Errorf("decision: got %q want grow (healthy window at cap)", fields[6])
	}
}

func TestNetwork_ControllerGoroutineStopsOnClose(t *testing.T) {
	g := NewNetwork("test", 2, 2, 4)
	time.Sleep(10 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		g.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not return")
	}
}

func TestNetwork_LiveSmoke(t *testing.T) {
	g := NewNetwork("smoke", 4, 2, 16)
	defer g.Close()

	ctx := t.Context()
	var wg sync.WaitGroup
	var admitted atomic.Int64
	for range 50 {
		wg.Go(func() {
			_, rel, err := g.Acquire(ctx)
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			admitted.Add(1)
			rel(nil)
		})
	}
	wg.Wait()

	if admitted.Load() != 50 {
		t.Errorf("admitted: got %d want 50", admitted.Load())
	}
}

// TestNetwork_GrowWakesBlockedAcquirer verifies a setCap grow wakes an
// acquirer parked at the cap. This is the path the controller relies on
// to admit waiting flushes when it raises the limit; without the
// broadcast on grow, waiters would not move until the next release.
func TestNetwork_GrowWakesBlockedAcquirer(t *testing.T) {
	g := newNetworkNoStart("test", 1, 1, 4)
	ctx := t.Context()

	_, rel, err := g.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer rel(nil)

	woke := make(chan struct{})
	go func() {
		_, rel2, err := g.Acquire(ctx)
		if err == nil {
			rel2(nil)
		}
		close(woke)
	}()

	// Let the second acquirer park at cap=1.
	select {
	case <-woke:
		t.Fatal("second Acquire admitted before grow; cap was 1")
	case <-time.After(50 * time.Millisecond):
	}

	g.setCap(2) // grow by 1 -> wake(1) should release the parked acquirer

	select {
	case <-woke:
	case <-time.After(2 * time.Second):
		t.Fatal("grow to cap=2 did not wake the blocked acquirer")
	}
}

// TestNetwork_NoLostWakeup guards Acquire's enqueue-under-mu re-check.
// Using testHookBeforePark, it freezes a sole waiter between the cap
// check and enqueuing, releases the only slot while it is frozen, then
// lets it proceed. That release wakes an empty queue, so if Acquire
// enqueued without re-checking the cap under mu the waiter would park
// forever. The re-check sees the freed slot and retries instead, so it
// always makes progress.
func TestNetwork_NoLostWakeup(t *testing.T) {
	g := newNetworkNoStart("test", 1, 1, 1)
	ctx := t.Context()

	atPark := make(chan struct{})
	proceed := make(chan struct{})
	var once sync.Once
	g.testHookBeforePark = func() {
		once.Do(func() {
			close(atPark)
			<-proceed
		})
	}

	_, rel, err := g.Acquire(ctx) // hold the only slot
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	acquired := make(chan struct{})
	go func() {
		_, rel2, err := g.Acquire(ctx) // blocks; freezes in the hook
		if err == nil {
			rel2(nil)
		}
		close(acquired)
	}()

	<-atPark       // waiter has passed the cap check, about to enqueue
	rel(nil)       // free the slot now: this release races the enqueue
	close(proceed) // let the waiter continue into the enqueue + select

	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("waiter never woke: a release racing the enqueue was lost")
	}
}

// TestNetwork_ConcurrentChurn hammers the FIFO wake path under a churning
// cap and frequent cancellations: it exercises wake-one (release),
// wake-delta (cap grows), and the woke-then-cancelled handoff in
// cancelWait together. A broken handoff would strand a freed slot and a
// worker would never complete, tripping the timeout; -race catches data
// races on the waiter queue. Every admission must stay within the
// ceiling.
func TestNetwork_ConcurrentChurn(t *testing.T) {
	g := newNetworkNoStart("test", 2, 1, 8)

	const (
		workers = 64
		iters   = 100
		ceiling = 8
	)
	var wg sync.WaitGroup

	stop := make(chan struct{})
	var churn sync.WaitGroup
	churn.Go(func() {
		caps := []int{1, 2, 4, 8, 2, 1}
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			g.setCap(caps[i%len(caps)])
			time.Sleep(50 * time.Microsecond)
		}
	})

	base := t.Context()
	for w := range workers {
		wg.Go(func() {
			for i := range iters {
				ctx := base
				var cancel context.CancelFunc
				if (w+i)%4 == 0 { // a quarter race a cancel against admission
					ctx, cancel = context.WithTimeout(base, 200*time.Microsecond)
				}
				_, rel, err := g.Acquire(ctx)
				if cancel != nil {
					cancel()
				}
				if err != nil {
					continue // cancelled before admission
				}
				rel(nil)
			}
		})
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("workers did not all finish; possible stranded slot / deadlock")
	}
	close(stop)
	churn.Wait()

	if got := g.MaxInFlight(); got > ceiling {
		t.Errorf("MaxInFlight %d exceeded ceiling %d", got, ceiling)
	}
}

// TestNetwork_CancelWaitForwardsSlotWhenWoken covers cancelWait's
// already-woken branch: a waiter whose channel was closed (a slot handed
// to it) but is now cancelling must pass that slot to the next waiter.
// (AcquireRespectsCancel covers the not-yet-woken branch.)
func TestNetwork_CancelWaitForwardsSlotWhenWoken(t *testing.T) {
	g := newNetworkNoStart("test", 1, 1, 1)

	next := make(chan struct{})
	g.waiters.PushBack(next) // the next queued waiter

	woken := make(chan struct{})
	close(woken) // this waiter's channel: already closed by a wake
	g.cancelWait(&list.Element{}, woken)

	select {
	case <-next:
		// forwarded -- correct.
	default:
		t.Fatal("cancelWait did not forward the freed slot to the next waiter")
	}
}
