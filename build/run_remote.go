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
	"go.chromium.org/build/siso/reapi"
	"go.chromium.org/build/siso/reapi/digest"
	"go.chromium.org/build/siso/scandeps"
)

var errNeedPreproc = errors.New("need to preproc")
var errRemoteExecDisabled = errors.New("remote exec disabled")

// ToomanyFallbackError is an error when it detects too many fallback, exceeding the limit.
type TooManyFallbackError struct {
	Action    digest.Digest
	Fallbacks int64
	Limit     int64
	Err       error
}

func (e TooManyFallbackError) Error() string {
	if e.Limit == 0 {
		return fmt.Sprintf("%s no-fallback: %v", e.Action, e.Err)
	}
	return fmt.Sprintf("%s fallback %d exceeds limit %d: %v", e.Action, e.Fallbacks, e.Limit, e.Err)
}

func (b *Builder) remoteClaimFallbackIfAllowed(ctx context.Context, step *Step, err error) (bool, error) {
	output := step.outputPaths[0]
	var fallbackReported bool
	fallbackReport := func(category string) {
		if fallbackReported {
			return
		}
		clog.Errorf(ctx, "%s: remote-exec %s failed, fallback to local: %q siso_config=%q, gn_target=%q: %v", category, step.cmd.ActionDigest(), output, step.def.RuleName(), step.def.Binding("gn_target"), err)
		fallbackReported = true
	}
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		return false, err
	}
	if errors.Is(err, reapi.ErrBadPlatformContainerImage) {
		return false, fmt.Errorf("remote-exec %s failed: %w", step.cmd.ActionDigest(), err)
	}
	switch errCode := status.Code(err); errCode {
	case codes.PermissionDenied,
		codes.Unauthenticated,
		codes.InvalidArgument,
		codes.FailedPrecondition:
		return false, fmt.Errorf("remote-exec %s failed: %w", step.cmd.ActionDigest(), err)
	case codes.Canceled:
		if errors.Is(ctx.Err(), context.Canceled) {
			return false, fmt.Errorf("remote-exec %s canceled: %w", step.cmd.ActionDigest(), err)
		}
	case codes.Unknown:
	default:
		fallbackReport(fmt.Sprintf("fallback-on-%s", errCode))
	}
	if errors.Is(err, context.DeadlineExceeded) {
		fallbackReport("fallback-on-deadline-exceeded")
	}
	if errors.Is(err, errNotRelocatable) {
		clog.Errorf(ctx, "not relocatable: %v", err)
		return false, fmt.Errorf("remote-exec %s failed: %w", step.cmd.ActionDigest(), err)
	}
	if errors.Is(err, errNotInsideWorkspace) {
		clog.Errorf(ctx, "not remote executable: %v\nUse `use_system_inputs` or put them inside workspace", err)
		return false, fmt.Errorf("remote-exec %s failed: %w", step.cmd.ActionDigest(), err)
	}
	var eerr execute.ExitError
	if errors.As(err, &eerr) {
		// report compile fail early to developers.
		// If user runs on non-terminal or user sets a
		// non-default -k, then it implies that they want to
		// keep going as much as possible and
		// correct result, rather than fast feedback.
		preferNoFallbackOnExecErr := len(step.cmd.Stdout())+len(step.cmd.Stderr()) > 0 && b.failures.allowed == 1
		switch {
		case eerr.ExitCode == 137:
			// we still see unexpected SIGKILL (OOM?)
			fallbackReport("fallback-on-SIGKILL")
		case experiments.Enabled("fallback-on-exec-error", "remote exec %s failed: %v", step.cmd.ActionDigest(), err):
		case preferNoFallbackOnExecErr:
			return false, fmt.Errorf("remote-exec %s failed: %w", step.cmd.ActionDigest(), err)
		}
		fallbackReport(fmt.Sprintf("fallback-on-exec-error-%d", eerr.ExitCode))
	}
	if errors.Is(err, scandeps.ErrTooSlow) {
		fallbackReport("fallback-on-scandeps-slow")
	}
	if errors.Is(err, errFlushOutput) {
		fallbackReport("fallback-on-output-error")
	}
	fallbackReport("fallback-on-other")
	if n := b.numFallback.Add(1); n >= b.maxFallbackAllowed {
		return false, fmt.Errorf("remote-exec %w", TooManyFallbackError{
			Action:    step.cmd.ActionDigest(),
			Fallbacks: n,
			Limit:     b.maxFallbackAllowed,
			Err:       err,
		})
	}
	return true, nil
}

func (b *Builder) setupFallback(ctx context.Context, step *Step, err error) {
	b.progressStepFallback(step)
	step.metrics.IsRemote = false
	step.metrics.Fallback = true
	res := cmdOutput(ctx, cmdOutputResultFALLBACK, step.cmd, b.reapiclient.Instance(), step.def.Binding("command"), step.def.RuleName(), err)
	b.logOutput(res, false)
	// Preserve remote action result and error.
	ar, _ := step.cmd.ActionResult()
	step.cmd.SetRemoteFallbackResult(ar, err)
	step.cmd.AuxiliaryOutputDigests = nil
}

// runRemote runs step with using remote apis.
//
//  1. for initial steps of startLocal, run locally.
//  2. Check remote cache with deps log if available.
//  3. If local resource is idle, run locally.
//  4. Otherwise, try running a remote execution with deps log.
//  5. If it failed, it will retry a remote execution with deps scan.
//  6. If it still failed, it will fallback to local execution.
//
// - Before each remote exec, it checks remote cache before running.
// - The fallbacks can be disabled via experiment flags.
func (b *Builder) runRemote(ctx context.Context, step *Step) error {
	preprocErr := errNeedPreproc
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
		ok, ferr := b.remoteClaimFallbackIfAllowed(ctx, step, err)
		if !ok {
			return ferr
		}
		b.setupFallback(ctx, step, err)
		err = b.execLocal(ctx, step)
		if err != nil {
			return err
		}
	}
	return err
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
