// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"time"

	log "github.com/golang/glog"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/blob"
	"go.chromium.org/build/siso/execute"
	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/o11y/trace"
	"go.chromium.org/build/siso/path"
)

func (b *Builder) execLocal(ctx context.Context, step *Step) (retErr error) {
	ctx, span := trace.NewSpan(ctx, "exec-local")
	defer span.Close(nil)
	if b.allowTwoPhaseCaching(step) {
		// TODO: no two phase caching when rewrapper is used?
		ok, err := b.twoPhaseCachingLookup(ctx, step)
		if ok {
			return err
		}
	}
	ensureExpanded(ctx, b, step)
	sandbox, sandboxOption := b.selectSandbox(ctx, step)
	clog.Infof(ctx, "exec local %s sandbox:%s", step.cmd.Desc, sandbox)
	step.setPhase(stepInput)
	err := b.prepareLocalInputs(ctx, step)
	if err != nil && !experiments.Enabled("ignore-missing-local-inputs", "step %s missing inputs: %v", step, err) {
		return err
	}
	if experiments.Enabled("disable-ensure-output-dirs", "") {
		// disable-ensure-output-dirs doesn't create out dirs during
		// scheduling, so that it needs to make sure output dirs
		// before each local exec.
		err := b.prepareLocalOutdirs(ctx, step)
		if err != nil {
			return err
		}
	}
	if err := b.resetDirOutputs(ctx, step); err != nil {
		return err
	}

	stateMessage := "local exec"
	var stateMessagePrefix string
	sema := b.localSema
	pool := step.def.Binding("pool")
	step.cmd.Console = pool == "console"
	if s, ok := b.poolSemas[pool]; ok {
		sema = s
		stateMessage += " (pool=" + pool + ")"
	}
	phase := stepLocalRun
	var executor execute.Executor = b.localExec

	switch sandbox {
	case "nsjail":
		stateMessagePrefix = "nsjail "
		nsjailExecutor, err := newNSJailExecutor(ctx, b, executor, sandboxOption)
		if err != nil {
			return fmt.Errorf("unable to perform nsjail: %w", err)
		}
		executor = nsjailExecutor
		defer func() {
			if retErr != nil {
				clog.Warningf(ctx, "failed to run nsjail: %v", retErr)
				return
			}
			err := nsjailExecutor.Close()
			if err != nil {
				retErr = fmt.Errorf("failed to cleanup nsjail: %w", err)
			}
		}()
		step.metrics.Sandbox = true
		// enforceDepfileOnlyPromotes defaults to true for an nsjail-sandboxed action
		if sandboxOption["enforce_depfile_only_promotes"] != "false" {
			step.enforceDepfileOnlyPromotes = true
		}
	case "landlock":
		landlockExecutor := newLandlockExecutor(executor, sandboxOption)
		executor = landlockExecutor
		step.metrics.Sandbox = true
		// enforceDepfileOnlyPromotes defaults to true for an landlock-sandboxed action
		if sandboxOption["enforce_depfile_only_promotes"] != "false" {
			step.enforceDepfileOnlyPromotes = true
		}
	case "file-access-trace":
		traceExecutor, err := newFileTraceExecutor(ctx, b, executor)
		if err != nil {
			return fmt.Errorf("unable to perform file-access-trace: %w", err)
		}
		executor = traceExecutor
		// Normally, file-access-trace doesn't do any enforcement. But it's the more
		// convenient sandbox to use in tests, so we allow enabling depfile enforcement.
		if sandboxOption["enforce_depfile_only_promotes"] == "true" {
			step.enforceDepfileOnlyPromotes = true
		}
	case "":
		// no sandbox. ignore
	default:
		return fmt.Errorf("unsupported sandbox %q", sandbox)
	}

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
	var started time.Time
	var dur time.Duration
	step.setPhase(phase.wait())
	err = sema.Do(ctx, step.weight, func(ctx context.Context) error {
		clog.Infof(ctx, "step state: %s", stateMessagePrefix+stateMessage)
		step.setPhase(phase)
		if step.cmd.Console {
			b.progress.startConsoleCmd(step.cmd)
		}
		started = time.Now()
		// local exec might be called as fallback.
		b.actionStartedTime(step, started)
		step.cmdRunTime = started
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
		}
		return err
	})
	if !errors.Is(err, context.Canceled) {
		logLocalExec := b.logLocalExec
		if le, ok := executor.(interface {
			logLocalExec(context.Context, *Step, time.Duration) error
		}); ok {
			logLocalExec = le.logLocalExec
		}
		lerr := logLocalExec(ctx, step, dur)
		if err == nil {
			err = lerr
		}
	}
	// Beyond this point, we should be marking the step done.  Do that as we leave the function.
	defer func() {
		if !started.IsZero() {
			span.Add(ctx, trace.SpanData{
				Name:  spanExecLocalRun,
				Start: started,
				End:   time.Now(),
			})
		}
		step.metrics.done(ctx, b, step, b.start)
	}()
	if err != nil {
		return err
	}
	err = b.updateDeps(ctx, step)
	if err != nil {
		return err
	}
	err = b.checkLocalOutputs(ctx, step)
	if err != nil {
		return err
	}
	if ctx.Err() == nil {
		// check allowCacheWrite after tapCanonicalCmd.
		// step.cmd.Pure may have been set true in updateDeps,
		// but set it in tapCanonicalizeCmd.
		if step.metrics.TwoPhaseCachingKey != "" {
			if experiments.Enabled("expand-phony-trigger-inputs", "") {
				// ensureExpanded populated step.cmd.Inputs with ExpandedInputs
				// (including order-only and any over-specified trigger inputs)
				// to materialize local inputs before execution. Clear it before
				// tapCanonicalizeCmd (which appends tapData.Reads to cmd.Inputs)
				// so the Phase 2 Action key records only inputs actually read
				// during execution.
				step.cmd.Inputs = nil
			}
			err := b.tapCanonicalizeCmd(ctx, step.cmd)
			if err != nil {
				if !errors.Is(err, errNoTapData) {
					clog.Errorf(ctx, "two phase caching: canonicalize cmd %v", err)
				} else if b.reCacheEnableWrite {
					clog.Warningf(ctx, "two phase caching: canonicalize cmd %v", err)
				}
			} else if b.allowCacheWrite(step) {
				err := b.cacheWrite(ctx, step)
				if errors.Is(err, errNoCacheWrite) {
					clog.Infof(ctx, "two phase cache: write ignored %q: %v", step.metrics.TwoPhaseCachingKey, err)
				} else if err != nil {
					clog.Warningf(ctx, "two phase cache: write failed %q: %v", step.metrics.TwoPhaseCachingKey, err)
				} else {
					err := b.twoPhaseCaching.Add(ctx, step.metrics.TwoPhaseCachingKey, step)
					if err != nil {
						clog.Warningf(ctx, "two phase caching: add %v", err)
					}
				}
			}
		} else if b.allowCacheWrite(step) {
			err := b.cacheWrite(ctx, step)
			if errors.Is(err, errNoCacheWrite) {
				clog.Infof(ctx, "cache write ignored: %v", err)
			} else if err != nil {
				clog.Warningf(ctx, "cache write failed: %v", err)
			}
		}
	}
	return nil
	// no need to call b.outputs, as all outputs are already on disk
	// so no need to flush.
}

