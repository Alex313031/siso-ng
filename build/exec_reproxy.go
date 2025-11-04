// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	ppb "github.com/bazelbuild/reclient/api/proxy"
	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	log "github.com/golang/glog"

	"go.chromium.org/build/siso/execute"
	"go.chromium.org/build/siso/execute/reproxyexec"
	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/o11y/trace"
	"go.chromium.org/build/siso/reapi"
)

func (b *Builder) execReproxy(ctx context.Context, step *Step) error {
	ctx, span := trace.NewSpan(ctx, "exec-reproxy")
	defer span.Close(nil)
	clog.Infof(ctx, "exec reproxy %s", step.cmd.Desc)
	step.setPhase(stepInput)
	err := b.prepareLocalInputs(ctx, step)
	if err != nil && !experiments.Enabled("ignore-missing-local-inputs", "step %s missing inputs: %v", step, err) {
		return err
	}
	err = allowWriteOutputs(ctx, step.cmd)
	if err != nil {
		return err
	}
	step.cmd.RecordPreOutputs(ctx)
	phase := stepRemoteRun
	step.setPhase(phase.wait())
	err = b.reproxySema.Do(ctx, step.weight, func(ctx context.Context) error {
		started := time.Now()
		step.setPhase(phase)
		b.actionStarted(step)
		ctx = reapi.NewContext(ctx, &rpb.RequestMetadata{
			ActionId:                step.cmd.ID,
			ToolInvocationId:        b.id,
			CorrelatedInvocationsId: b.jobID,
			ActionMnemonic:          step.def.ActionName(),
			TargetId:                step.cmd.Outputs[0],
		})
		clog.Infof(ctx, "step state: remote exec (via reproxy)")
		maybeDisableLocalFallback(ctx, b, step)

		err := b.reproxyExec.Run(ctx, step.cmd)
		step.setPhase(stepOutput)
		ar, cached := step.cmd.ActionResult()
		if err == nil && !validateRemoteActionResult(ar) {
			clog.Errorf(ctx, "no outputs in action result. retry without cache lookup. b/350360391")
			step.cmd.SkipCacheLookup = true
			step.setPhase(stepRemoteRun)
			err = b.reproxyExec.Run(ctx, step.cmd)
			step.setPhase(stepOutput)
			ar, cached = step.cmd.ActionResult()
			if err == nil && !validateRemoteActionResult(ar) {
				clog.Errorf(ctx, "no outputs in action result again. b/350360391")
			}
		}
		switch ar.GetExecutionMetadata().GetWorker() {
		case reproxyexec.WorkerNameFallback:
			step.metrics.IsLocal = true
			step.metrics.Fallback = true
			fallbackResult, _ := step.cmd.RemoteFallbackResult()
			exitCode := -1
			if e := fallbackResult.GetExitCode(); e != 0 {
				exitCode = int(e)
			}
			res := cmdOutput(ctx, cmdOutputResultFALLBACK, step.cmd, step.def.Binding("command"), step.def.RuleName(), fmt.Errorf("fallback in reproxy exit=%d", exitCode))
			if stdout := fallbackResult.GetStdoutRaw(); len(stdout) > 0 {
				res.stdout = stdout
			}
			if stderr := fallbackResult.GetStderrRaw(); len(stderr) > 0 {
				res.stderr = stderr
			}
			b.logOutput(res, false)
		case reproxyexec.WorkerNameLocal, reproxyexec.WorkerNameRacingLocal:
			// TODO: Siso may want to have `racing`flag in the step metrics.
			step.metrics.IsLocal = true
		default:
			step.metrics.IsRemote = true
		}
		if cached {
			step.metrics.Cached = true
		}
		step.metrics.RunTime = IntervalMetric(time.Since(started))
		step.metrics.done(ctx, step, b.start)
		return err
	})
	if err != nil {
		return fmt.Errorf("reproxy error: %w", err)
	}
	// need to update deps for remote exec for deps=gcc with depsfile,
	// or deps=msvc with showIncludes
	if err = b.updateDeps(ctx, step); err != nil {
		return err
	}
	return b.outputs(ctx, step)
}

// allowWriteOutputs fixes the permissions of the output files if they are not writable.
// TODO: b/299227633 - Remove this workaround after Reproxy fixes the write operation.
func allowWriteOutputs(ctx context.Context, cmd *execute.Cmd) error {
	ctx, span := trace.NewSpan(ctx, "allow-write-outputs")
	defer span.Close(nil)
	for _, out := range cmd.Outputs {
		fname := filepath.Join(cmd.ExecRoot, out)
		fi, err := os.Lstat(fname)
		if errors.Is(err, fs.ErrNotExist) {
			// Do nothing if the file doesn't exist.
			continue
		} else if err != nil {
			// We don't know the filemode. So let it go.
			clog.Warningf(ctx, "failed to stat %s: %v", fname, err)
			continue
		}
		// The file needs to be writable. Otherwise writing the output file fails with permission denied.
		if fi.Mode()&0200 == 0 {
			err = os.Chmod(fname, fi.Mode()|0200)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func maybeDisableLocalFallback(ctx context.Context, b *Builder, step *Step) {
	// Manually override remote_local_fallback to remote when falback is disabled.
	// TODO: b/297807325 - Siso relies on Reclient metrics and monitoring at this moment.
	// CompileErrorRatioAlert checks remote failure/local success case. So it
	// needs to do local fallback on Reproxy side. However, all local executions
	// need to be handled at Siso layer.
	if !b.localFallbackEnabled() && strings.ToUpper(step.cmd.REProxyConfig.ExecStrategy) == ppb.ExecutionStrategy_REMOTE_LOCAL_FALLBACK.String() {
		if log.V(1) {
			clog.Infof(ctx, "overriding reproxy REMOTE_LOCAL_FALLBACK to REMOTE")
		}
		step.cmd.REProxyConfig.ExecStrategy = ppb.ExecutionStrategy_REMOTE.String()
	}
}
