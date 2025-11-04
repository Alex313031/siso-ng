// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"sync"

	"go.chromium.org/build/siso/o11y/clog"
)

type stats struct {
	mu sync.Mutex
	s  Stats
}

func newStats(total int) *stats {
	return &stats{
		s: Stats{
			Total: total,
		},
	}
}

func (s *stats) update(ctx context.Context, m *StepMetric, pure bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.s.Done++
	// done step should be one of the followings.
	// note: metrics may have Cached+IsRemote, if
	// remote exec called and got cache, rather than
	// just get cache by GetActionResult.
	switch {
	case m.skip:
		s.s.Skipped++
	case m.NoExec:
		s.s.NoExec++
	case m.Cached:
		s.s.CacheHit++
	case m.IsRemote:
		s.s.Remote++
	case m.IsLocal:
		s.s.Local++
	case m.Err: // maybe canceled?
	default:
		clog.Warningf(ctx, "unexpected metrics? %#v", m)
	}

	if m.Fallback {
		s.s.LocalFallback++
	}
	if m.CacheWrite {
		s.s.CacheWrite++
	}
	if m.CacheWriteErr {
		s.s.CacheWriteErr++
	}
	if m.Err {
		s.s.Fail++
	}
	s.s.RemoteRetry += m.RemoteRetry

	if m.DepsLog {
		s.s.FastDepsSuccess++
	}
	if m.DepsLogErr {
		s.s.FastDepsFailed++
	}
	if m.ScandepsErr {
		s.s.ScanDepsFailed++
	}
	if m.ClangScandeps {
		s.s.ClangScanDeps++
	}
	if pure {
		s.s.Pure++
	}
}

// Stats keeps statistics about the build, such as the number of total, skipped or remote actions.
type Stats struct {
	Done            int // completed actions, including skipped, failed
	Fail            int // failed actions
	Pure            int // pure actions
	Skipped         int // skipped actions, because they were still up-to-date
	NoExec          int // actions that was completed by handler without execute cmds e.g. stamp, copy
	FastDepsSuccess int // actions that ran successfully when we used deps from the deps cache
	FastDepsFailed  int // actions that failed when we used deps from the deps cache
	ScanDepsFailed  int // actions that scandeps failed
	ClangScanDeps   int // actions that use clang for scandeps.
	CacheHit        int // actions for which we got a cache hit
	Local           int // locally executed actions
	Remote          int // remote executed actions
	LocalFallback   int // actions for which remote execution failed, and we did a local fallback
	CacheWrite      int // locally executed actions whose trusted results were uploaded directly to RE
	CacheWriteErr   int // locally executed actions that failed uploading results directly to RE
	RemoteRetry     int // accumulated remote retry counts
	Total           int // total actions that ran during this build
}

func (s *stats) stats() Stats {
	if s == nil {
		return Stats{}
	}
	s.mu.Lock()
	stats := s.s
	s.mu.Unlock()
	return stats
}