func (b *Builder) selectSandbox(ctx context.Context, step *Step) (string, map[string]string) {
	sandbox := step.def.Sandbox()
	if sandbox["backend"] != "" {
		return sandbox["backend"], sandbox
	}
	enableTrace := experiments.Enabled("file-access-trace", "enable file access-trace")
	if !enableTrace {
		if log.V(1) {
			clog.Warningf(ctx, "unable to use file-access-trace")
		}
		return "", nil
	}
	// check impure explicitly set in config,
	// rather than step.cmd.Pure.
	// step.cmd.Pure may be false when config is not set
	// for the step too, but we want to disable
	// file-access-trace only for the step with impure=true.
	// http://b/261655377 errorprone_plugin_tests: too slow under strace?
	impure := step.def.Binding("impure") == "true"
	if impure {
		clog.Warningf(ctx, "disable file-access-trace by impure")
		return "", nil
	}
	return "file-access-trace", nil
}

func (b *Builder) allowCacheWrite(step *Step) bool {
	// Cache write must be enabled
	if b.reapiclient == nil || !b.reCacheEnableWrite {
		return false
	}
	// step must have pure inputs/outputs
	if !step.cmd.Pure {
		return false
	}
	// Upload if remotable steps or two phase caching
	return b.allowRemote(step) || b.allowTwoPhaseCaching(step)
}

var errNoCacheWrite = errors.New("no cache write")

