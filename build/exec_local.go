// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	log "github.com/golang/glog"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"go.chromium.org/build/siso/execute"
	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/o11y/trace"
	"go.chromium.org/build/siso/reapi/digest"
)

func (b *Builder) execLocal(ctx context.Context, step *Step) error {
	ctx, span := trace.NewSpan(ctx, "exec-local")
	defer span.Close(nil)
	clog.Infof(ctx, "exec local %s", step.cmd.Desc)

	step.setPhase(stepInput)
	err := b.prepareLocalInputs(ctx, step)
	if err != nil && !experiments.Enabled("ignore-missing-local-inputs", "step %s missing inputs: %v", step, err) {
		return err
	}
	step.cmd.RecordPreOutputs(ctx)

	stateMessage := "local exec"
	sema := b.localSema
	pool := step.def.Binding("pool")
	step.cmd.Console = pool == "console"
	if s, ok := b.poolSemas[pool]; ok {
		sema = s
		stateMessage += " (pool=" + pool + ")"
	}
	phase := stepLocalRun
	var executor execute.Executor = b.localExec
	logLocalExec := b.logLocalExec
	switch sandbox := step.def.Binding("sandbox"); sandbox {
	// TODO(crbug.com/420752996): add sandbox supports
	case "":
		enableTrace := experiments.Enabled("file-access-trace", "enable file-access-trace")
		if enableTrace {
			// check impure explicitly set in config,
			// rather than step.cmd.Pure.
			// step.cmd.Pure may be false when config is not set
			// for the step too, but we want to disable
			// file-access-trace only for the step with impure=true.
			// http://b/261655377 errorprone_plugin_tests: too slow under strace?
			impure := step.def.Binding("impure") == "true"
			if impure {
				clog.Warningf(ctx, "disable file-access-trace by impure")
			} else {
				traceExecutor, err := newFileTraceExecutor(ctx, b, executor)
				if err != nil {
					return fmt.Errorf("unable to perform file-access-trace: %w", err)
				}
				executor = traceExecutor
				logLocalExec = traceExecutor.logLocalExec
			}
		} else if log.V(1) {
			clog.Warningf(ctx, "unable to use file-access-trace")
		}
	default:
		clog.Warningf(ctx, "unsupported sandbox %q", sandbox)
	}

	// native integration is handled by exec_reproxy.go
	if step.def.Binding("use_remote_exec_wrapper") != "" {
		// no sandbox and no need to file trace for gomacc/rewwapper.
		stateMessage = "remote exec wrapper"
		phase = stepREWrapperRun
		sema = b.rewrapSema
	}
	if phase == stepLocalRun && step.metrics.Fallback {
		phase = stepFallbackRun
		stateMessage = "local exec [fallback]"
	}

	queueTime := time.Now()
	var dur time.Duration
	step.setPhase(phase.wait())
	err = sema.Do(ctx, step.weight, func(ctx context.Context) error {
		clog.Infof(ctx, "step state: %s", stateMessage)
		step.setPhase(phase)
		if step.cmd.Console {
			b.progress.startConsoleCmd(step.cmd)
		}
		started := time.Now()
		// local exec might be called as fallback.
		b.actionStarted(step)
		err := executor.Run(ctx, step.cmd)
		dur = time.Since(started)
		step.setPhase(stepOutput)
		if step.cmd.Console {
			b.progress.finishConsoleCmd()
		}
		step.metrics.IsLocal = true
		result, cached := step.cmd.ActionResult()
		if cached {
			step.metrics.Cached = true
		}
		if result != nil {
			if result.ExecutionMetadata == nil {
				result.ExecutionMetadata = &rpb.ExecutedActionMetadata{}
			}
			result.ExecutionMetadata.QueuedTimestamp = timestamppb.New(queueTime)
			result.ExecutionMetadata.WorkerStartTimestamp = timestamppb.New(started)
		}
		step.metrics.RunTime = IntervalMetric(time.Since(started))
		step.metrics.done(ctx, step, b.start)
		return err
	})
	if !errors.Is(err, context.Canceled) {
		lerr := logLocalExec(ctx, step, dur)
		if err == nil {
			err = lerr
		}
	}
	if err != nil {
		return err
	}
	b.cacheWrite(ctx, step)
	err = b.updateDeps(ctx, step)
	if err != nil {
		return err
	}
	return b.checkLocalOutputs(ctx, step)
	// no need to call b.outputs, as all outputs are already on disk
	// so no need to flush.
}

