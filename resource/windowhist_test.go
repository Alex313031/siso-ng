// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package resource

import (
	"testing"
	"time"
)

func TestWindowHist_EmptyReturnsZero(t *testing.T) {
	var h windowHist
	n, m := h.snapshotAndReset()
	if n != 0 || m != 0 {
		t.Fatalf("empty hist: got (%d, %v) want (0, 0)", n, m)
	}
}

func TestWindowHist_SingleSampleReturnsInBucket(t *testing.T) {
	var h windowHist
	h.observe(123 * time.Millisecond)
	n, m := h.snapshotAndReset()
	if n != 1 {
		t.Fatalf("n: got %d want 1", n)
	}
	// 123ms lands in bucket [100ms, 150ms). Median at sample 1 of 1 with
	// frac=0 returns the lower edge: 100ms.
	if m != 100*time.Millisecond {
		t.Fatalf("median: got %v want 100ms (lower edge of bucket)", m)
	}
}

func TestWindowHist_InterpolatesWithinBucket(t *testing.T) {
	var h windowHist
	// 1000 samples all in [200ms, 300ms). Median interpolated to ~middle.
	for range 1000 {
		h.observe(250 * time.Millisecond)
	}
	n, m := h.snapshotAndReset()
	if n != 1000 {
		t.Fatalf("n: got %d want 1000", n)
	}
	// Target sample 500 of 1000 in the bucket: frac ~= 0.5, median ~= 250ms.
	if m < 240*time.Millisecond || m > 260*time.Millisecond {
		t.Fatalf("median: got %v want ~250ms", m)
	}
}

func TestWindowHist_MedianCrossesBuckets(t *testing.T) {
	var h windowHist
	// 100 samples at 50ms, 100 samples at 250ms. Median between buckets.
	for range 100 {
		h.observe(50 * time.Millisecond)
	}
	for range 100 {
		h.observe(250 * time.Millisecond)
	}
	n, m := h.snapshotAndReset()
	if n != 200 {
		t.Fatalf("n: got %d want 200", n)
	}
	// 50ms lands in [50,75); 100 samples there is the first half.
	// Median at sample 100 of 200 lies at the top of the 50-75 bucket.
	if m < 50*time.Millisecond || m > 75*time.Millisecond {
		t.Fatalf("median: got %v want in [50ms, 75ms]", m)
	}
}

func TestWindowHist_ResetsCounts(t *testing.T) {
	var h windowHist
	h.observe(100 * time.Millisecond)
	h.snapshotAndReset()
	n, _ := h.snapshotAndReset()
	if n != 0 {
		t.Fatalf("second snapshot not zero: got %d", n)
	}
}

func TestWindowHist_SamplesBeyondTopBucketCap(t *testing.T) {
	var h windowHist
	h.observe(60 * time.Second) // beyond 10s top edge
	n, m := h.snapshotAndReset()
	if n != 1 {
		t.Fatalf("n: got %d want 1", n)
	}
	// Overflow bucket: capped at 2x the top edge (20s).
	if m > 20*time.Second {
		t.Fatalf("median: got %v; overflow bucket should cap at 20s", m)
	}
	if m < 10*time.Second {
		t.Fatalf("median: got %v; overflow should be at or above top edge 10s", m)
	}
}

func TestWindowHist_IgnoresNonPositive(t *testing.T) {
	var h windowHist
	h.observe(0)
	h.observe(-1 * time.Millisecond)
	n, _ := h.snapshotAndReset()
	if n != 0 {
		t.Fatalf("non-positive observations counted: %d", n)
	}
}
