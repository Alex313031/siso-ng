// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package resource

import (
	"sync/atomic"
	"time"
)

// windowHist is a fixed-bucket histogram of RPC durations observed
// during a single window. Observations are lock-free; snapshotAndReset
// drains the counts and returns the approximate p50 and total sample
// count. Designed to be drained once per controller tick.
//
// Buckets give ~50-100ms precision in the 100-500ms range where the
// TTFB median typically lives on Chrome cache-warm.
type windowHist struct {
	counts [len(windowHistBucketsNs) + 1]atomic.Int64
}

var windowHistBucketsNs = [...]int64{
	int64(5 * time.Millisecond),
	int64(10 * time.Millisecond),
	int64(20 * time.Millisecond),
	int64(50 * time.Millisecond),
	int64(75 * time.Millisecond),
	int64(100 * time.Millisecond),
	int64(150 * time.Millisecond),
	int64(200 * time.Millisecond),
	int64(300 * time.Millisecond),
	int64(400 * time.Millisecond),
	int64(500 * time.Millisecond),
	int64(700 * time.Millisecond),
	int64(1 * time.Second),
	int64(2 * time.Second),
	int64(5 * time.Second),
	int64(10 * time.Second),
}

// observe adds one sample.
func (h *windowHist) observe(d time.Duration) {
	if d <= 0 {
		return
	}
	ns := d.Nanoseconds()
	for i, edge := range windowHistBucketsNs {
		if ns < edge {
			h.counts[i].Add(1)
			return
		}
	}
	h.counts[len(windowHistBucketsNs)].Add(1)
}

// snapshotAndReset atomically drains the counters and returns the
// number of samples and the approximate p50, interpolated within its
// bucket.
func (h *windowHist) snapshotAndReset() (total int64, p50 time.Duration) {
	snap := make([]int64, len(h.counts))
	for i := range h.counts {
		snap[i] = h.counts[i].Swap(0)
		total += snap[i]
	}
	if total == 0 {
		return 0, 0
	}
	return total, percentile(snap, (total+1)/2)
}

// percentile finds the 1-indexed target-th sample in a bucket histogram
// and returns it interpolated within the containing bucket.
func percentile(snap []int64, target int64) time.Duration {
	var cum int64
	for i, c := range snap {
		if c == 0 {
			continue
		}
		prevCum := cum
		cum += c
		if cum < target {
			continue
		}
		var lo, hi int64
		if i == 0 {
			lo = 0
		} else {
			lo = windowHistBucketsNs[i-1]
		}
		if i < len(windowHistBucketsNs) {
			hi = windowHistBucketsNs[i]
		} else {
			hi = windowHistBucketsNs[len(windowHistBucketsNs)-1] * 2
		}
		frac := float64(target-prevCum-1) / float64(c)
		if frac < 0 {
			frac = 0
		}
		return time.Duration(lo + int64(frac*float64(hi-lo)))
	}
	return 0
}
