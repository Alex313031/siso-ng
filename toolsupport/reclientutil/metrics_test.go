// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package reclientutil

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"go.chromium.org/build/siso/build"
)

func TestCompletionStatus(t *testing.T) {
	stats := build.Stats{
		Done:             100,
		CacheHit:         50,
		CacheHitEarly:    30,
		CacheHitLate:     20,
		RacingRemote:     25,
		RacingLocal:      15,
		Remote:           10,
		LocalFallback:    2,
		Local:            38,
		Fail:             1,
		TwoPhaseCacheHit: 10,
	}

	stat := completionStatus(stats)
	if stat.Name != "CompletionStatus" {
		t.Errorf("stat.Name = %q; want CompletionStatus", stat.Name)
	}

	counts := make(map[string]int64)
	for _, v := range stat.CountsByValue {
		counts[v.Name] = v.Count
	}

	expected := map[string]int64{
		"STATUS_CACHE_HIT":           50,
		"STATUS_TWO_PHASE_CACHE_HIT": 10,
		"STATUS_CACHE_HIT_EARLY":     30,
		"STATUS_CACHE_HIT_LATE":      20,
		"STATUS_RACING_REMOTE":       25,
		"STATUS_RACING_LOCAL":        15,
		"STATUS_REMOTE_EXECUTION":    10,
		"STATUS_LOCAL_FALLBACK":      2,
		"STATUS_LOCAL_EXECUTION":     38,
		"STATUS_NON_ZERO_EXIT":       1,
	}

	if diff := cmp.Diff(expected, counts); diff != "" {
		t.Errorf("completionStatus mismatch (-want +got):\n%s", diff)
	}
}

func TestRBEBuildMetrics(t *testing.T) {
	stats := build.Stats{
		Total:            100,
		Done:             100,
		Skipped:          10,
		CacheHit:         45,
		CacheHitEarly:    30,
		CacheHitLate:     15,
		RacingRemote:     20,
		RacingLocal:      15,
		LocalFallback:    2,
		TwoPhaseCacheHit: 10,
		Remote:           5,
		Local:            40,
	}

	metrics := RBEBuildMetrics("build-123", "v1.0", 12*time.Second, stats)
	if metrics.NumRecords != 90 {
		t.Errorf("NumRecords = %d; want 90", metrics.NumRecords)
	}
	if !cmp.Equal(metrics.RemoteCacheHitRatio, 0.8653846, cmpopts.EquateApprox(0, 0.000001)) {
		t.Errorf("RemoteCacheHitRatio = %f; want %f", metrics.RemoteCacheHitRatio, 0.8653846)
	}
	if !cmp.Equal(metrics.TwoPhaseCacheHitRatio, 0.2, cmpopts.EquateApprox(0, 0.000001)) {
		t.Errorf("TwoPhaseCacheHitRatio = %f; want %f", metrics.TwoPhaseCacheHitRatio, 0.2)
	}
	if !cmp.Equal(metrics.BuildCacheHitRatio, 0.50, cmpopts.EquateApprox(0, 0.000001)) {
		t.Errorf("BuildCacheHitRatio = %f; want %f", metrics.BuildCacheHitRatio, 0.50)
	}
	if len(metrics.Stats) != 1 {
		t.Fatalf("len(metrics.Stats) = %d; want 1", len(metrics.Stats))
	}
	stat := metrics.Stats[0]
	if stat.Name != "CompletionStatus" {
		t.Errorf("stat.Name = %q; want CompletionStatus", stat.Name)
	}

	// Verify empty stats behavior (avoid division by 0)
	emptyMetrics := RBEBuildMetrics("build-124", "v1.0", 5*time.Second, build.Stats{})
	if emptyMetrics.BuildCacheHitRatio != 0 {
		t.Errorf("BuildCacheHitRatio = %f; want 0", emptyMetrics.BuildCacheHitRatio)
	}
	if emptyMetrics.TwoPhaseCacheHitRatio != 0 {
		t.Errorf("TwoPhaseCacheHitRatio = %f; want 0", emptyMetrics.TwoPhaseCacheHitRatio)
	}
	if emptyMetrics.RemoteCacheHitRatio != 0 {
		t.Errorf("RemoteCacheHitRatio = %f; want 0", emptyMetrics.RemoteCacheHitRatio)
	}
}
