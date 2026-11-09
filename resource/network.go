// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package resource provides an adaptive flush-admission gate. Network
// caps concurrent hashfs flushes; a Gradient2 controller (controller.go)
// resizes the cap once per window from the p50 time to first byte of
// download RPCs, growing while latency stays near a long-window
// baseline and shrinking when it inflates.
package resource

import (
	"container/list"
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/status"

	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/o11y/trace"
	"go.chromium.org/build/siso/sync/semaphore"
)

// Network is an adaptive admission gate. It is driven by an external
// stats.Handler (see NewStatsHandler) that pushes per-RPC TTFB
// observations in via observeTTFB.
type Network struct {
	name         string
	initial      int
	floor        int
	ceiling      int
	window       time.Duration
	waitSpanName string
	servSpanName string

	cap      atomic.Int64
	inFlight atomic.Int64

	// mu guards waiters, the FIFO of parked acquirers. Each enqueues a
	// personal channel; a release closes the front one (waking one), and
	// a cap grow closes as many as the cap grew by.
	mu      sync.Mutex
	waiters list.List

	reqs        atomic.Int64
	waits       atomic.Int64
	maxInFlight atomic.Int64

	// statMu guards stat, the acquire-wait/serve accumulator exposed via
	// Stat (see Stat for why it has the semaphore.Stat shape).
	statMu sync.Mutex
	stat   semaphore.Stat

	// windowTTFBHist accumulates per-RPC TTFB; drained once per tick.
	windowTTFBHist   windowHist
	windowInFlightPk atomic.Int64

	ctrlMu sync.Mutex
	ctrl   controller

	traceWriter atomic.Pointer[writerHolder]

	// testHookBeforePark, when set, runs after the cap check and before
	// an acquirer parks. nil in production; tests use it to drive the
	// lost-wakeup window (a release racing the check->park transition).
	testHookBeforePark func()

	start  time.Time
	stopCh chan struct{}
	doneCh chan struct{}
}

type writerHolder struct {
	w io.Writer
}

// atomicMax raises a to v when v is larger, retrying on contention.
func atomicMax(a *atomic.Int64, v int64) {
	for {
		cur := a.Load()
		if v <= cur || a.CompareAndSwap(cur, v) {
			return
		}
	}
}

// NewNetwork constructs a Network gate starting at initial and starts
// its controller goroutine. initial is clamped to [floor, ceiling].
func NewNetwork(name string, initial, floor, ceiling int) *Network {
	g := newNetworkNoStart(name, initial, floor, ceiling)
	g.stopCh = make(chan struct{})
	g.doneCh = make(chan struct{})
	go g.run()
	return g
}

func newNetworkNoStart(name string, initial, floor, ceiling int) *Network {
	floor = max(floor, 1)
	ceiling = max(ceiling, floor)
	initial = min(max(initial, floor), ceiling)
	g := &Network{
		name:         fmt.Sprintf("%s/%d..%d", name, floor, ceiling),
		initial:      initial,
		floor:        floor,
		ceiling:      ceiling,
		window:       defaultWindow,
		waitSpanName: fmt.Sprintf("wait:%s", name),
		servSpanName: fmt.Sprintf("serv:%s", name),
		start:        time.Now(),
	}
	g.cap.Store(int64(initial))
	g.ctrl = newController(floor, ceiling)
	return g
}

// Close stops the controller goroutine. Acquire still works after
// Close; the cap just stops adjusting.
func (g *Network) Close() {
	if g.stopCh == nil {
		return
	}
	select {
	case <-g.stopCh:
		return
	default:
	}
	close(g.stopCh)
	<-g.doneCh
}

// Acquire blocks until a slot is free, returning a release that must be
// called when the admitted flush completes.
func (g *Network) Acquire(ctx context.Context) (context.Context, func(error), error) {
	_, waitSpan := trace.NewSpan(ctx, g.waitSpanName)
	g.waits.Add(1)
	defer waitSpan.Close(nil)
	defer g.waits.Add(-1)

	start := time.Now()

	for {
		inf := g.inFlight.Load()
		curCap := g.cap.Load()
		if inf < curCap {
			if !g.inFlight.CompareAndSwap(inf, inf+1) {
				continue
			}
			g.reqs.Add(1)
			newInFlight := inf + 1
			atomicMax(&g.maxInFlight, newInFlight)
			atomicMax(&g.windowInFlightPk, newInFlight)
			servCtx, servSpan := trace.NewSpan(ctx, g.servSpanName)
			waitDur := time.Since(start)
			if waitDur > 1*time.Second {
				clog.Infof(ctx, "wait %s for %s", g.name, waitDur)
			}
			grantedAt := time.Now()
			return servCtx, func(err error) {
				st, ok := status.FromError(err)
				if !ok {
					st = status.FromContextError(err)
				}
				servSpan.Close(st.Proto())
				g.statMu.Lock()
				g.stat.Update(waitDur, time.Since(grantedAt), err != nil)
				g.statMu.Unlock()
				g.release()
			}, nil
		}

		if g.testHookBeforePark != nil {
			g.testHookBeforePark()
		}

		// Park in FIFO order. Enqueue under mu and re-check the cap while
		// holding it: a release can only wake us by taking mu, so it
		// either finds us queued, or has already freed a slot that this
		// re-check observes. Either way no wakeup is lost.
		ready := make(chan struct{})
		g.mu.Lock()
		if g.inFlight.Load() < g.cap.Load() {
			g.mu.Unlock()
			continue
		}
		elem := g.waiters.PushBack(ready)
		g.mu.Unlock()

		select {
		case <-ready:
			// Woken: loop and try to acquire.
		case <-ctx.Done():
			g.cancelWait(elem, ready)
			return ctx, func(error) {}, context.Cause(ctx)
		}
	}
}

