// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"errors"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/o11y/trace"
	"go.chromium.org/build/siso/reapi/retry"
)

// execRemoteRun runs the remote execution phase without post-processing.
// It handles retry, semaphore acquisition, and (unless
// cmd.SkipRecordOutputs is set, as in racing mode) recording outputs
// in hashFS, but does NOT call updateDeps or outputs.
// Used by runRacing to separate execution from post-processing.
//
// uploadCtx controls CAS input uploads; execCtx controls execution and
// retry/semaphore. In the non-racing path both are the same context.
// In racing mode, uploadCtx is the build context (not canceled by
// race cancellation) so that shared CAS upload operations are not
// poisoned when the race goroutine's execCtx is canceled.
func (b *Builder) execRemoteRun(uploadCtx, execCtx context.Context, step *Step) error {
	ctx, span := trace.NewSpan(execCtx, "exec-remote")
	defer span.Close(nil)
	noFallback := !b.localFallbackEnabled()
	var timeout time.Duration
	if noFallback {
		// In no-fallback mode, remote execution will be tried 4 times at most.
		// Since an execution sets cmd.Timeout * 2 as action timeout, the 2nd try might be deduplicated by RBE scheduler. 3rd or 4th try should successfully restart a new execution.
		// Alternatively, Siso could avoid extending action timeout in no-fallback mode.
		// However, this will lose the opportunity to cache long actions.
		timeout = step.cmd.Timeout * 4
	}
	step.cmd.RecordPreOutputs(ctx)
	clog.Infof(ctx, "exec remote %s", step.cmd.Desc)
	phase := stepRemoteRun
	var reExecDur time.Duration
	return retry.Do(ctx, func() error {
		step.setPhase(phase.wait())
		err := b.remoteSema.Do(ctx, step.weight, func(ctx context.Context) error {
			step.setPhase(phase)
			if phase == stepRetryRun {
				step.metrics.RemoteRetry++
				b.progressStepRetry(step)
			}
			reExecStarted := time.Now()
			b.actionStartedTime(step, reExecStarted)
			clog.Infof(ctx, "step state: remote exec [%s]", phase)
			phase = stepRetryRun
			err := b.remoteExec.Run(uploadCtx, ctx, step.cmd)
			step.setPhase(stepOutput)
			step.metrics.IsRemote = true
			result, cached := step.cmd.ActionResult()
			if !cached {
				b.updateREStat(result, err)
			}
			if err == nil && !validateRemoteActionResult(result) {
				clog.Errorf(ctx, "no outputs in action result. retry without cache lookup. b/350360391")
				res := cmdOutput(ctx, cmdOutputResultRETRY, step.cmd, b.reapiclient.Instance(), step.def.Binding("command"), step.def.RuleName(), err)
				b.logOutput(res, false)
				step.metrics.RemoteRetry++
				step.cmd.SkipCacheLookup = true
				step.setPhase(phase)
				err = b.remoteExec.Run(uploadCtx, ctx, step.cmd)
				step.setPhase(stepOutput)
				step.metrics.IsRemote = true
				result, cached = step.cmd.ActionResult()
				if !cached {
					b.updateREStat(result, err)
				}
				if err == nil && !validateRemoteActionResult(result) {
					clog.Errorf(ctx, "no outputs in action result again. b/350360391")
				}
			}
			if cached {
				step.metrics.Cached = true
			}
			// Simulates cache miss by sleeping for the remote execution time.
			// This is to match the total execution time with the actual remote execution
			// even when it hits the cache in RBE.
			if cached && experiments.Enabled("simulate-remote-cache-misses", "simulate cache miss") {
				step.metrics.Cached = false
				md := result.GetExecutionMetadata()
				if md != nil {
					execDur := md.GetWorkerCompletedTimestamp().AsTime().Sub(md.GetWorkerStartTimestamp().AsTime())
					runDur := time.Since(reExecStarted)
					sleepDur := execDur - runDur
					if sleepDur > 0 {
						clog.Infof(ctx, "simulate cache miss in execRemote: sleep %s", sleepDur)
						select {
						case <-ctx.Done():
							return context.Cause(ctx)
						case <-time.After(sleepDur):
						}
					}
				} else {
					clog.Warningf(ctx, "simulate cache miss: missing execution metadata in action result")
				}
			}
			step.metrics.RunTime = IntervalMetric(time.Since(reExecStarted))
			step.metrics.done(ctx, step, b.start)
			return err
		})
		reExecDur += time.Duration(step.metrics.RunTime)
		if code := status.Code(err); noFallback && (code == codes.DeadlineExceeded || errors.Is(err, context.DeadlineExceeded)) && reExecDur < timeout {
			clog.Warningf(ctx, "exec remote timedout duration=%s timeout=%s: %v", reExecDur, timeout, err)
			err = status.Errorf(codes.Unavailable, "reapi timedout %v", err)
		}
		return err
	})
}

