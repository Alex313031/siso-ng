// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package reclientutil

import (
	"fmt"
	"runtime"
	"time"

	"go.chromium.org/build/siso/build"
	pb "go.chromium.org/build/siso/toolsupport/reclientutil/proto"
)

// RBEBuildMetrics converts Siso build stats into Reclient's RBE build metrics.
func RBEBuildMetrics(buildID string, version string, dur time.Duration, stats build.Stats) *pb.RbeBuildMetrics {
	var remoteCacheHitRatio float64 = 0
	var localCacheHitRatio float64 = 0
	var overallCacheHitRatio float64 = 0
	// Remote includes CacheHitLate: do not add that to the total.
	// Include RacingLocal and LocalFallback here as they represent actions
	// that are configured to be run remotely that were either executed
	// locally (not found in the REAPI cache), or were run locally
	// (cache-miss, the build failed, and succeeded when run locally).
	totalRemoted := stats.Remote + stats.CacheHitEarly + stats.RacingLocal + stats.LocalFallback
	if totalRemoted > 0 {
		// Count late cache hits as cache hits.
		remoteCacheHitRatio = float64(stats.CacheHitEarly+stats.CacheHitLate) / float64(totalRemoted)
	}
	// The two-phase-caching experiment means that local actions may be cached.
	// Remoted actions never use the two-phase cache, making the calculation easier.
	totalLocal := stats.Local + stats.TwoPhaseCacheHit
	if totalLocal > 0 {
		localCacheHitRatio = float64(stats.TwoPhaseCacheHit) / float64(totalLocal)
	}
	// Calculate the overall cache hit ratio.
	if stats.Done-stats.Skipped > 0 {
		overallCacheHitRatio = float64(stats.CacheHit) / float64(stats.Done-stats.Skipped)
	}
	return &pb.RbeBuildMetrics{
		NumRecords: int64(stats.Done - stats.Skipped),
		Stats: []*pb.Stat{
			completionStatus(stats),
		},
		ToolVersion:   fmt.Sprintf("siso-%s", version),
		InvocationIds: []string{buildID},
		MachineInfo: &pb.MachineInfo{
			NumCpu:   int64(runtime.GOMAXPROCS(0)),
			OsFamily: runtime.GOOS,
			Arch:     runtime.GOARCH,
		},
		BuildCacheHitRatio:    overallCacheHitRatio,
		TwoPhaseCacheHitRatio: localCacheHitRatio,
		RemoteCacheHitRatio:   remoteCacheHitRatio,
		BuildLatency:          dur.Seconds(),
	}
}

// completionStatus fills in Reclient's CompletionStatus with Siso's build stats.
// https://github.com/bazelbuild/reclient/blob/dbabdc03691e4a293f0b8b6656cdc27f892c4e54/api/log/log.proto#L51
func completionStatus(stats build.Stats) *pb.Stat {
	s := &pb.Stat{
		Name:          "CompletionStatus",
		CountsByValue: []*pb.Stat_Value{},
	}
	if stats.CacheHit > 0 {
		s.CountsByValue = append(s.CountsByValue, &pb.Stat_Value{
			Name:  "STATUS_CACHE_HIT",
			Count: int64(stats.CacheHit),
		})
	}
	if stats.TwoPhaseCacheHit > 0 {
		s.CountsByValue = append(s.CountsByValue, &pb.Stat_Value{
			Name:  "STATUS_TWO_PHASE_CACHE_HIT",
			Count: int64(stats.TwoPhaseCacheHit),
		})
	}
	if stats.CacheHitEarly > 0 {
		s.CountsByValue = append(s.CountsByValue, &pb.Stat_Value{
			Name:  "STATUS_CACHE_HIT_EARLY",
			Count: int64(stats.CacheHitEarly),
		})
	}
	if stats.CacheHitLate > 0 {
		s.CountsByValue = append(s.CountsByValue, &pb.Stat_Value{
			Name:  "STATUS_CACHE_HIT_LATE",
			Count: int64(stats.CacheHitLate),
		})
	}
	if stats.RacingRemote > 0 {
		s.CountsByValue = append(s.CountsByValue, &pb.Stat_Value{
			Name:  "STATUS_RACING_REMOTE",
			Count: int64(stats.RacingRemote),
		})
	}
	if stats.RacingLocal > 0 {
		s.CountsByValue = append(s.CountsByValue, &pb.Stat_Value{
			Name:  "STATUS_RACING_LOCAL",
			Count: int64(stats.RacingLocal),
		})
	}
	if stats.Remote > 0 {
		s.CountsByValue = append(s.CountsByValue, &pb.Stat_Value{
			Name:  "STATUS_REMOTE_EXECUTION",
			Count: int64(stats.Remote),
		})
	}
	if stats.LocalFallback > 0 {
		s.CountsByValue = append(s.CountsByValue, &pb.Stat_Value{
			Name:  "STATUS_LOCAL_FALLBACK",
			Count: int64(stats.LocalFallback),
		})
	}
	if stats.Local > 0 {
		s.CountsByValue = append(s.CountsByValue, &pb.Stat_Value{
			Name:  "STATUS_LOCAL_EXECUTION",
			Count: int64(stats.Local),
		})
	}
	if stats.Fail > 0 {
		s.CountsByValue = append(s.CountsByValue, &pb.Stat_Value{
			Name:  "STATUS_NON_ZERO_EXIT",
			Count: int64(stats.Fail),
		})
	}
	return s
}
