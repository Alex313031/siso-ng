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

// execRemoteExecute runs the remote execution phase without post-processing.
// It handles retry, semaphore acquisition, and remote execution,
// but does NOT download outputs or stdout/stderr.
//
// Canceling ctx (e.g. when the local racer wins) makes this return
// promptly even while CAS input uploads are in flight; the uploads are
// owned by the reapi client and keep running for other steps.
func (b *Builder) execRemoteExecute(ctx context.Context, step *Step) error {
	ctx, span := trace.NewSpan(ctx, "exec-remote-execute")
	defer span.Close(nil)
	noFallback := !b.localFallbackEnabled(step)
	var timeout time.Duration
	if noFallback {
		// In no-fallback mode, remote execution will be tried 4 times at most.
		// Since an execution sets cmd.Timeout * 2 as action timeout, the 2nd try might be deduplicated by RBE scheduler. 3rd or 4th try should successfully restart a new execution.
		// Alternatively, Siso could avoid extending action timeout in no-fallback mode.
		// However, this will lose the opportunity to cache long actions.
		timeout = step.cmd.Timeout * 4
	}
	clog.Infof(ctx, "exec remote execute %s", step.cmd.Desc)
	phase := stepRemoteRun
	var reExecDur time.Duration
	var maxRetries int
	if b.reapiclient != nil {
		maxRetries = b.reapiclient.MaxRetries()
	}
	return retry.DoWithMaxRetries(ctx, maxRetries, func() error {
		step.setPhase(phase.wait())
		// When and how long the attempt took.
		// Stays zero if sema is never entered (e.g. due to cancel)
		var attemptStart time.Time
		var attemptDur time.Duration
		err := b.remoteSema.Do(ctx, step.weight, func(ctx context.Context) error {
			step.setPhase(phase)
			if phase == stepRetryRun {
				step.metrics.RemoteRetry++
				b.progressStepRetry(step)
			}
			attemptStart = time.Now()
			b.actionStartedTime(step, attemptStart)
			clog.Infof(ctx, "step state: remote exec [%s]", phase)
			phase = stepRetryRun
			result, cached, err := b.remoteExec.Execute(ctx, step.cmd)
			step.setPhase(stepOutput)
			step.metrics.IsRemote = true
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
				result, cached, err = b.remoteExec.Execute(ctx, step.cmd)
				step.setPhase(stepOutput)
				step.metrics.IsRemote = true
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
					elapsed := time.Since(attemptStart)
					sleepDur := execDur - elapsed
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
			attemptEnd := time.Now()
			attemptDur = attemptEnd.Sub(attemptStart)
			span.Add(ctx, trace.SpanData{
				Name:  spanExecRemoteExecAttempt,
				Start: attemptStart,
				End:   attemptEnd,
			})
			step.metrics.done(ctx, b, step, b.start)
			return err
		})
		reExecDur += attemptDur
		if code := status.Code(err); noFallback && (code == codes.DeadlineExceeded || errors.Is(err, context.DeadlineExceeded)) && reExecDur < timeout {
			clog.Warningf(ctx, "exec remote timedout duration=%s timeout=%s: %v", reExecDur, timeout, err)
			err = status.Errorf(codes.Unavailable, "reapi timedout %v", err)
		}
		return err
	})
}

// execRemoteDownload downloads the results from the remote execution.
func (b *Builder) execRemoteDownload(ctx context.Context, step *Step, execErr error) error {
	ctx, span := trace.NewSpan(ctx, "exec-remote-download")
	defer span.Close(nil)
	result, cached := step.cmd.ActionResult()
	return b.remoteExec.ProcessResult(ctx, step.cmd, result, cached, execErr)
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
	execErr := b.execRemoteExecute(ctx, step)

	ctx, span := trace.NewSpan(ctx, spanExecRemoteExecPostProc)
	defer span.Close(nil)

	downloadErr := b.execRemoteDownload(ctx, step, execErr)
	if downloadErr != nil {
		return downloadErr
	}
	// need to update deps for remote exec for deps=gcc with depsfile,
	// or deps=msvc with showIncludes
	if err := b.updateDeps(ctx, step); err != nil {
		return err
	}
	return b.outputs(ctx, step)
}

func (b *Builder) execRemoteCache(ctx context.Context, step *Step) error {
	ctx, span := trace.NewSpan(ctx, spanExecRemoteCache)
	defer span.Close(nil)
	var runSpan *trace.Span
	var start time.Time
	err := b.cacheSema.Do(ctx, func(ctx context.Context) error {
		start = time.Now()
		b.actionStartedSilent(step)
		ctx, runSpan = trace.NewSpan(ctx, spanExecRemoteCacheCheck)
		defer runSpan.Close(nil)
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
	// TODO(b/520207778): Refactor actionStartedTime. Better to decide "real start time"
	// at the end of step so that it's easier to understand how the metrics are decided.
	// Otherwise, need to manually trace every execution strategy to understand how metrics
	// are decided for each step which is error-prone.
	actionStarted := start
	if runSpan != nil {
		actionStarted = runSpan.Start()
	}
	b.actionStartedTime(step, actionStarted)
	defer func() {
		span.Add(ctx, trace.SpanData{
			Name:  spanExecRemoteCacheRun,
			Start: start,
			End:   time.Now(),
		})
		step.metrics.done(ctx, b, step, b.start)
	}()

	// need to update deps for cache hit for deps=gcc, msvc.
	// even if cache hit, deps should be updated with gcc depsfile,
	// or with msvc showIncludes outputs.
	if err = b.updateDeps(ctx, step); err != nil {
		return err
	}
	return b.outputs(ctx, step)
}
