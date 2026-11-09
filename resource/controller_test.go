// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package resource

import (
	"testing"
	"time"
)

func newCtrl(initial, floor, ceiling int) *controller {
	c := newController(floor, ceiling)
	c.estimatedLimit = float64(initial)
	return &c
}

// feed seeds longRtt with a stable baseline (past the EWMA warmup) so
// tests can exercise grow/shrink.
func feed(c *controller, rtt time.Duration) {
	for range longRttWarmup + 5 {
		c.longRtt.add(float64(rtt))
	}
}

// step runs one tick with enough samples to clear the per-window floor.
func step(c *controller, rtt time.Duration, samples, peak int64, curCap int) (int, string) {
	return c.advance(rtt, samples, peak, curCap)
}

func TestEWMA_WarmupIsPlainAverage(t *testing.T) {
	e := newEWMA(600, 5)
	for _, v := range []float64{100, 200, 300, 400, 500} {
		e.add(v)
	}
	// After 5 warmup samples: simple mean = 300.
	if e.value != 300 {
		t.Fatalf("ewma warmup mean: got %v want 300", e.value)
	}
}

func TestEWMA_PostWarmupUsesFactor(t *testing.T) {
	e := newEWMA(100, 2) // small window for quick convergence in test
	e.add(100)
	e.add(100)
	// EWMA now = 100. Factor = 2/(100+1) ~= 0.0198.
	// Add a 200 sample: value = 100*(1-0.0198) + 200*0.0198 ~= 101.98.
	e.add(200)
	if e.value < 101 || e.value > 103 {
		t.Fatalf("ewma post-warmup: got %v want ~102", e.value)
	}
}

func TestGradient2_InitialTickSeedsFromCurCap(t *testing.T) {
	c := newController(16, 4096)
	// estimatedLimit is 0 until the first tick seeds it from curCap.
	cc := &c
	step(cc, 150*time.Millisecond, 100, 128, 128)
	if cc.estimatedLimit == 0 {
		t.Fatal("estimatedLimit not seeded on first tick")
	}
}

func TestGradient2_SkipUnloadedBelowSampleFloor(t *testing.T) {
	c := newCtrl(128, 16, 4096)
	newCap, decision := step(c, 150*time.Millisecond, 5, 128, 128)
	if decision != "skip_unloaded" {
		t.Fatalf("decision: got %q want skip_unloaded", decision)
	}
	if newCap != 128 {
		t.Fatalf("cap should not change on skip: got %d", newCap)
	}
}

func TestGradient2_AppLimitedHolds(t *testing.T) {
	c := newCtrl(128, 16, 4096)
	feed(c, 150*time.Millisecond)
	// peak=10 (below limit/2 = 64) → app_limited.
	newCap, decision := step(c, 150*time.Millisecond, 100, 10, 128)
	if decision != "app_limited" {
		t.Fatalf("decision: got %q want app_limited", decision)
	}
	if newCap != 128 {
		t.Fatalf("cap should not change when app-limited: got %d", newCap)
	}
}

func TestGradient2_HealthyTickGrows(t *testing.T) {
	c := newCtrl(128, 16, 4096)
	feed(c, 150*time.Millisecond)
	// sample == longRtt: gradient = clamp(tolerance, 0.5, 1.0) = 1.0.
	// newLimit = 128*1.0 + queueSize(32), smoothed at 0.5 → grows.
	cap := 128
	for range 20 {
		cap, _ = step(c, 150*time.Millisecond, 100, int64(cap), cap)
	}
	if cap <= 128 {
		t.Fatalf("cap did not grow across 20 healthy ticks: got %d", cap)
	}
}

func TestGradient2_InflatedSampleShrinks(t *testing.T) {
	c := newCtrl(128, 16, 4096)
	feed(c, 100*time.Millisecond)
	// sample = 300ms (~3x longRtt) drives the gradient to ~0.5, so the
	// smoothed newLimit drops below the current cap.
	newCap, decision := step(c, 300*time.Millisecond, 100, 128, 128)
	if decision != "shrink" {
		t.Fatalf("decision: got %q want shrink", decision)
	}
	if newCap >= 128 {
		t.Fatalf("cap did not shrink: got %d", newCap)
	}
}

func TestGradient2_GradientClampedAt0_5(t *testing.T) {
	c := newCtrl(128, 16, 4096)
	feed(c, 50*time.Millisecond)
	// sample = 1000ms (20x longRtt). Raw gradient ~0.075 clamps to 0.5,
	// so one tick sheds only ~12% (128 -> ~112) instead of collapsing
	// toward the floor.
	newCap, _ := step(c, 1*time.Second, 100, 128, 128)
	if newCap < 90 {
		t.Fatalf("one-tick shrink not bounded by the 0.5 gradient floor: got %d", newCap)
	}
}

func TestGradient2_RecoveryDecayTriggers(t *testing.T) {
	c := newCtrl(128, 16, 4096)
	feed(c, 500*time.Millisecond) // longRtt starts high
	before := c.longRtt.value
	// Fast sample: longRtt/sample = 500/100 = 5 > 2, trigger decay.
	step(c, 100*time.Millisecond, 100, 128, 128)
	if after := c.longRtt.value; after >= before {
		t.Fatalf("longRtt did not decay on recovery: before=%v after=%v", before, after)
	}
}

func TestGradient2_CapClampsAtFloor(t *testing.T) {
	c := newCtrl(64, 32, 4096)
	feed(c, 50*time.Millisecond)
	cap := 64
	// Hammer with 500ms (10x longRtt) until cap bottoms out.
	for range 50 {
		cap, _ = step(c, 500*time.Millisecond, 100, int64(cap), cap)
	}
	if cap < 32 {
		t.Fatalf("cap went below floor: got %d want >= 32", cap)
	}
}

func TestGradient2_CapClampsAtCeiling(t *testing.T) {
	c := newCtrl(128, 16, 200)
	feed(c, 150*time.Millisecond)
	cap := 128
	for range 50 {
		cap, _ = step(c, 150*time.Millisecond, 100, int64(cap), cap)
	}
	if cap > 200 {
		t.Fatalf("cap went above ceiling: got %d want <= 200", cap)
	}
}

func TestGradient2_EquilibriumAtTwiceBaseline(t *testing.T) {
	// queueSize=limit/4 puts the operating point at 2x baseline (gradient
	// 0.75): below 2x the cap grows, above it shrinks.
	const base = 100 * time.Millisecond
	const cap = 1000

	below := newCtrl(cap, 16, 1<<20)
	feed(below, base)
	if n, _ := step(below, 18*base/10, 100, cap, cap); n <= cap { // 1.8x baseline
		t.Errorf("at 1.8x baseline (below equilibrium): got %d want > %d (grow)", n, cap)
	}

	above := newCtrl(cap, 16, 1<<20)
	feed(above, base)
	if n, _ := step(above, 22*base/10, 100, cap, cap); n >= cap { // 2.2x baseline
		t.Errorf("at 2.2x baseline (above equilibrium): got %d want < %d (shrink)", n, cap)
	}
}

func TestGradient2_BaselineReturnsLongRtt(t *testing.T) {
	c := newCtrl(128, 16, 4096)
	feed(c, 200*time.Millisecond)
	if diff := c.baseline() - 200*time.Millisecond; diff > time.Millisecond || diff < -time.Millisecond {
		t.Fatalf("baseline: got %v want ~200ms", c.baseline())
	}
}
