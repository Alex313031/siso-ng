// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package resource

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/stats"
)

// feedRPC drives one RPC's Begin + InPayload events through the handler.
func feedRPC(h *StatsHandler, method string, begin time.Time, payloads ...time.Time) {
	ctx := h.TagRPC(context.Background(), &stats.RPCTagInfo{FullMethodName: method})
	if !begin.IsZero() {
		h.HandleRPC(ctx, &stats.Begin{BeginTime: begin})
	}
	for _, recv := range payloads {
		h.HandleRPC(ctx, &stats.InPayload{RecvTime: recv})
	}
}

func TestStatsHandler_IncludedMethodObservesTTFB(t *testing.T) {
	g := newNetworkNoStart("test", 16, 4, 16)
	t0 := time.Unix(0, 0)
	feedRPC(NewStatsHandler(g), "/google.bytestream.ByteStream/Read", t0, t0.Add(50*time.Millisecond))

	count, p50 := g.windowTTFBHist.snapshotAndReset()
	if count != 1 {
		t.Fatalf("ttfb count: got %d want 1", count)
	}
	// 50ms p50: lower edge of bucket [50ms,75ms).
	if p50 < 50*time.Millisecond || p50 > 75*time.Millisecond {
		t.Errorf("ttfb p50: got %v want ~50ms", p50)
	}
}

func TestStatsHandler_ExcludedMethodIgnored(t *testing.T) {
	g := newNetworkNoStart("test", 16, 4, 16)
	t0 := time.Unix(0, 0)
	feedRPC(NewStatsHandler(g),
		"/build.bazel.remote.execution.v2.ContentAddressableStorage/FindMissingBlobs",
		t0, t0.Add(50*time.Millisecond))

	if count, _ := g.windowTTFBHist.snapshotAndReset(); count != 0 {
		t.Errorf("control-plane method produced %d TTFB samples, want 0", count)
	}
}

func TestStatsHandler_FirstPayloadOnly(t *testing.T) {
	g := newNetworkNoStart("test", 16, 4, 16)
	t0 := time.Unix(0, 0)
	// Three payloads; only the first (at +50ms) is the time to first byte.
	feedRPC(NewStatsHandler(g), "/google.bytestream.ByteStream/Read", t0,
		t0.Add(50*time.Millisecond), t0.Add(200*time.Millisecond), t0.Add(400*time.Millisecond))

	count, p50 := g.windowTTFBHist.snapshotAndReset()
	if count != 1 {
		t.Fatalf("ttfb count: got %d want 1 (only the first payload)", count)
	}
	if p50 > 75*time.Millisecond {
		t.Errorf("ttfb p50: got %v; later payloads must not count", p50)
	}
}

func TestStatsHandler_MissingBeginSkipped(t *testing.T) {
	g := newNetworkNoStart("test", 16, 4, 16)
	// InPayload with no preceding Begin: no baseline, so no sample.
	feedRPC(NewStatsHandler(g), "/google.bytestream.ByteStream/Read",
		time.Time{}, time.Unix(0, 0).Add(50*time.Millisecond))

	if count, _ := g.windowTTFBHist.snapshotAndReset(); count != 0 {
		t.Errorf("InPayload without Begin produced %d samples, want 0", count)
	}
}