// Uploads and sets local execution result in RE if builder is trusted
// Note: currently does not work with layered cache and blocks on digest calculation
// Note: local step does not fail if cache-write fails but error and metrics are logged
func (b *Builder) cacheWrite(ctx context.Context, step *Step) error {
	if ctx.Err() != nil {
		return fmt.Errorf("%w: %w", errNoCacheWrite, context.Cause(ctx))
	}
	if !b.allowCacheWrite(step) {
		return errNoCacheWrite
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
		ds := blob.NewStore()
		actionDigest, err := cmd.Digest(ctx, ds)

		if err != nil {
			clog.Warningf(ctx, "failed to compute digest for trusted local upload: %v", err)
			return err
		}
		step.metrics.Digest = actionDigest.String()

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

			// deprecated as of v2.1
			OutputFileSymlinks:      result.GetOutputFileSymlinks(),      //nolint:staticcheck // existing deprecation
			OutputDirectorySymlinks: result.GetOutputDirectorySymlinks(), //nolint:staticcheck // existing deprecation

			ExitCode:          result.GetExitCode(),
			StdoutRaw:         result.GetStdoutRaw(),
			StderrRaw:         result.GetStderrRaw(),
			StdoutDigest:      result.GetStdoutDigest(),
			StderrDigest:      result.GetStderrDigest(),
			ExecutionMetadata: metadata,
		}

		// Convert rawStdout to digest since RE spec v2 prohibits inlining
		if len(result.GetStdoutRaw()) != 0 && result.GetStdoutDigest() == nil {
			stdoutDigest := blob.FromBytes(b.hashFS.DigestFunction(), "stdout", result.GetStdoutRaw())
			result.StdoutDigest = stdoutDigest.Digest().Proto()
			if log.V(1) {
				clog.Infof(ctx, "stdout digest %s", stdoutDigest.Digest())
			}
			ds.Set(stdoutDigest)
		}
		result.StdoutRaw = nil

		// Convert rawStderr to digest since RE spec v2 prohibits inlining
		if len(result.GetStderrRaw()) != 0 && result.GetStderrDigest() == nil {
			stderrDigest := blob.FromBytes(b.hashFS.DigestFunction(), "stderr", result.GetStderrRaw())
			result.StderrDigest = stderrDigest.Digest().Proto()
			if log.V(1) {
				clog.Infof(ctx, "stderr digest %s", stderrDigest.Digest())
			}
			ds.Set(stderrDigest)
		}
		result.StderrRaw = nil

		// Populate output files (and a real tree per directory output) from
		// the command's typed outputs, recording their blobs in ds.
		if err := cmd.SetResultOutputs(ctx, result, ds); err != nil {
			return fmt.Errorf("set result outputs: %w", err)
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
			err := b.reapiclient.UpdateActionResult(ctx, actionDigest, result)
			if err != nil {
				clog.Warningf(ctx, "failed to write action result: %v\n%s", err, result)
				return fmt.Errorf("update action result for %s: %w", actionDigest, err)
			}
			return nil
		})
		return err
	}()
	if err == nil {
		step.metrics.CacheWrite = true
		b.progressStepCacheWrite(step)
	} else {
		step.metrics.CacheWriteErr = true
	}
	return err
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
	_, mspan := trace.NewSpan(ctx, spanMaterializeInputs)
	err := b.hashFS.Flush(ctx, step.cmd.WorkspaceRoot, inputs)
	mspan.Close(nil)
	clog.Infof(ctx, "prepare-local-inputs %d %s: %v", len(inputs), time.Since(start), err)
	if errors.Is(err, context.Canceled) {
		return err
	}
	// now, all inputs are expected to be on disk.
	// for reproxy and local, no need to scan deps.
	// but need to remove missing inputs from cmd.Inputs
	// because we'll record header inputs for deps=msvc in deps log.
	// TODO: b/322712783 - minimize local disk check.
	if step.cmd.Deps == "msvc" {
		// we need to check this against local disk, not hashfs.
		// because command may add/remove files that are not
		// known in ninja build graph.
		inputs = b.hashFS.ForgetMissings(ctx, step.cmd.WorkspaceRoot, step.cmd.Inputs)
	} else {
		// if deps is not "msvc", just check against hashfs.
		inputs = b.hashFS.Availables(ctx, step.cmd.WorkspaceRoot, step.cmd.Inputs)
	}
	if len(inputs) != len(step.cmd.Inputs) {
		clog.Infof(ctx, "deps remove missing inputs %d -> %d", len(step.cmd.Inputs), len(inputs))
		step.cmd.Inputs = inputs
	}
	return err
}

