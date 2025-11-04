// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"time"

	"cloud.google.com/go/logging"
	"google.golang.org/grpc/status"

	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/o11y/trace"
	"go.chromium.org/build/siso/reapi"
	"go.chromium.org/build/siso/toolsupport/msvcutil"
	"go.chromium.org/build/siso/ui"
)

// StepError is step execution error.
type StepError struct {
	Target string
	Cause  error
}

func (e StepError) Error() string {
	return e.Cause.Error()
}

func (e StepError) Unwrap() error {
	return e.Cause
}

// runStep runs a step.
//
//   - check if up-to-date. do nothing if so.
//   - expand inputs with step defs (except deps=gcc, msvc)
//   - run handler if set.
//   - setup RSP file.
//   - try cmd with deps log cache (fast deps)
//   - if failed, preproc runs deps command (e.g.clang -M)
//   - run cmd
//
// can control the flows with the experiment ids, defined in experiments.go.
func (b *Builder) runStep(ctx context.Context, step *Step) (err error) {
	step.startTime = time.Now()
	tc := trace.New(ctx, step.def.String())
	ctx = trace.NewContext(ctx, tc)
	spanName := stepSpanName(step.def)
	ctx, span := trace.NewSpan(ctx, "step:"+spanName)
	traceID, spanID := span.ID(b.projectID)
	ctx = clog.NewSpan(ctx, traceID, spanID, map[string]string{
		logLabelKeyID: step.def.String(),
	})
	logger := clog.FromContext(ctx)
	logger.Formatter = logFormat
	defer func(span *trace.Span) {
		if r := recover(); r != nil {
			panic(r) // re-throw panic, handled in *Builder.Build.
		}
		if errors.Is(err, context.Canceled) {
			span.Close(status.FromContextError(err).Proto())
			return
		}
		if err != nil {
			step.metrics.Err = true
			st, ok := status.FromError(err)
			if !ok {
				st = status.FromContextError(err)
			}
			span.Close(st.Proto())
		} else {
			span.Close(nil)
		}
		if !step.metrics.skip {
			step.endTime = time.Now()
			duration := step.endTime.Sub(step.startTime)
			step.metrics.Duration = IntervalMetric(duration)
			step.metrics.ActionEndTime = IntervalMetric(step.endTime.Sub(b.start))
			step.metrics.Err = err != nil
			stepLogEntry(ctx, logger, step, duration, err)
			b.recordMetrics(ctx, step.metrics)
			b.recordNinjaLogs(ctx, step)
			b.recordCloudMonitoringActionMetrics(ctx, step, err)
			b.stats.update(ctx, &step.metrics, step.cmd.Pure)
			b.finalizeTrace(ctx, tc)
			b.outputFailureSummary(ctx, step, err)
			b.outputFailedCommands(ctx, step, err)
		}
		// unref for GC to reclaim memory.
		step.cmd = nil
	}(span)

	stepManifest := newStepManifest(ctx, step.def)

	if !b.needToRun(ctx, step.def, stepManifest) {
		step.metrics.skip = true
		b.plan.completeStep(ctx, step)
		b.stats.update(ctx, &step.metrics, true)
		return nil
	}
	select {
	case <-ctx.Done():
		// interrupt during needToRun.
		return context.Cause(ctx)
	default:
	}

	step.init(ctx, b, stepManifest)
	description := stepDescription(step.def)
	prevStepOut := b.prevStepOut(ctx, step)
	stepStartLog(ctx, logger, step, description, spanName)
	step.metrics.init(ctx, b, step, step.startTime, prevStepOut)
	b.stepSpanInit(ctx, span, step, description, spanName, prevStepOut)

	ctx, span = trace.NewSpan(ctx, "run-step")
	defer span.Close(nil)

	if b.dryRun {
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		default:
		}
		ui.Default.Infof("%s\n", step.def.Binding("command"))
		b.plan.completeStep(ctx, step)
		return nil
	}

	b.progressStepStarted(step)
	defer b.progressStepFinished(step)

	step.setPhase(stepHandler)
	exited, err := b.handleStep(ctx, step)
	if err != nil {
		if !experiments.Enabled("keep-going-handle-error", "handle %s failed: %v", step, err) {
			res := cmdOutput(ctx, cmdOutputResultFAILED, step.cmd, step.def.Binding("command"), step.def.RuleName(), err)
			step.cmd.SetOutputResult(b.logOutput(res, step.cmd.Console))
			clog.Warningf(ctx, "Failed to exec(handle): %v", err)
			return fmt.Errorf("failed to run handler for %s: %w", step, err)
		}
	} else if exited {
		// store handler generated outputs to local disk.
		// better to upload to CAS, or store in fs_state?
		clog.Infof(ctx, "outputs[handler] %d", len(step.cmd.Outputs))
		err = b.hashFS.Flush(ctx, step.cmd.ExecRoot, step.cmd.Outputs)
		if err == nil {
			b.plan.completeStep(ctx, step)
			return nil
		}
		clog.Warningf(ctx, "handle step failure: %v", err)
	}
	err = b.setupRSP(ctx, step)
	if err != nil {
		res := cmdOutput(ctx, cmdOutputResultFAILED, step.cmd, step.def.Binding("command"), step.def.RuleName(), err)
		step.cmd.SetOutputResult(b.logOutput(res, step.cmd.Console))
		return fmt.Errorf("failed to setup rsp: %s: %w", step, err)
	}
	defer func() {
		if err != nil && !errors.Is(err, context.Canceled) {
			// force flush to disk
			ferr := b.hashFS.Flush(ctx, step.cmd.ExecRoot, []string{step.cmd.RSPFile})
			clog.Warningf(ctx, "failed to exec %v: preserve rsp=%s flush:%v", err, step.cmd.RSPFile, ferr)
			return
		}
		b.teardownRSP(ctx, step)
	}()

	// expand inputs to get full action inputs unless deps=gcc,msvc with main supported source files such as .c, .cc, .mm etc.
	// deps gcc,msvc for rust and cxx module compiles will still rely on `depsExpandInputs` instead of scandeps.
	depsExpandInputs(ctx, b, step)

	// BuildActionStarted is called just before remote exec calls or
	// local execution.
	defer b.actionFinished(ctx, step)
	runCmd := b.runStrategy(step)
	err = runCmd(ctx, step)
	clog.Infof(ctx, "done err=%v", err)
	if err != nil {
		if ctx.Err() != nil {
			err = fmt.Errorf("ctx err: %w: %w", ctx.Err(), err)
		}
		switch {
		case errors.Is(err, context.Canceled):
			// do nothing
			return err
		case errors.Is(err, reapi.ErrBadPlatformContainerImage):
			// RBE returns permission denied when
			// platform container image are not available
			// on RBE worker.
			res := cmdOutput(ctx, cmdOutputResultFAILED, step.cmd, step.def.Binding("command"), step.def.RuleName(), err)
			step.cmd.SetOutputResult(b.logOutput(res, step.cmd.Console))
		default:
			msgs := cmdOutput(ctx, cmdOutputResultFAILED, step.cmd, step.def.Binding("command"), step.def.RuleName(), err)
			step.cmd.SetOutputResult(b.logOutput(msgs, step.cmd.Console))
		}
		return StepError{
			Target: b.path.MaybeToWD(ctx, step.cmd.Outputs[0]),
			Cause:  err,
		}
	}

	res := cmdOutput(ctx, cmdOutputResultSUCCESS, step.cmd, step.def.Binding("command"), step.def.RuleName(), nil)
	if res != nil {
		step.cmd.SetOutputResult(b.logOutput(res, step.cmd.Console))
		if experiments.Enabled("fail-on-stdouterr", "step %s emit stdout/stderr", step) {
			return fmt.Errorf("%s emit stdout/stderr", step)
		}
	}
	if len(b.lastFailureTargets) > 0 {
		out := b.path.MaybeToWD(ctx, step.cmd.Outputs[0])
		if _, ok := b.lastFailureTargets[out]; ok {
			ui.Default.PrintLines(fmt.Sprintf(ui.SGR(ui.Green, "last failed target fixed: %s\n\n"), out))
		}
	}
	b.plan.completeStep(ctx, step)
	return nil
}

