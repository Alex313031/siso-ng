// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"time"
)

func (b *Builder) allowRemote(step *Step) bool {
	// Criteria for remote executable:
	// - Allow remote if available and command has platform property.
	return (b.remoteExec != nil && len(step.cmd.Platform) > 0)
}

func (b *Builder) allowREProxy(step *Step) bool {
	// Criteria for REProxy:
	// - Allow reproxy if available and command has reproxy config set.
	return (b.reproxyExec.Enabled() && step.cmd.REProxyConfig != nil)
}

func (b *Builder) runStrategy(step *Step) func(context.Context, *Step) error {
	// Check criteria for allowRemote and allowREProxy
	// If the command doesn't meet either criteria, fallback to local.
	// Any further validation should be done in the exec handler, not here.
	switch {
	case step.cmd.Pure && b.allowREProxy(step):
		return b.runReproxy
	case step.cmd.Pure && b.allowRemote(step) && b.racingEnabled:
		return b.runRacing
	case step.cmd.Pure && b.allowRemote(step):
		return b.runRemote
	default:
		return b.runLocal
	}
}

func (b *Builder) runReproxy(ctx context.Context, step *Step) error {
	dedupInputs(ctx, step.cmd)
	// TODO: b/297807325 - Siso relies on Reproxy's local fallback for
	// monitoring at this moment. So, Siso shouldn't try local fallback.
	return b.execReproxy(ctx, step)
}

func (b *Builder) runLocal(ctx context.Context, step *Step) error {
	// preproc performs scandeps to list up all inputs, so
	// we can flush these inputs before local execution.
	// but we already flushed generated *.h etc, no need to
	// preproc for local run.
	dedupInputs(ctx, step.cmd)
	// TODO: use local cache?
	return b.execLocal(ctx, step)
}

// actionStarted is called when the early steps of execution (scandeps, cache
// query) are started.  Do not report the action started to the frontend.
func (b *Builder) actionStarted(step *Step) {
	// actionStarted may be called when fallback/retry.
	// Do not change ActionStartTime if it's already set.
	if step.metrics.ActionStartTime == 0 {
		step.metrics.ActionStartTime = IntervalMetric(time.Since(b.start))
	}
}

// actionStartedTime is called when execution of the action begins.
// If the start time has not been recorded, set it to the time provided.  This
// is generally `time.Now()` except in the case of a cache hit, where it is the
// cacheStartTime.
//
// Always report the action started to the frontend, but only once.
func (b *Builder) actionStartedTime(step *Step, start time.Time) {
	// actionStarted may be called when fallback/retry.
	// Do not change ActionStartTime if it's already set.
	if step.metrics.ActionStartTime == 0 {
		step.metrics.ActionStartTime = IntervalMetric(start.Sub(b.start))
	}
	step.startReported.Do(func() {
		b.statusReporter.BuildActionStarted(step, start)
	})
}

func (b *Builder) actionFinished(ctx context.Context, step *Step) {
	step.finishReported.Do(func() {
		if ctx.Err() != nil {
			b.statusReporter.BuildActionCanceled(step)
			return
		}
		b.statusReporter.BuildActionFinished(step)
	})
}

func (b *Builder) scandepsStarted(step *Step) {
	b.actionStarted(step)
	if step.metrics.ScandepsStartTime == 0 {
		step.metrics.ScandepsStartTime = IntervalMetric(time.Since(b.start))
	}
}

func (b *Builder) scandepsFinish(step *Step) {
	if step.metrics.ScandepsStartTime != 0 {
		end := time.Since(b.start)
		step.metrics.ScandepsTime = IntervalMetric(end - time.Duration(step.metrics.ScandepsStartTime))
	}
}

func (b *Builder) cacheStarted(step *Step) {
	b.actionStarted(step)
	if step.metrics.CacheStartTime == 0 {
		step.metrics.CacheStartTime = IntervalMetric(time.Since(b.start))
	}
}

func (b *Builder) cacheFinish(step *Step) {
	if step.metrics.CacheStartTime != 0 {
		end := time.Since(b.start)
		step.metrics.CacheTime = IntervalMetric(end - time.Duration(step.metrics.CacheStartTime))
	}
}
