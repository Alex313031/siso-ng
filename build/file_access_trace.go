// Copyright 2025 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	log "github.com/golang/glog"

	"go.chromium.org/build/siso/execute"
	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/o11y/trace"
	"go.chromium.org/build/siso/toolsupport/straceutil"
)

type fileTraceExecutor struct {
	b               *Builder
	executor        execute.Executor
	inputs, outputs []string
}

func newFileTraceExecutor(ctx context.Context, b *Builder, executor execute.Executor) (*fileTraceExecutor, error) {
	if !straceutil.Available(ctx) {
		return nil, errors.New("strace is not available")
	}
	return &fileTraceExecutor{
		b:        b,
		executor: executor,
	}, nil
}

func (f *fileTraceExecutor) Run(ctx context.Context, cmd *execute.Cmd) error {
	st := straceutil.New(ctx, cmd.ID, cmd.Args, cmd.Dir)
	newCmd := &execute.Cmd{}
	*newCmd = *cmd
	newCmd.Args = st.Args(ctx)
	err := f.executor.Run(ctx, newCmd)
	if err != nil {
		return err
	}
	f.inputs, f.outputs, err = st.PostProcess(ctx)
	return err
}

func (f *fileTraceExecutor) logLocalExec(ctx context.Context, step *Step, dur time.Duration) error {
	err := f.checkTrace(ctx, step, dur)
	if err != nil {
		clog.Warningf(ctx, "failed to check trace %v", err)
	}
	return err
}

// TODO(b/276390237): Provide user friendly build dependency errors caught by file trace

// checkTrace checks step's inputs/outputs and file access check.
//
//   - pure:true/* -  step has rule and marked as pure
//
//   - pure:false/* - step has no rule nor marked as pure
//
//   - pure:*/true - step's inputs/outputs matches the file access.
//
//   - pure:*/can-be-true - step's inputs/outputs cover the file access.
//     i.e. step's inputs has extra files than file access.
//
//   - pure:*/false - step's inputs/outputs don't cover the file access
//     i.e. file access has extra files more than step's inputs/outputs.
//
// It will return error if pure:true/false case, except
//
// - for deps=gcc/msvc, we believe deps is correct by `clang -M` so never return error.
// - if `keeps-going-impure` experiment flag is set, not return error.
func (f *fileTraceExecutor) checkTrace(ctx context.Context, step *Step, dur time.Duration) error {
	ctx, span := trace.NewSpan(ctx, "check-trace")
	defer span.Close(nil)
	b := f.b
	command := step.def.Binding("command")
	if len(command) > 256 {
		command = command[:256] + "..."
	}
	// TODO: collect files in precomputed trees too.
	allInputs := step.cmd.AllInputs()
	allOutputs := step.cmd.AllOutputs()
	var output string
	if len(allOutputs) > 0 {
		output = allOutputs[0]
	}
	var inouts []string
	if step.cmd.Restat {
		inouts = allOutputs
		allOutputs = nil
	}
	inadds, indels, inplatforms, inerrs := filesDiff(ctx, b, allInputs, inouts, f.inputs, step.def.Binding("ignore_extra_input_pattern"))
	outadds, outdels, outplatforms, outerrs := filesDiff(ctx, b, allOutputs, inouts, f.outputs, step.def.Binding("ignore_extra_output_pattern"))
	clog.Infof(ctx, "check-trace inputs=%d+%d+%d=>%d+%d+%d outputs=%d+%d+%d=>%d+%d+%d",
		len(allInputs), len(inouts), len(f.inputs),
		len(inadds), len(indels), len(inplatforms),
		len(allOutputs), len(inouts), len(f.outputs),
		len(outadds), len(outdels), len(outplatforms))

	if len(inerrs) > 0 {
		clog.Warningf(ctx, "inerrs: %q", inerrs)
	}
	if len(outerrs) > 0 {
		clog.Warningf(ctx, "outerrs: %q", outerrs)
	}
	if len(inadds) == 0 && len(indels) == 0 && len(inerrs) == 0 &&
		len(outadds) == 0 && len(outdels) == 0 && len(outerrs) == 0 {
		clog.Infof(ctx, "trace-diff pure")
		log.V(1).Infof("trace-diff-platform %s\ninputs\n %s\noutputs\n %s", step, strings.Join(inplatforms, "\n "), strings.Join(outplatforms, "\n "))
		var buf bytes.Buffer
		fmt.Fprintf(&buf, `cmd: %s pure:%t/true restat:%t %s
action: %s %s
command: %s %d
in:%d in/out:%d out:%d
inerr:%d outerr:%d

`,
			step, step.cmd.Pure, step.cmd.Restat, dur,
			step.cmd.ActionName, output,
			command, dur.Milliseconds(),
			len(allInputs), len(inouts), len(allOutputs),
			len(inerrs), len(outerrs))
		b.localexecLogWriter.Write(buf.Bytes())
		return nil
	}

	if len(inadds) == 0 && len(inerrs) == 0 &&
		len(outadds) == 0 && len(outerrs) == 0 {
		clog.Infof(ctx, "trace-diff can-be-pure")
		log.V(1).Infof("%s trace-diff\ninputs\n-%s\noutputs\n-%s", step, strings.Join(indels, "\n-"), strings.Join(outdels, "\n-"))
		log.V(1).Infof("%s trace-diff-platform\ninputs\n %s\noutputs\n %s", step, strings.Join(inplatforms, "\n "), strings.Join(outplatforms, "\n "))

		var buf bytes.Buffer
		fmt.Fprintf(&buf, `cmd: %s pure:%t/can-be-true restat:%t %s
action: %s %s
command: %s %d
in:%d in/out:%d out:%d
inputs:
-%s
outputs:
-%s

`,
			step, step.cmd.Pure, step.cmd.Restat, dur,
			step.cmd.ActionName, output,
			command, dur.Milliseconds(),
			len(allInputs), len(inouts), len(allOutputs),
			strings.Join(indels, "\n-"),
			strings.Join(outdels, "\n-"))
		b.localexecLogWriter.Write(buf.Bytes())
		return nil
	}
	clog.Infof(ctx, "trace-diff impure")
	log.V(1).Infof("%s trace-diff\ninputs\n+%s\n-%s\n?%s\noutputs\n+%s\n-%s\n?%s", step,
		strings.Join(inadds, "\n+"),
		strings.Join(indels, "\n-"),
		strings.Join(inerrs, "\n?"),
		strings.Join(outadds, "\n+"),
		strings.Join(outdels, "\n-"),
		strings.Join(outerrs, "\n?"))
	log.V(1).Infof("%s trace-diff-platform\ninputs\n %s\noutputs\n %s", step, strings.Join(inplatforms, "\n "), strings.Join(outplatforms, "\n "))

	ruleBuf := step.def.RuleFix(ctx, inadds, outadds)

	var buf bytes.Buffer
	fmt.Fprintf(&buf, `cmd: %s pure:%t/false restat:%t %s
action: %s %s
command: %s %d
in:%d in/out:%d out:%d
inerr:%d outerr:%d
inputs:
+%s
-%s
outputs:
+%s
-%s
toolchainInfo:
%s
allInputs:
 %s

`,
		step, step.cmd.Pure, step.cmd.Restat, dur,
		step.cmd.ActionName, output,
		command, dur.Milliseconds(),
		len(allInputs), len(inouts), len(allOutputs),
		len(inerrs), len(outerrs),
		strings.Join(inadds, "\n+"),
		strings.Join(indels, "\n-"),
		strings.Join(outadds, "\n+"),
		strings.Join(outdels, "\n-"),
		ruleBuf,
		strings.Join(allInputs, "\n "))
	b.localexecLogWriter.Write(buf.Bytes())
	if step.cmd.Pure {
		clog.Warningf(ctx, "impure cmd deps=%q marked as pure", step.cmd.Deps)
		return depsImpureCheck(step, command)
	}
	return nil
}

