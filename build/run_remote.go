// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"go.chromium.org/build/siso/execute"
	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/o11y/trace"
	"go.chromium.org/build/siso/reapi"
)

var errDepsLog = errors.New("failed to exec with deps log")
var errNeedPreproc = errors.New("need to preproc")
var errRemoteExecDisabled = errors.New("remote exec disabled")

// runRemote runs step with using remote apis.
//
//  1. for initial steps of startLocal, run locally.
//  2. Check remote cacche with deps log if available.
//  3. If local resource is idle, run locally.
//  4. Otherwise, try running a remote execution with deps log.
//  5. If it failed, it will retry a remote execution with deps scan.
//  6. If it still failed, it will fallback to local execution.
//
// - Before each remote exec, it checks remote cache before running.
// - The fallbacks can be disabled via experiment flags.
func (b *Builder) runRemote(ctx context.Context, step *Step) error {
	var fastStep *Step
	var fastOK, fastChecked bool
	preprocErr := errNeedPreproc
	fastNeedCheckCache := true
	needCheckCache := true
	cacheCheck := b.cache != nil && b.reCacheEnableRead
	startLocal := b.startLocalCounter.Add(-1) >= 0
	if startLocal {
		// no cacheCheck as startlocal for incremental build
		// will build modified code, and not expect cache hit (?)
		clog.Infof(ctx, "start local %s", step.cmd.Desc)
		err := b.execLocal(ctx, step)
		step.metrics.StartLocal = true
		return err
	} else if b.fastLocalSema != nil && int(b.progress.numLocal.Load()) < b.fastLocalSema.Capacity() {
		// TODO: skip check cache when step is too new and can't expect cache hit?
		if cacheCheck {
			clog.Infof(ctx, "check cache before fast local")
			fastStep, fastOK = fastDepsCmd(ctx, b, step)
			if fastOK {
				err := b.execRemoteCache(ctx, fastStep)
				if err == nil {
					return b.fastStepDone(ctx, step, fastStep)
				}
				fastNeedCheckCache = false
				clog.Infof(ctx, "cmd fast cache miss: %v", err)
			}
			fastChecked = true

			preprocErr = preprocCmd(ctx, b, step)
			if len(step.cmd.Platform) > 0 && preprocErr == nil {
				err := b.execRemoteCache(ctx, step)
				if err == nil {
					return nil
				}
				needCheckCache = false
				clog.Infof(ctx, "cmd cache miss: %v", err)
			}
		}
		if ctx, done, err := b.fastLocalSema.TryAcquire(ctx); err == nil {
			var err error
			defer func() { done(err) }()
			clog.Infof(ctx, "fast local %s", step.cmd.Desc)
			// TODO: detach remote for future cache hit.
			err = b.execLocal(ctx, step)
			step.metrics.FastLocal = true
			return err
		}
	}
	if !fastChecked {
		fastStep, fastOK = fastDepsCmd(ctx, b, step)
	}
	if fastOK {
		err := b.tryFastStep(ctx, step, fastStep, fastNeedCheckCache && cacheCheck)
		if !errors.Is(err, errDepsLog) {
			return err
		}
	}
	if errors.Is(preprocErr, errNeedPreproc) {
		preprocErr = preprocCmd(ctx, b, step)
	}
	err := preprocErr
	if err == nil {
		err = b.runRemoteStep(ctx, step, needCheckCache && cacheCheck)
	}
	if err != nil {
		if errors.Is(err, errRemoteExecDisabled) {
			return b.execLocal(ctx, step)
		}
		if errors.Is(err, context.Canceled) {
			return err
		}
		if errors.Is(err, reapi.ErrBadPlatformContainerImage) {
			return err
		}
		if status.Code(err) == codes.PermissionDenied {
			return err
		}
		if errors.Is(err, errNotRelocatable) {
			clog.Errorf(ctx, "not relocatable: %v", err)
			return err
		}
		if errors.Is(err, errNotUnderExecRoot) {
			clog.Errorf(ctx, "not remote executable: %v\nUse `use_system_inputs` or put them under exec_root", err)
			return err
		}
		var eerr execute.ExitError
		if errors.As(err, &eerr) && len(step.cmd.Stdout())+len(step.cmd.Stderr()) > 0 && b.failures.allowed == 1 {
			var output string
			if len(step.cmd.Outputs) > 0 {
				output = step.cmd.Outputs[0]
			}
			switch {
			case eerr.ExitCode == 137:
				clog.Warningf(ctx, "Fallback due to potential SIGKILL by docker: remote exec %s failed: output=%q siso_config=%q, gn_target=%q: %v", step.cmd.ActionDigest(), output, step.def.RuleName(), step.def.Binding("gn_target"), err)
			case experiments.Enabled("fallback-on-exec-error", "remote exec %s failed: %v", step.cmd.ActionDigest(), err):
				clog.Warningf(ctx, "fallback-on-exec-error: remote exec %s failed: output=%q siso_config=%q, gn_target=%q: %v", step.cmd.ActionDigest(), output, step.def.RuleName(), step.def.Binding("gn_target"), err)
			default:
				// report compile fail early to developers.
				// If user runs on non-terminal or user sets a
				// non-default -k, then it implies that they want to
				// keep going as much as possible and
				// correct result, rather than fast feedback.
				return fmt.Errorf("remote-exec %s failed: %w", step.cmd.ActionDigest(), err)
			}
		}
		if !b.localFallbackEnabled() {
			return fmt.Errorf("remote-exec %s failed no-fallback: %w", step.cmd.ActionDigest(), err)
		}
		clog.Warningf(ctx, "remote-exec %s failed, fallback to local: %v", step.cmd.ActionDigest(), err)
		b.progressStepFallback(step)
		step.metrics.IsRemote = false
		step.metrics.Fallback = true
		res := cmdOutput(ctx, cmdOutputResultFALLBACK, step.cmd, step.def.Binding("command"), step.def.RuleName(), err)
		b.logOutput(res, false)
		// Preserve remote action result and error.
		ar, _ := step.cmd.ActionResult()
		step.cmd.SetRemoteFallbackResult(ar, err)
		err = b.execLocal(ctx, step)
		if err != nil {
			return err
		}
	}
	return err
}