func (b *Builder) prevStepOut(ctx context.Context, step *Step) string {
	if step.prevStepOut == 0 {
		return ""
	}
	s, err := b.graph.TargetPath(ctx, step.prevStepOut)
	if err != nil {
		clog.Warningf(ctx, "failed to get target path: %v", err)
		return ""
	}
	return s
}

func stepStartLog(ctx context.Context, logger *clog.Logger, step *Step, description, spanName string) {
	logEntry := logger.Entry(logging.Info, description)
	logEntry.Labels = map[string]string{
		"id":          step.def.String(),
		"command":     step.def.Binding("command"),
		"description": description,
		"action":      step.def.ActionName(),
		"span_name":   spanName,
		"output0":     step.def.Outputs(ctx)[0],
	}
	logger.Log(logEntry)
}

func (b *Builder) stepSpanInit(ctx context.Context, span *trace.Span, step *Step, description, spanName, prevStepOut string) {
	span.SetAttr("ready_time", time.Since(step.readyTime).Milliseconds())
	span.SetAttr("prev", step.prevStepID)
	span.SetAttr("prev_out", prevStepOut)
	span.SetAttr("queue_time", time.Since(step.queueTime).Milliseconds())
	span.SetAttr("queue_size", step.queueSize)
	span.SetAttr("build_id", b.id)
	span.SetAttr("id", step.def.String())
	span.SetAttr("command", step.def.Binding("command"))
	span.SetAttr("description", description)
	span.SetAttr("action", step.def.ActionName())
	span.SetAttr("span_name", spanName)
	span.SetAttr("output0", step.def.Outputs(ctx)[0])
	if next := step.def.Next(); next != nil {
		span.SetAttr("next_id", step.def.Next().String())
	}
	if step.metrics.GNTarget != "" {
		span.SetAttr("gn_target", step.metrics.GNTarget)
	}
	span.SetAttr(logLabelKeyBacktraces, stepBacktraces(ctx, step))
}

