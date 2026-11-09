// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"slices"
	"time"

	"go.chromium.org/build/siso/o11y/trace"
)

func (b *Builder) allowRemote(step *Step) bool {
	// Criteria for remote executable:
	// - Allow remote if available and command has platform property.
	return (b.remoteExec != nil && len(step.cmd.Platform) > 0)
}

func (b *Builder) allowTwoPhaseCaching(step *Step) bool {
	if !experiments.Enabled("two-phase-caching", "") {
		return false
	}
	if b.twoPhaseCaching == nil {
		return false
	}
	if b.cache == nil || !b.reCacheEnableRead {
		return false
	}
	if step.def.Binding("generator") != "" {
		// gn gen step fails?
		// err: error in depfile "out/tpc/build.ninja.d": deps input "clang_x64_for_rust_host_build_tools/gen/build/modules/linux/module.modulemap" is output
		return false
	}
	if step.cmd.SkipCacheLookup || step.cmd.DoNotCache {
		return false
	}
	if step.cmd.Pure && b.allowRemote(step) {
		switch step.cmd.Deps {
		case "gcc", "msvc":
			// try two phase caching to avoid scandeps.
			return true
		default:
			// no need to use two phase caching.
			// just lookup cache by GetActionResult.
			return false
		}
	}
	return true
}

func (b *Builder) runStrategy(step *Step) func(context.Context, *Step) error {
	// Check criteria for allowRemote.
	// If the command doesn't meet either criteria, fallback to local.
	// Any further validation should be done in the exec handler, not here.
	switch {
	case step.cmd.Pure && b.allowRemote(step) && b.racingEnabled:
		return b.runRacing
	case step.cmd.Pure && b.allowRemote(step):
		return b.runRemote
	default:
		return b.runLocal
	}
}

func (b *Builder) runLocal(ctx context.Context, step *Step) error {
	if len(b.localActionSalt) > 0 {
		step.cmd.ActionSalt = b.localActionSalt
	}
	if len(b.env) > 0 {
		step.cmd.Env = slices.Concat(b.env, step.cmd.Env)
	}
	if len(step.cmd.Env) > 0 {
		step.cmd.EnvCacheKeyOmits = b.envCacheKeyOmits
		step.cmd.EnvCacheKeySubstitutions = b.envCacheKeySubstitutions
		step.cmd.EnvCacheKeyTrimPrefixes = b.envCacheKeyTrimPrefixes
	}
	// preproc performs scandeps to list up all inputs, so
	// we can flush these inputs before local execution.
	// but we already flushed generated *.h etc, no need to
	// preproc for local run.
	// execLocal expands and dedups the inputs.
	// TODO: use local cache?
	return b.execLocal(ctx, step)
}

// actionStartedSilent is called when the early steps of execution (scandeps, cache
// query) are started.  Do not report the action started to the frontend.
func (b *Builder) actionStartedSilent(step *Step) {
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

// scandepsStarted exists as a common function for scandeps methods (gcc, msvc)
// to call when the scandeps semaphore is acquired.
//
// Mark the step as started internally (if not already marked started),
// but don't present the step started to the frontend yet because this is still
// early stages of execution; see [Builder.actionStartedSilent].
//
// Returns a trace context and span to cover the scandeps execution.
// This span should be closed when scandeps is finished.
func (b *Builder) scandepsStarted(ctx context.Context, step *Step) (context.Context, *trace.Span) {
	b.actionStartedSilent(step)
	return trace.NewSpan(ctx, spanScandepsRun)
}