func (b *Builder) tryFastStep(ctx context.Context, step, fastStep *Step, cacheCheck bool) error {
	fctx, fastSpan := trace.NewSpan(ctx, "fast-deps-run")
	err := b.runRemoteStep(fctx, fastStep, cacheCheck)
	fastSpan.Close(nil)
	if err == nil {
		return b.fastStepDone(ctx, step, fastStep)
	}
	if errors.Is(err, context.Canceled) {
		return err
	}
	if errors.Is(err, reapi.ErrBadPlatformContainerImage) {
		return err
	}
	step.metrics.DepsLogErr = true
	stats := b.stats.stats()
	nFastDeps := stats.FastDepsSuccess + stats.FastDepsFailed + 1
	if nFastDeps > 100 && (stats.FastDepsFailed+1)*100 > nFastDeps {
		// many fast-deps failure.
		// better to use scandeps to reduce retry by fast-deps failure.
		clog.Infof(ctx, "too many fast-deps failure detected %d/%d", stats.FastDepsFailed+1, stats.FastDepsSuccess)
		b.disableFastDeps.CompareAndSwap(nil, "too many fast-deps failure")
	}

	if experiments.Enabled("no-fast-deps-fallback", "fast-deps %s failed", step) {
		return fmt.Errorf("fast-deps failed: %w", err)
	}
	return errDepsLog
}

func (b *Builder) fastStepDone(ctx context.Context, step, fastStep *Step) error {
	step.metrics = fastStep.metrics
	step.metrics.DepsLog = true
	res := cmdOutput(ctx, cmdOutputResultSUCCESS, fastStep.cmd, step.def.Binding("command"), step.def.RuleName(), nil)
	if res != nil {
		b.logOutput(res, step.cmd.Console)
		if experiments.Enabled("fail-on-stdouterr", "step %s emit stdout/stderr", step) {
			return fmt.Errorf("%s emit stdout/stderr", step)
		}
	}
	return nil
}

func (b *Builder) runRemoteStep(ctx context.Context, step *Step, cacheCheck bool) error {
	if len(step.cmd.Platform) == 0 {
		return fmt.Errorf("no remote available (missing platform property)")
	}
	if cacheCheck {
		err := b.execRemoteCache(ctx, step)
		if err == nil {
			return nil
		}
		clog.Infof(ctx, "cmd cache miss: %v", err)
	}
	if !b.reExecEnable {
		return errRemoteExecDisabled
	}
	return b.execRemote(ctx, step)
}
