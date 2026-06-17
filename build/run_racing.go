// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"errors"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"go.chromium.org/build/siso/execute"
	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/o11y/trace"
)

type raceResultType int

const (
	raceRemote raceResultType = iota
	raceLocal
	raceCanceled
	raceRemoteFallback
)

type raceResult struct {
	resultType raceResultType
	err        error
}

// runRacing runs a step with local and remote execution in parallel.
// Whichever finishes first wins; the loser is cancelled.
// If the remote side fails (during execution or while post-processing
// its result: downloading outputs, updating deps, or flushing outputs
// to the local disk), it falls back to local execution when
// remoteClaimFallbackIfAllowed permits; otherwise the step fails.
func (b *Builder) runRacing(ctx context.Context, step *Step) error {
	ctx, span := trace.NewSpan(ctx, "racing")
	defer span.Close(nil)

	step.metrics.Racing = true

	preprocErr := preprocCmd(ctx, b, step)
	if preprocErr != nil {
		// Can't determine inputs for remote - just run locally.
		clog.Infof(ctx, "racing: preproc failed, local only: %v", preprocErr)
		return b.execLocal(ctx, step)
	}
	dedupInputs(ctx, step.cmd)

	// Check remote cache with full deps.
	if b.cache != nil && b.reCacheEnableRead {
		err := b.execRemoteCache(ctx, step)
		if err == nil {
			return nil
		}
		clog.Infof(ctx, "racing: cache miss: %v", err)
	}

	if !b.reExecEnable {
		// Remote execution disabled - just run locally.
		return b.execLocal(ctx, step)
	}

	// Clone the step for the local racer so the two sides don't
	// interfere with each other's mutable state.
	localStep := step.Clone()

	// Skip RecordOutputs inside remoteExec.Run so that the remote
	// goroutine does not modify hashFS concurrently with the local
	// goroutine's RecordOutputsFromLocal.  Outputs are recorded
	// after the race is decided (see raceRemote case below).
	step.cmd.SkipRecordOutputs = true

	raceCtx, raceCancel := context.WithCancel(ctx)
	defer raceCancel()
	ch := make(chan raceResult, 2)

	// Remote goroutine - only the execution phase (no updateDeps/outputs).
	// Post-processing runs after the race is decided to avoid hashFS
	// races with the local goroutine's RecordOutputsFromLocal.
	go func() {
		// ctx is the build context; raceCtx is canceled when the
		// race decides. Use ctx for CAS uploads so they survive
		// race cancellation and don't poison shared upload
		// operations used by other steps (reclient-style).
		err := b.execRemoteExecute(ctx, raceCtx, step)
		if isContextCanceledErr(err) {
			ch <- raceResult{resultType: raceCanceled, err: err}
			return
		}
		if err == nil {
			if result, _ := step.cmd.ActionResult(); result != nil && result.GetExitCode() != 0 {
				err = execute.ExitError{ExitCode: int(result.GetExitCode())}
			}
		}
		if err != nil {
			ok, ferr := b.remoteClaimFallbackIfAllowed(ctx, step, err)
			if !ok {
				ch <- raceResult{resultType: raceRemote, err: ferr}
				return
			}
			b.setupFallback(ctx, step, err)
			ch <- raceResult{resultType: raceRemoteFallback, err: err}
			return
		}
		ch <- raceResult{resultType: raceRemote, err: err}
	}()

	// Local goroutine - full execLocal (including post-processing).
	go func() {
		err := b.execLocal(raceCtx, localStep)
		if isContextCanceledErr(err) {
			ch <- raceResult{resultType: raceCanceled, err: err}
			return
		}
		ch <- raceResult{resultType: raceLocal, err: err}
	}()

	// Wait for the first non-canceled result.
	first := <-ch

	// Determine the winner.
	var winner raceResult
	switch first.resultType {
	case raceCanceled:
		// First result was a cancellation (e.g. couldn't acquire
		// semaphore, or gRPC canceled from shared CAS upload).
		// Wait for the second result instead.
		winner = <-ch
		// Both goroutines have now sent their results;
		// defer raceCancel() handles cleanup.
	case raceRemoteFallback:
		// Remote failed, and we are falling back to local.
		// Do not cancel local, wait for it to finish.
		winner = <-ch
	default:
		// Cancel the loser.
		raceCancel()
		// Wait for the second goroutine to finish before returning,
		// so we don't leak goroutines that hold semaphore slots.
		<-ch
		winner = first
	}

	switch winner.resultType {
	case raceRemote:
		step.metrics.RacingWinner = "remote"
		clog.Infof(ctx, "racing: remote won")

		if winner.err != nil {
			// The remote goroutine already ran remoteClaimFallbackIfAllowed
			// and was denied fallback. Fetch stdout/stderr for the failure
			// report and fail the step.
			return b.execRemoteDownload(ctx, step, winner.err)
		}

		// Post-process the remote result. Any failure here (downloading
		// outputs, updating deps, missing outputs, or flushing outputs to
		// the local disk) falls back to local execution, like runRemote does.

		// Download outputs and stdout/stderr now.
		if err := b.execRemoteDownload(ctx, step, nil); err != nil {
			return b.fallbackLocal(ctx, step, err)
		}
		// The loser (local) has been canceled and drained - on cancellation
		// localexec kills the action's whole process group and waits until it is
		// empty, so nothing is still writing - but it might have already deleted
		// or modified the local output files before being killed.
		// Even when the local racer never started the command, hashFS may
		// hold a stale local-ready entry for an output that was removed
		// from disk behind siso's back (e.g. by a pre-build cleanup step
		// after .siso_fs_state was loaded). b/522434556
		// Forget the cached outputs in HashFS so that Siso doesn't assume the
		// outputs are still "local-ready" (up-to-date) on the local disk.
		// This forces Siso to download/verify them during b.outputs().
		step.cmd.HashFS.ForgetOutputs(ctx, step.cmd.WorkspaceRoot, step.cmd.AllOutputs())
		// Re-record the remote outputs so that updateDeps and b.outputs
		// see the correct state.
		if err := step.cmd.RecordOutputs(ctx, step.cmd.HashFS.DataSource(), time.Now()); err != nil {
			return b.fallbackLocal(ctx, step, err)
		}
		// Update deps from the remote result (gcc depsfile / msvc showIncludes).
		if err := b.updateDeps(ctx, step); err != nil {
			return b.fallbackLocal(ctx, step, err)
		}
		// Flush the outputs to the local disk.
		if err := b.outputs(ctx, step); err != nil {
			return b.fallbackLocal(ctx, step, err)
		}
		return nil

	case raceLocal:
		step.metrics.RacingWinner = "local"
		step.metrics.IsRemote = false
		step.metrics.IsLocal = true
		if step.metrics.Fallback {
			clog.Infof(ctx, "racing: local fallback finished")
		} else {
			clog.Infof(ctx, "racing: local won")
		}
		// Copy stdout/stderr and action result even if it failed,
		// so the caller can report the failure details.
		if stdout := localStep.cmd.Stdout(); len(stdout) > 0 {
			step.cmd.StdoutWriter().Write(stdout)
		}
		if stderr := localStep.cmd.Stderr(); len(stderr) > 0 {
			step.cmd.StderrWriter().Write(stderr)
		}
		step.adoptRacingLocalResult(localStep)
		result, cached := localStep.cmd.ActionResult()
		step.cmd.SetActionResult(result, cached)

		if winner.err != nil {
			return winner.err
		}
		// Local outputs are already on disk - no need to call
		// b.outputs(), b.updateDeps(), or b.checkLocalOutputs()
		// since execLocal already did all of that on localStep.
		return nil

	default:
		// Both were canceled - parent context was canceled.
		clog.Warningf(ctx, "racing: both sides canceled")
		return winner.err
	}
}

// adoptRacingLocalResult copies the local racer clone's execution result
// back onto the original step after the local side wins a race. The
// weighted duration is derived from the original step, which is the one
// the progress ticker accumulated it on.
func (step *Step) adoptRacingLocalResult(localStep *Step) {
	step.metrics.copyExecResult(&localStep.metrics)
	step.state.copyExecResult(localStep.state)
	step.metrics.WeightedDuration = IntervalMetric(step.getWeightedDuration())
}

// isContextCanceledErr reports whether err wraps context.Canceled or
// a gRPC status with code Canceled.  gRPC wraps context.Canceled into
// a status error whose Is method does not match context.Canceled, so
// errors.Is alone is insufficient.  This also catches cancellation
// errors propagated through shared infrastructure (e.g. CAS upload
// ops) where a different goroutine's context was canceled.
func isContextCanceledErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return true
	}
	// Unwrap through fmt.Errorf %w chains to find a gRPC status error.
	var se interface{ GRPCStatus() *status.Status }
	if errors.As(err, &se) {
		return se.GRPCStatus().Code() == codes.Canceled
	}
	return false
}