func (b *Builder) handleStep(ctx context.Context, step *Step) (bool, error) {
	ctx, span := trace.NewSpan(ctx, "handle-step")
	defer span.Close(nil)
	started := time.Now()
	err := step.def.Handle(ctx, step.cmd)
	if err != nil {
		return false, err
	}
	result, _ := step.cmd.ActionResult()
	exited := result != nil
	if exited {
		step.metrics.ActionStartTime = IntervalMetric(started.Sub(b.start))
		step.metrics.RunTime = IntervalMetric(time.Since(started))
		step.metrics.NoExec = true
	}
	return exited, nil
}

func (b *Builder) outputFailureSummary(ctx context.Context, step *Step, err error) {
	if err == nil || b.failureSummaryWriter == nil || step.cmd == nil {
		return
	}
	stat := b.stats.stats()
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "[%d/%d] %s\n", stat.Done-stat.Skipped, stat.Total-stat.Skipped, step.cmd.Desc)
	fmt.Fprintf(&buf, "%s\n", strings.Join(step.cmd.Args, " "))
	stderr := step.cmd.Stderr()
	stdout := step.cmd.Stdout()
	if step.cmd.Deps == "msvc" {
		// cl.exe, clang-cl shows included file to stderr
		// but RBE merges stderr into stdout...
		_, stdout = msvcutil.ParseShowIncludes(stdout)
		_, stderr = msvcutil.ParseShowIncludes(stderr)
	}
	if len(stderr) > 0 {
		fmt.Fprint(&buf, ui.StripANSIEscapeCodes(string(stderr)))
	}
	if len(stdout) > 0 {
		fmt.Fprint(&buf, ui.StripANSIEscapeCodes(string(stdout)))
	}
	fmt.Fprintf(&buf, "%v\n", err)
	_, err = b.failureSummaryWriter.Write(buf.Bytes())
	if err != nil {
		clog.Warningf(ctx, "failed to write failure_summary: %v", err)
	}
}

func (b *Builder) outputFailedCommands(ctx context.Context, step *Step, err error) {
	if err == nil || b.failedCommandsWriter == nil || step.cmd == nil {
		return
	}
	var buf bytes.Buffer
	// rspfile is preserved when failed, so assume it exists.
	comment := "#"
	newline := "\n"
	if runtime.GOOS == "windows" {
		comment = "rem"
		newline = "\r\n"
	}
	fmt.Fprintf(&buf, "%s step_id=%s%s", comment, step.String(), newline)
	fmt.Fprintf(&buf, "%s %s%s", comment, step.cmd.Desc, newline)
	fmt.Fprintf(&buf, "%s%s", step.def.Binding("command"), newline)
	fmt.Fprint(&buf, newline)
	_, err = b.failedCommandsWriter.Write(buf.Bytes())
	if err != nil {
		clog.Warningf(ctx, "failed to write failed commands: %v", err)
	}
}