func (g *Network) release() {
	g.inFlight.Add(-1)
	g.wake(1)
}

// wake closes up to n parked acquirers' channels, front first (FIFO).
func (g *Network) wake(n int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for n > 0 {
		front := g.waiters.Front()
		if front == nil {
			return
		}
		g.waiters.Remove(front)
		close(front.Value.(chan struct{}))
		n--
	}
}

// cancelWait removes a parked acquirer that is giving up. But a release
// or grow may have already woken it (closed its channel and handed it a
// free slot); if so, that slot passes to the next waiter rather than
// being stranded.
func (g *Network) cancelWait(elem *list.Element, ready chan struct{}) {
	g.mu.Lock()
	select {
	case <-ready:
		g.mu.Unlock()
		g.wake(1)
	default:
		g.waiters.Remove(elem)
		g.mu.Unlock()
	}
}

// observeTTFB is called by the stats handler for each download RPC with
// the time from Begin to first InPayload.
func (g *Network) observeTTFB(d time.Duration) {
	g.windowTTFBHist.observe(d)
}

// setCap clamps to [floor, ceiling]. A grow wakes blocked acquirers; a
// shrink lets them drain naturally.
func (g *Network) setCap(n int) {
	target := min(max(int64(n), int64(g.floor)), int64(g.ceiling))
	old := g.cap.Swap(target)
	if target > old {
		g.wake(int(target - old))
	}
}

// SetTraceWriter installs an io.Writer that receives one CSV row per
// controller tick. Columns: ms,N,in_flight_peak,ttfb_p50_ms,
// baseline_ms,samples,decision. Caller writes the header; pass nil to
// stop writing.
func (g *Network) SetTraceWriter(w io.Writer) {
	if w == nil {
		g.traceWriter.Store(nil)
		return
	}
	g.traceWriter.Store(&writerHolder{w: w})
}

func (g *Network) Name() string     { return g.name }
func (g *Network) Capacity() int    { return int(g.cap.Load()) }
func (g *Network) NumServs() int    { return int(g.inFlight.Load()) }
func (g *Network) NumWaits() int    { return int(g.waits.Load()) }
func (g *Network) NumRequests() int { return int(g.reqs.Load()) }

// MaxInFlight returns the peak admitted in-flight over the gate's life.
func (g *Network) MaxInFlight() int64 { return g.maxInFlight.Load() }

// Stat returns the acquire wait / serve duration accumulator in the
// shape the resource-usage table consumes, so the adaptive gate reports
// there alongside the static semaphores.
func (g *Network) Stat() semaphore.Stat {
	g.statMu.Lock()
	defer g.statMu.Unlock()
	s := g.stat
	s.Name = g.name
	return s
}

func (g *Network) run() {
	defer close(g.doneCh)
	ticker := time.NewTicker(g.window)
	defer ticker.Stop()
	for {
		select {
		case <-g.stopCh:
			return
		case <-ticker.C:
			g.tick()
		}
	}
}

func (g *Network) tick() {
	samples, p50 := g.windowTTFBHist.snapshotAndReset()
	peak := g.windowInFlightPk.Swap(0)
	if cur := g.inFlight.Load(); cur > peak {
		peak = cur
	}
	curCap := int(g.cap.Load())

	g.ctrlMu.Lock()
	newCap, decision := g.ctrl.advance(p50, samples, peak, curCap)
	baseline := g.ctrl.baseline()
	g.ctrlMu.Unlock()

	if newCap != curCap {
		g.setCap(newCap)
	}

	if wh := g.traceWriter.Load(); wh != nil {
		ms := time.Since(g.start).Milliseconds()
		fmt.Fprintf(wh.w, "%d,%d,%d,%.1f,%.1f,%d,%s\n",
			ms, newCap, peak,
			float64(p50)/float64(time.Millisecond),
			float64(baseline)/float64(time.Millisecond),
			samples, decision)
	}
}

// Dump writes a human-readable summary.
func (g *Network) Dump(w io.Writer) {
	g.ctrlMu.Lock()
	c := g.ctrl
	g.ctrlMu.Unlock()

	fmt.Fprintf(w, "\nresource.Network %q (Gradient2):\n", g.name)
	fmt.Fprintf(w, "  final N=%d estimated=%d initial=%d floor=%d ceiling=%d\n",
		g.Capacity(), int(c.estimatedLimit), g.initial, g.floor, g.ceiling)
	fmt.Fprintf(w, "  windows: loaded=%d idle=%d total=%d\n",
		c.loadedWindows, c.idleWindows, c.loadedWindows+c.idleWindows)
	fmt.Fprintf(w, "  updates: total=%d app_limited=%d grows=%d shrinks=%d holds=%d\n",
		c.loadedWindows, c.appLimited, c.grows, c.shrinks, c.holds)
	fmt.Fprintf(w, "  peak in-flight: %d\n", g.maxInFlight.Load())
	fmt.Fprintf(w, "  longRtt EWMA: %s\n", c.baseline().Round(time.Millisecond))
	fmt.Fprintf(w, "  last sample RTT: %s\n", c.lastSampleRtt.Round(time.Millisecond))
}
