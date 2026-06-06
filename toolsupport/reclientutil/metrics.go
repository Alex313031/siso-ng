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
	var cacheHitRatio float64 = 0
	if stats.Remote+stats.CacheHit > 0 {
		cacheHitRatio = float64(stats.CacheHit / (stats.Remote + stats.CacheHit))
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
		BuildCacheHitRatio: cacheHitRatio,
		BuildLatency:       dur.Seconds(),
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