func (b *Builder) updateREStat(result *rpb.ActionResult, err error) {
	md := result.GetExecutionMetadata()
	if md == nil {
		return
	}
	var queueDur, workerDur, inputDur, execDur, outputDur time.Duration
	queued := md.GetQueuedTimestamp()
	workerStarted := md.GetWorkerStartTimestamp()
	if queued != nil && workerStarted != nil {
		queueDur = workerStarted.AsTime().Sub(queued.AsTime())
	}
	workerCompleted := md.GetWorkerCompletedTimestamp()
	if workerStarted != nil && workerCompleted != nil {
		workerDur = workerCompleted.AsTime().Sub(workerStarted.AsTime())
	}
	inputFetchStarted := md.GetInputFetchStartTimestamp()
	inputFetchCompleted := md.GetInputFetchCompletedTimestamp()
	if inputFetchStarted != nil && inputFetchCompleted != nil {
		inputDur = inputFetchCompleted.AsTime().Sub(inputFetchStarted.AsTime())
	}
	execStarted := md.GetExecutionStartTimestamp()
	execCompleted := md.GetExecutionCompletedTimestamp()
	if execStarted != nil && execCompleted != nil {
		execDur = execCompleted.AsTime().Sub(execStarted.AsTime())
	}
	outputUploadStarted := md.GetOutputUploadStartTimestamp()
	outputUploadCompleted := md.GetOutputUploadCompletedTimestamp()
	if outputUploadStarted != nil && outputUploadCompleted != nil {
		outputDur = outputUploadCompleted.AsTime().Sub(outputUploadStarted.AsTime())
	}
	b.reStatMu.Lock()
	defer b.reStatMu.Unlock()
	b.reSchedStat.Update(queueDur, workerDur, err != nil)
	b.reWorkerStat.Update(inputDur+outputDur, execDur, err != nil)
}

func (b *Builder) execRemote(ctx context.Context, step *Step) error {
	if err := b.execRemoteRun(ctx, ctx, step); err != nil {
		return err
	}
	// need to update deps for remote exec for deps=gcc with depsfile,
	// or deps=msvc with showIncludes
	if err := b.updateDeps(ctx, step); err != nil {
		return err
	}
	return b.outputs(ctx, step)
}

func (b *Builder) execRemoteCache(ctx context.Context, step *Step) error {
	ctx, span := trace.NewSpan(ctx, "exec-remote-cache")
	defer span.Close(nil)
	var start time.Time
	err := b.cacheSema.Do(ctx, func(ctx context.Context) error {
		start = time.Now()
		b.cacheStarted(step)
		defer b.cacheFinish(step)
		err := b.cache.GetActionResult(ctx, step.cmd)
		if err != nil {
			return err
		}
		if experiments.Enabled("simulate-remote-cache-misses", "simulate cache miss") {
			clog.Infof(ctx, "simulate cache miss in execRemoteCache")
			return status.Errorf(codes.NotFound, "simulate cache miss")
		}
		result, _ := step.cmd.ActionResult()
		// result may be nil if GetActionResult detects
		// "skip: no need to update", i.e. all outputs
		// generated by the same action digest.
		if result != nil && !validateRemoteActionResult(result) {
			clog.Errorf(ctx, "no outputs in action result. ignore cache lookup. b/350360391")
			step.cmd.SkipCacheLookup = true
			return errors.New("no output in action result")
		}
		b.progressStepCacheHit(step)
		step.metrics.Cached = true
		return nil
	})
	if err != nil {
		// An error at this point means that we will run this step, so we are still not done.
		return err
	}

	// Beyond this point, we should be marking the step done.  Do that as we leave the function.
	b.actionStartedTime(step, b.start.Add(time.Duration(step.metrics.CacheStartTime)))
	defer func() {
		step.metrics.RunTime = IntervalMetric(time.Since(start))
		step.metrics.done(ctx, step, b.start)
	}()

	// need to update deps for cache hit for deps=gcc, msvc.
	// even if cache hit, deps should be updated with gcc depsfile,
	// or with msvc showIncludes outputs.
	if err = b.updateDeps(ctx, step); err != nil {
		return err
	}
	return b.outputs(ctx, step)
}