func (b *Builder) prepareLocalOutdirs(ctx context.Context, step *Step) error {
	ctx, span := trace.NewSpan(ctx, "prepare-local-outdirs")
	defer span.Close(nil)

	seen := make(map[path.Path]bool)
	for _, out := range step.cmd.Outputs {
		outdir := out.Dir()
		if seen[outdir] {
			continue
		}
		if fi, err := b.hashFS.Stat(ctx, b.path.WorkspaceRoot, outdir); err == nil && fi.IsDir() {
			seen[outdir] = true
			continue
		}
		clog.Infof(ctx, "prepare outdirs %s", outdir)
		err := b.hashFS.Mkdir(ctx, b.path.WorkspaceRoot, outdir, nil, nil)
		if err != nil {
			return fmt.Errorf("prepare outdir %q: %w", outdir, err)
		}
		seen[outdir] = true
	}
	return nil
}

// resetDirOutputs wipes each declared directory output before the command runs,
// so a rerun with fewer files leaves no stale members. The action recreates the
// dir itself (siso doesn't), matching the REAPI worker. Call after
// RecordPreOutputs: it removes the prior tree restat compares against.
func (b *Builder) resetDirOutputs(ctx context.Context, step *Step) error {
	for _, dir := range step.cmd.OutputDirs {
		if err := b.hashFS.ResetDirOutput(ctx, step.cmd.WorkspaceRoot, string(dir)); err != nil {
			return fmt.Errorf("reset dir output %s: %w", dir, err)
		}
	}
	return nil
}

// checkLocalOutputs checks if all outputs are on local disk.
// If not, it returns error.
// It ignores missing outputs added by siso config.
func (b *Builder) checkLocalOutputs(ctx context.Context, step *Step) error {
	ctx, span := trace.NewSpan(ctx, "capture-local-outputs")
	defer span.Close(nil)
	span.SetAttr("outputs", len(step.cmd.Outputs)+len(step.cmd.OutputDirs))
	result, _ := step.cmd.ActionResult()
	if result.GetExitCode() != 0 {
		return nil
	}
	if step.def.Binding("phony_output") != "" {
		clog.Infof(ctx, "phony_output. no check output files %q", step.cmd.Outputs)
		return nil
	}

	defOutputs := step.def.Outputs(ctx)

	var dirFsys fs.FS
	if len(step.cmd.OutputDirs) > 0 {
		dirFsys = step.cmd.HashFS.FileSystem(ctx, step.cmd.WorkspaceRoot)
	}
	// Check declared outputs only; the depfile is not declared (updateDeps handles it).
	for i, out := range step.cmd.DeclaredOutputs() {
		fi, err := step.cmd.HashFS.Stat(ctx, step.cmd.WorkspaceRoot, out)
		if err != nil {
			b.targets.Store(string(out), targetState{
				dirtyErr: err,
			})
			required := isRequiredOutput(out, defOutputs)
			if !required {
				clog.Warningf(ctx, "ignore missing outputs %s: %v", out, err)
				continue
			}
			if experiments.Enabled("ignore-missing-outputs", "") {
				b.hashFS.AddMissingOutput(ctx, step.cmd.WorkspaceRoot, out)
				clog.Warningf(ctx, "ignore missing outputs %s: %v", out, err)
				continue
			}
			return fmt.Errorf("missing local outputs %s: %w", out, err)
		}
		isDir := i >= len(step.cmd.Outputs)
		mtime, walkErr := effectiveMtime(dirFsys, string(out), fi.ModTime(), isDir)
		if walkErr != nil {
			b.targets.Store(string(out), targetState{
				dirtyErr: walkErr,
			})
			if isRequiredOutput(out, defOutputs) && !experiments.Enabled("ignore-missing-outputs", "") {
				return fmt.Errorf("walk dir output %s: %w", out, walkErr)
			}
			clog.Warningf(ctx, "walk dir output %s: %v", out, walkErr)
			continue
		}
		b.targets.Store(string(out), targetState{
			mtime:   mtime,
			changed: fi.IsChanged(),
		})
	}
	// don't set result.OutputFiles etc to lazily calculate digest
	// for outputs. b/311312613
	return nil
}

// isRequiredOutput reports whether out is a declared (required) output. It
// matches both slash forms since defOutputs keeps a dir target's trailing
// slash while out (from cmd.AllOutputs) has it stripped.
func isRequiredOutput(out path.Path, defOutputs []path.Path) bool {
	if slices.Contains(defOutputs, out) {
		return true
	}
	return slices.Contains(defOutputs, out+"/")
}

func (b *Builder) logLocalExec(ctx context.Context, step *Step, dur time.Duration) error {
	command := step.def.Binding("command")
	if len(command) > 256 {
		command = command[:256] + " ..."
	}
	allOutputs := step.cmd.AllOutputs()
	var output string
	if len(allOutputs) > 0 {
		output = string(allOutputs[0])
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
