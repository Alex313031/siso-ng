// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package monitoring

import (
	"context"
	"net"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/grpc/stats"
)

// makeAddr returns a net.Addr for LocalAddr in synthetic events.
func makeAddr(addr string) net.Addr {
	a, _ := net.ResolveTCPAddr("tcp", addr)
	return a
}

func TestBytestreamHandler_ActiveStreamsPerConn(t *testing.T) {
	h := &bytestreamStatsHandler{}

	// Start 3 RPCs on conn A and 1 on conn B. Active counts: A=3, B=1.
	rpcs := []struct{ addr string }{
		{"127.0.0.1:1001"}, {"127.0.0.1:1001"}, {"127.0.0.1:1001"},
		{"127.0.0.1:2002"},
	}
	ctxs := make([]context.Context, len(rpcs))
	for i, r := range rpcs {
		ctxs[i] = h.TagRPC(t.Context(), &stats.RPCTagInfo{})
		h.HandleRPC(ctxs[i], &stats.Begin{})
		h.HandleRPC(ctxs[i], &stats.OutHeader{LocalAddr: makeAddr(r.addr)})
	}

	if got := h.MaxActiveStreams(); got != 3 {
		t.Errorf("MaxActiveStreams() = %d, want 3 (3 on A, 1 on B)", got)
	}

	// End the first RPC on A. Now A=2, B=1, max=2.
	h.HandleRPC(ctxs[0], &stats.End{})
	if got := h.MaxActiveStreams(); got != 2 {
		t.Errorf("after one End, MaxActiveStreams() = %d, want 2", got)
	}

	// End the rest.
	for i := 1; i < len(ctxs); i++ {
		h.HandleRPC(ctxs[i], &stats.End{})
	}
	if got := h.MaxActiveStreams(); got != 0 {
		t.Errorf("after all Ends, MaxActiveStreams() = %d, want 0", got)
	}
}

// Peak captures the high-water mark between calls, even if the value
// has dropped by the time PeakActiveStreamsSinceReset is read. Solves
// the under-sampling problem where the OTel scrape happens to land
// between saturation spikes.
func TestBytestreamHandler_PeakSinceReset(t *testing.T) {
	h := &bytestreamStatsHandler{}
	addr := makeAddr("127.0.0.1:1001")

	// Open 4 streams.
	ctxs := make([]context.Context, 4)
	for i := range ctxs {
		ctxs[i] = h.TagRPC(t.Context(), &stats.RPCTagInfo{})
		h.HandleRPC(ctxs[i], &stats.Begin{})
		h.HandleRPC(ctxs[i], &stats.OutHeader{LocalAddr: addr})
	}
	// Close 3 of them BEFORE the gauge reads peak. The instantaneous
	// count is 1, but the peak should be 4.
	for i := range 3 {
		h.HandleRPC(ctxs[i], &stats.End{})
	}

	if got := h.MaxActiveStreams(); got != 1 {
		t.Errorf("MaxActiveStreams = %d, want 1 (instant)", got)
	}
	if got := h.PeakActiveStreamsSinceReset(); got != 4 {
		t.Errorf("PeakActiveStreamsSinceReset = %d, want 4 (high-water mark)", got)
	}
	// Reset semantics: after a read, the peak follows the current
	// active count until the next stream-start raises it.
	if got := h.PeakActiveStreamsSinceReset(); got != 1 {
		t.Errorf("after-reset PeakActiveStreamsSinceReset = %d, want 1 (current active)", got)
	}

	// Drain.
	h.HandleRPC(ctxs[3], &stats.End{})
}

// Stage-timing path must be panic-safe when instruments are nil.
func TestBytestreamHandler_StageTimingsRecorded(t *testing.T) {
	h := &bytestreamStatsHandler{}
	ctx := h.TagRPC(t.Context(), &stats.RPCTagInfo{FullMethodName: bytestreamReadMethod})
	h.HandleRPC(ctx, &stats.Begin{})
	h.HandleRPC(ctx, &stats.OutHeader{LocalAddr: makeAddr("127.0.0.1:1001")})
	h.HandleRPC(ctx, &stats.InHeader{})
	h.HandleRPC(ctx, &stats.InPayload{})
	h.HandleRPC(ctx, &stats.End{})
	if got := h.MaxActiveStreams(); got != 0 {
		t.Errorf("after End, active = %d, want 0", got)
	}
}