// Uploads and sets local execution result in RE if builder is trusted
// Note: currently does not work with layered cache and blocks on digest calculation
// Note: local step does not fail if cache-write fails but error and metrics are logged
func (b *Builder) cacheWrite(ctx context.Context, step *Step) {
	// Cache write must be enabled and step must have pure inputs/outputs
	if b.reapiclient == nil || !b.reCacheEnableWrite || !step.cmd.Pure {
		return
	}

	// Upload only remotable steps
	if !b.allowRemote(step) {
		return
	}

	err := func() error {
		ctx, span := trace.NewSpan(ctx, "cache-write")
		defer span.Close(nil)
		phase := stepCacheWrite
		step.setPhase(phase)
		clog.Infof(ctx, "step state: cache write started %s", step.cmd.Desc)

		// Action digests are lazily computed for local so they are not available at this point
		cmd := step.cmd
		result, _ := cmd.ActionResult()
		ds := digest.NewStore()
		actionDigest, err := cmd.Digest(ctx, ds)

		if err != nil {
			clog.Warningf(ctx, "failed to compute digest for trusted local upload: %v", err)
			return err
		}

		var metadata *rpb.ExecutedActionMetadata
		if md := result.GetExecutionMetadata(); md != nil {
			metadata = proto.CloneOf(md)
			// don't store auxiliary metadata
			// as buildfarm can't accept unknown auxiliary metadata.
			metadata.AuxiliaryMetadata = nil
		}

		// Create new ActionResult to not mutate cmd result
		// We need to unset StderrRaw, StdoutRaw, and populate OutputFiles
		result = &rpb.ActionResult{
			OutputFiles:       result.GetOutputFiles(),
			OutputSymlinks:    result.GetOutputSymlinks(),
			OutputDirectories: result.GetOutputDirectories(),
			ExitCode:          result.GetExitCode(),
			StdoutRaw:         result.GetStdoutRaw(),
			StderrRaw:         result.GetStderrRaw(),
			StdoutDigest:      result.GetStdoutDigest(),
			StderrDigest:      result.GetStderrDigest(),
			ExecutionMetadata: metadata,
		}

		// Retrieve and compute output digests from HashFS on the action
		hashFS := b.hashFS
		outputEntries, err := hashFS.Entries(ctx, cmd.ExecRoot, cmd.AllOutputs())
		if err != nil {
			return err
		}

		// Convert rawStdout to digest since RE spec v2 prohibits inlining
		if len(result.GetStdoutRaw()) != 0 && result.GetStdoutDigest() == nil {
			stdoutDigest := digest.FromBytes("stdout", result.GetStdoutRaw())
			result.StdoutDigest = stdoutDigest.Digest().Proto()
			ds.Set(stdoutDigest)
		}
		result.StdoutRaw = nil

		// Convert rawStderr to digest since RE spec v2 prohibits inlining
		if len(result.GetStderrRaw()) != 0 && result.GetStderrDigest() == nil {
			stderrDigest := digest.FromBytes("stderr", result.GetStderrRaw())
			result.StderrDigest = stderrDigest.Digest().Proto()
			ds.Set(stderrDigest)
		}
		result.StderrRaw = nil

		// Set the outputs on the result
		execute.ResultFromEntries(ctx, result, cmd.Dir, outputEntries)
		for _, entry := range outputEntries {
			ds.Set(entry.Data)
		}

		step.setPhase(phase.wait())
		err = b.cacheSema.Do(ctx, func(ctx context.Context) error {
			step.setPhase(phase)
			// Upload all collected output data, input data, and action itself
			_, err = b.reapiclient.UploadAll(ctx, ds)
			if err != nil {
				return err
			}
			// Now set the action result in RE
			return b.reapiclient.UpdateActionResult(ctx, actionDigest, result)
		})
		return err
	}()
	if err == nil {
		step.metrics.CacheWrite = true
		b.progressStepCacheWrite(step)
	} else {
		step.metrics.CacheWriteErr = true
		clog.Warningf(ctx, "cache write failed %s: %v", step.cmd.Desc, err)
	}
}

