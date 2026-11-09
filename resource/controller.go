// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package resource

import "time"

// Gradient2 admission controller, adapted from Netflix's
// concurrency-limits library (Gradient2Limit.java): the same control
// law, re-driven on windowed p50 TTFB samples with build-shaped
// constants (see smoothing and queueSizeFor). It is a production-tested
// alternative to min-RTT-based algorithms for heterogeneous RPC
// workloads, which is exactly our situation (blob sizes span 5 orders
// of magnitude, per-window RTT is bursty).
//
// Each update computes:
//
//	gradient = clamp(tolerance * longRtt / sampleRtt, 0.5, 1.0)
//	newLimit = currentLimit*gradient + queueSize
//	newLimit = currentLimit*(1-smoothing) + newLimit*smoothing
//
//  1. No minRTT, no probing: the long-window EWMA stays fresh without
//     periodic resets.
//  2. Gradient is floored at 0.5, so the loop can never shed more than
//     half its limit in one update.
//  3. queueSize is limit-scaled headroom that keeps the limit climbing
//     past the pure-gradient fixed point while latency stays healthy.

const (
	defaultWindow = 1 * time.Second

	// Long-window EWMA. 600 samples at 1s cadence is 10 minutes of
	// decay memory (Netflix's default). The first 10 samples use a
	// plain average before the EWMA takes over.
	longRttWindow = 600
	longRttWarmup = 10

	// tolerance: how far longRtt/sampleRtt may drift and still count as
	// healthy (gradient = 1.0).
	tolerance = 1.5

	// smoothing: blend of the new limit into the current. Netflix uses
	// 0.2, tuned for a 10-minute EWMA horizon on always-on services.
	// A ~2-minute build wants faster grow/shrink; 0.5 converges ~2.5x
	// faster in both directions while still damping per-window noise.
	smoothing = 0.5

	// recovery: when longRtt/sampleRtt exceeds the trigger ratio we are
	// emerging from a slow period; decay longRtt so the gradient does
	// not stay pinned at 1.0 while the EWMA catches down.
	recoveryDecayFactor  = 0.95
	recoveryTriggerRatio = 2.0

	// minSamplesPerWindow: skip windows with too few RPCs for the
	// per-window p50 to be meaningful.
	minSamplesPerWindow = 10

	// minSampleRtt: treat a sub-threshold p50 as effectively idle
	// (cache hits, no-op paths) and skip the update.
	minSampleRtt = 5 * time.Millisecond
)

// queueSizeFor returns the additive headroom (in RPCs) for the
// Gradient2 update. Netflix's default is a constant 4, sized for
// server-side limits in the hundreds. With smoothing 0.5 baked in, the
// effective growth per healthy tick is 0.5*queueSize, so a constant 4
// converges in minutes at our N (~thousands on a fast link), longer
// than a whole build.
//
// limit/4 gives growth of ~0.125*limit per tick (doubling in ~6 ticks
// at any scale), reaching steady state inside the first ~30s of a
// 2-minute build and removing the ramp tax of starting near the
// historical static default.
//
// Trade-off: the equilibrium gradient (where shrink stops being net
// positive) is 1-1/k for queueSize=limit/k, so limit/4 sets it at 0.75:
// the sample must reach 2.0x baseline before the loop shrinks (vs 1.71x
// at limit/8). Fine on a stationary link; slower to shed on a
// collapsing one.
func queueSizeFor(limit int) int {
	if q := limit / 4; q > 4 {
		return q
	}
	return 4
}

// ewma is Netflix's ExpAvgMeasurement: a plain average during warmup,
// then an EWMA with factor 2/(window+1).
type ewma struct {
	value  float64
	sum    float64
	count  int
	window int
	warmup int
}

func newEWMA(window, warmup int) ewma {
	return ewma{window: window, warmup: warmup}
}

func (e *ewma) add(sample float64) float64 {
	if e.count < e.warmup {
		e.count++
		e.sum += sample
		e.value = e.sum / float64(e.count)
	} else {
		factor := 2.0 / float64(e.window+1)
		e.value = e.value*(1-factor) + sample*factor
	}
	return e.value
}

// decay multiplies the current value, used for the recovery speedup.
func (e *ewma) decay(factor float64) { e.value *= factor }

// controller holds the Gradient2 state plus a few counters read by
// Network.Dump. It is not safe for concurrent use; Network serializes
// advance and Dump under ctrlMu.
type controller struct {
	floor   int
	ceiling int

	// estimatedLimit is the cap the algorithm wants; Network.setCap
	// clamps and publishes it. Stored as float for smoothing precision.
	estimatedLimit float64

	// longRtt is the long-window EWMA of per-window p50 TTFB.
	longRtt ewma

	// Observability counters.
	lastSampleRtt time.Duration
	appLimited    int
	grows         int
	shrinks       int
	holds         int
	loadedWindows int
	idleWindows   int
}

func newController(floor, ceiling int) controller {
	return controller{
		floor:   floor,
		ceiling: ceiling,
		longRtt: newEWMA(longRttWindow, longRttWarmup),
	}
}

// advance runs one control tick. sampleRtt is the window p50 TTFB,
// ttfbSamples the number of TTFB observations in the window, and
// peakInFlight the max admitted in-flight during it. It returns the new
// cap and a decision label for the trace.
func (c *controller) advance(sampleRtt time.Duration, ttfbSamples, peakInFlight int64, curCap int) (int, string) {
	if c.estimatedLimit == 0 {
		c.estimatedLimit = float64(curCap) // seed from the gate's current cap
	}

	if ttfbSamples < minSamplesPerWindow || sampleRtt < minSampleRtt {
		c.idleWindows++
		return curCap, "skip_unloaded"
	}
	c.loadedWindows++
	c.lastSampleRtt = sampleRtt

	short := float64(sampleRtt)
	long := c.longRtt.add(short)

	// Recovery: a fast sample against a high baseline means the loaded
	// period is over; let the EWMA catch down so the gradient doesn't
	// stay stuck at 1.0.
	if long/short > recoveryTriggerRatio {
		c.longRtt.decay(recoveryDecayFactor)
		long = c.longRtt.value
	}

	// App-limited: the gate isn't binding, so the sample carries no
	// meaningful congestion signal. Hold.
	if peakInFlight*2 < int64(c.estimatedLimit) {
		c.appLimited++
		c.holds++
		return curCap, "app_limited"
	}

	gradient := tolerance * long / short
	if gradient > 1.0 {
		gradient = 1.0
	}
	if gradient < 0.5 {
		gradient = 0.5
	}

	newLimit := c.estimatedLimit*gradient + float64(queueSizeFor(int(c.estimatedLimit)))
	newLimit = c.estimatedLimit*(1-smoothing) + newLimit*smoothing
	if newLimit < float64(c.floor) {
		newLimit = float64(c.floor)
	}
	if newLimit > float64(c.ceiling) {
		newLimit = float64(c.ceiling)
	}
	c.estimatedLimit = newLimit

	switch {
	case int(newLimit) > curCap:
		c.grows++
		return int(newLimit), "grow"
	case int(newLimit) < curCap:
		c.shrinks++
		return int(newLimit), "shrink"
	default:
		c.holds++
		return int(newLimit), "hold"
	}
}

// baseline returns the current long-window RTT EWMA, reported by Dump
// and the trace CSV.
func (c *controller) baseline() time.Duration { return time.Duration(c.longRtt.value) }