// Stages fire only for ByteStream.Read; active streams cover every RPC.
func TestBytestreamHandler_NonReadRPCFiltered(t *testing.T) {
	h := &bytestreamStatsHandler{}
	ctx := h.TagRPC(t.Context(), &stats.RPCTagInfo{
		FullMethodName: "/build.bazel.remote.execution.v2.ContentAddressableStorage/FindMissingBlobs",
	})
	h.HandleRPC(ctx, &stats.Begin{})
	h.HandleRPC(ctx, &stats.OutHeader{LocalAddr: makeAddr("127.0.0.1:1001")})
	if got := h.MaxActiveStreams(); got != 1 {
		t.Errorf("active = %d, want 1; active-stream tracking is method-agnostic", got)
	}
	h.HandleRPC(ctx, &stats.InHeader{})
	h.HandleRPC(ctx, &stats.InPayload{})
	h.HandleRPC(ctx, &stats.End{})
	if got := h.MaxActiveStreams(); got != 0 {
		t.Errorf("after End, active = %d, want 0", got)
	}
}

// HandleRPC on a ctx without TagRPC state must not panic.
func TestBytestreamHandler_NoCtxState(t *testing.T) {
	h := &bytestreamStatsHandler{}
	h.HandleRPC(t.Context(), &stats.Begin{})
	h.HandleRPC(t.Context(), &stats.End{})
}

// Many goroutines on a few conns; active counts must drain to zero.
func TestBytestreamHandler_Concurrent(t *testing.T) {
	h := &bytestreamStatsHandler{}
	const goroutines = 100
	const opsPer = 100
	addrs := []string{"127.0.0.1:1001", "127.0.0.1:2002", "127.0.0.1:3003"}
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := range goroutines {
		go func(g int) {
			defer wg.Done()
			for i := range opsPer {
				addr := addrs[(g+i)%len(addrs)]
				ctx := h.TagRPC(t.Context(), &stats.RPCTagInfo{})
				h.HandleRPC(ctx, &stats.Begin{})
				h.HandleRPC(ctx, &stats.OutHeader{LocalAddr: makeAddr(addr)})
				h.HandleRPC(ctx, &stats.End{})
			}
		}(g)
	}
	wg.Wait()
	if got := h.MaxActiveStreams(); got != 0 {
		t.Errorf("after concurrent run, MaxActiveStreams() = %d, want 0", got)
	}
}

// transport_pick splits the begin..OutHeader gap. A delayed pick advances
// queueStart to the moment the pick completes, so client_queue is measured
// from there instead of double counting the pick wait, and the wait lands in
// transportPick. With no delayed pick, queueStart stays at begin and
// transportPick is zero. synctest supplies a deterministic fake clock.
func TestBytestreamHandler_TransportPickSplit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := &bytestreamStatsHandler{}

		// No delayed pick: queueStart tracks begin, transportPick stays 0.
		ctx := h.TagRPC(t.Context(), &stats.RPCTagInfo{FullMethodName: bytestreamReadMethod})
		s := ctx.Value(rpcStateKey{}).(*rpcState)
		h.HandleRPC(ctx, &stats.Begin{})
		begin := s.begin.Load()
		if got := s.queueStart.Load(); got != begin {
			t.Errorf("queueStart = %d, want begin %d at Begin", got, begin)
		}
		h.HandleRPC(ctx, &stats.OutHeader{LocalAddr: makeAddr("127.0.0.1:1001")})
		if got := s.transportPick.Load(); got != 0 {
			t.Errorf("transportPick = %d, want 0 without a delayed pick", got)
		}
		if got := s.queueStart.Load(); got != begin {
			t.Errorf("queueStart = %d, want begin %d without a delayed pick", got, begin)
		}

		// Delayed pick: transportPick captures the wait and queueStart
		// resets to the pick completion time so client_queue starts there.
		ctx2 := h.TagRPC(t.Context(), &stats.RPCTagInfo{FullMethodName: bytestreamReadMethod})
		s2 := ctx2.Value(rpcStateKey{}).(*rpcState)
		h.HandleRPC(ctx2, &stats.Begin{})
		begin2 := s2.begin.Load()
		const pickWait = 7 * time.Millisecond
		time.Sleep(pickWait)
		h.HandleRPC(ctx2, &stats.DelayedPickComplete{})
		if got := s2.transportPick.Load(); got != pickWait.Nanoseconds() {
			t.Errorf("transportPick = %d ns, want %d ns", got, pickWait.Nanoseconds())
		}
		if got, want := s2.queueStart.Load(), begin2+pickWait.Nanoseconds(); got != want {
			t.Errorf("queueStart = %d, want begin+pickWait %d", got, want)
		}
	})
}