func (b *Builder) prepareLocalInputs(ctx context.Context, step *Step) error {
	ctx, span := trace.NewSpan(ctx, "prepare-local-inputs")
	defer span.Close(nil)
	inputs := step.cmd.AllInputs()
	span.SetAttr("inputs", len(inputs))
	start := time.Now()
	if log.V(1) {
		clog.Infof(ctx, "prepare-local-inputs %d", len(inputs))
	}
	err := b.hashFS.Flush(ctx, step.cmd.ExecRoot, inputs)
	clog.Infof(ctx, "prepare-local-inputs %d %s: %v", len(inputs), time.Since(start), err)
	// now, all inputs are expected to be on disk.
	// for reproxy and local, no need to scan deps.
	// but need to remove missing inputs from cmd.Inputs
	// because we'll record header inputs for deps=msvc in deps log.
	// TODO: b/322712783 - minimize local disk check.
	if step.cmd.Deps == "msvc" {
		// we need to check this against local disk, not hashfs.
		// because command may add/remove files that are not
		// known in ninja build graph.
		inputs = b.hashFS.ForgetMissings(ctx, step.cmd.ExecRoot, step.cmd.Inputs)
	} else {
		// if deps is not "msvc", just check against hashfs.
		inputs = b.hashFS.Availables(ctx, step.cmd.ExecRoot, step.cmd.Inputs)
	}
	if len(inputs) != len(step.cmd.Inputs) {
		clog.Infof(ctx, "deps remove missing inputs %d -> %d", len(step.cmd.Inputs), len(inputs))
		step.cmd.Inputs = inputs
	}
	return err
}

// checkLocalOutputs checks if all outputs are on local disk.
// If not, it returns error.
// It ignores missing outputs added by siso config.
func (b *Builder) checkLocalOutputs(ctx context.Context, step *Step) error {
	ctx, span := trace.NewSpan(ctx, "capture-local-outputs")
	defer span.Close(nil)
	span.SetAttr("outputs", len(step.cmd.Outputs))
	result, _ := step.cmd.ActionResult()
	if result.GetExitCode() != 0 {
		return nil
	}
	if step.def.Binding("phony_output") != "" {
		clog.Infof(ctx, "phony_output. no check output files %q", step.cmd.Outputs)
		return nil
	}

	defOutputs := step.def.Outputs(ctx)

	for _, out := range step.cmd.Outputs {
		_, err := step.cmd.HashFS.Stat(ctx, step.cmd.ExecRoot, out)
		if err != nil {
			required := slices.Contains(defOutputs, out)
			if !required {
				clog.Warningf(ctx, "ignore missing outputs %s: %v", out, err)
				continue
			}
			if experiments.Enabled("ignore-missing-outputs", "") {
				b.hashFS.AddMissingOutput(ctx, step.cmd.ExecRoot, out)
				clog.Warningf(ctx, "ignore missing outputs %s: %v", out, err)
				continue
			}
			return fmt.Errorf("missing local outputs %s: %w", out, err)
		}
	}
	// don't set result.OutputFiles etc to lazily calculate digest
	// for outputs. b/311312613
	return nil
}

func (b *Builder) logLocalExec(ctx context.Context, step *Step, dur time.Duration) error {
	command := step.def.Binding("command")
	if len(command) > 256 {
		command = command[:256] + " ..."
	}
	allOutputs := step.cmd.AllOutputs()
	var output string
	if len(allOutputs) > 0 {
		output = allOutputs[0]
	}
	var buf bytes.Buffer
	fmt.Fprintf(&buf, `cmd: %s pure:%t/unknown restat:%t %s
action: %s %s
command: %q %d

`,
		step, step.cmd.Pure, step.cmd.Restat, dur,
		step.cmd.ActionName, output,
		command, dur.Milliseconds())
	_, err := b.localexecLogWriter.Write(buf.Bytes())
	if err != nil {
		clog.Warningf(ctx, "failed to log localexec: %v", err)
	}
	return nil
}