func filesDiff(ctx context.Context, b *Builder, x, opts, y []string, ignorePattern string) (adds, dels, platforms, errs []string) {
	type state int
	const (
		stateRequired state = iota
		stateOptional
		stateDetected
		stateUsed
	)
	seen := make(map[string]state)
	for _, s := range x {
		seen[s] = stateRequired
	}
	for _, s := range opts {
		seen[s] = stateOptional
	}
	var ignoreRE *regexp.Regexp
	if ignorePattern != "" {
		var err error
		ignoreRE, err = regexp.Compile(ignorePattern)
		if err != nil {
			clog.Warningf(ctx, "bad ignore pattern %q: %v", ignorePattern, err)
		}
	}
	for _, pathname := range y {
		if strings.Contains(pathname, "/__pycache__/") {
			continue
		}
		if strings.HasSuffix(pathname, ".pyc") {
			continue
		}
		if strings.HasSuffix(pathname, ".cache") && strings.Contains(pathname, "__jinja2_") {
			continue
		}
		if strings.Contains(pathname, ".siso") || (strings.Contains(pathname, "siso.") && strings.Contains(pathname, "INFO")) {
			continue
		}
		if ignoreRE != nil && ignoreRE.MatchString(pathname) {
			continue
		}
		name := pathname
		pathname = b.path.AbsFromWD(pathname)
		relname, err := filepath.Rel(b.path.ExecRoot, pathname)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: rel %v", name, err))
			continue
		}
		if !filepath.IsLocal(relname) {
			platforms = append(platforms, pathname)
			continue
		}
		if _, ok := seen[relname]; ok {
			seen[relname] = stateDetected
			continue
		}
		fi, err := b.hashFS.Stat(ctx, b.path.ExecRoot, relname)
		if errors.Is(err, os.ErrNotExist) {
			if log.V(1) {
				clog.Infof(ctx, "%s: stat %v", name, err)
			}
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: stat %v", name, err))
			continue
		}
		if fi.IsDir() {
			continue
		}
		adds = append(adds, relname)
		seen[relname] = stateUsed
		if target := fi.Target(); target != "" {
			target := filepath.Join(filepath.Dir(relname), target)
			s, ok := seen[target]
			if ok {
				if s == stateRequired {
					seen[target] = stateDetected
				}
				continue
			}
			seen[target] = stateUsed
			adds = append(adds, target)
		}
	}
	for name, s := range seen {
		if s == stateRequired {
			dels = append(dels, name)
		}
	}
	sort.Strings(dels)
	return uniqueFiles(adds), uniqueFiles(dels), uniqueFiles(platforms), errs
}

func depsImpureCheck(step *Step, command string) error {
	// deps="gcc","msvc" doesn't use file access. new *.d will have correct deps.
	switch step.cmd.Deps {
	case "gcc", "msvc":
		return nil
	default:
		if experiments.Enabled("keep-going-impure", "impure cmd %s %s %s marked as pure", step, step.cmd.ActionName, command) {
			return nil
		}
	}
	return fmt.Errorf("impure cmd %s %s %s marked as pure", step, step.cmd.ActionName, command)
}
