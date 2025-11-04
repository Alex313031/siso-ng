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
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	log "github.com/golang/glog"

	"go.chromium.org/build/siso/execute"
	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/o11y/trace"
	"go.chromium.org/build/siso/toolsupport/gccutil"
	"go.chromium.org/build/siso/toolsupport/makeutil"
	"go.chromium.org/build/siso/toolsupport/msvcutil"
)

type depsProcessor interface {
	// create new cmd for fast deps run
	DepsFastCmd(context.Context, *Builder, *execute.Cmd) (*execute.Cmd, error)

	// fix step.cmd and returns deps inputs.
	// paths are execroot relative.
	DepsCmd(context.Context, *Builder, *Step) ([]string, error)

	// collects deps after cmd run.
	// paths are cwd relative.
	DepsAfterRun(context.Context, *Builder, *Step) ([]string, error)
}

var depsProcessors = map[string]depsProcessor{
	"depfile": depsDepfile{},
	"gcc":     depsGCC{},
	"msvc":    depsMSVC{},
}

func depsFastStep(ctx context.Context, b *Builder, step *Step) (*Step, error) {
	if step.useReclient() {
		return nil, fmt.Errorf("no fast-deps (use reclient)")
	}
	if len(step.cmd.Platform) == 0 {
		return nil, errors.New("no fast-deps (no remote step)")
	}
	if step.def.Binding("no_fast_deps") != "" {
		return nil, errors.New("no fast-deps (siso config no_fast_deps=true)")
	}
	if reason, ok := b.disableFastDeps.Load().(string); ok && reason != "" {
		return nil, fmt.Errorf("no fast-deps (%s)", reason)
	}
	ds, found := depsProcessors[step.cmd.Deps]
	if !found {
		return nil, fmt.Errorf("no fast-deps (deps=%q depfile=%q)", step.cmd.Deps, step.cmd.Depfile)
	}
	var newCmd *execute.Cmd
	// protect from thread exhaustion,
	// because it would access many files, which would call syscalls
	// and may create new threads.
	err := b.scanDepsSema.Do(ctx, step.weight, func(ctx context.Context) error {
		depsIter, err := step.def.DepInputs(ctx)
		if err != nil {
			return fmt.Errorf("failed to get fast deps log (deps=%q): %w", step.cmd.Deps, err)
		}
		var depsIns []string
		depsIter(func(in string) bool {
			depsIns = append(depsIns, in)
			return true
		})
		newCmd, err = ds.DepsFastCmd(ctx, b, step.cmd)
		if err != nil {
			return err
		}
		newCmd.ID += "-fast-deps"
		// Inputs may contains unnecessary inputs.
		// just needs ToolInputs.
		stepInputs := newCmd.ToolInputs
		depsIns = step.def.ExpandedCaseSensitives(ctx, depsIns)
		inputs, err := fixInputsByDeps(ctx, b, stepInputs, depsIns)
		if err != nil {
			clog.Warningf(ctx, "failed to fix inputs by deps: %v", err)
			return err
		}
		clog.Infof(ctx, "fix inputs by deps %d -> %d", len(step.cmd.Inputs), len(inputs))
		newCmd.Inputs = inputs
		newCmd.Pure = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	fastStep := &Step{}
	*fastStep = *step
	fastStep.cmd = newCmd
	return fastStep, nil
}

// depsExpandInputs expands step.cmd.Inputs.
// result will not contain labels nor non-existing files.
func depsExpandInputs(ctx context.Context, b *Builder, step *Step) {
	ctx, span := trace.NewSpan(ctx, "deps-expand-inputs")
	defer span.Close(nil)

	fsys := b.hashFS.FileSystem(ctx, filepath.Join(step.cmd.ExecRoot, step.cmd.Dir))

	// deps=gcc,msvc with sources doesn't need to expand inputs.
	switch step.cmd.Deps {
	case "gcc":
		params, err := gccutil.ExtractScanDepsParams(ctx, step.cmd.Args, step.cmd.Env, fsys)
		if err == nil && len(params.Sources) > 0 {
			return
		}
	case "msvc":
		params, err := msvcutil.ExtractScanDepsParams(ctx, step.cmd.Args, step.cmd.Env, fsys)
		if err == nil && len(params.Sources) > 0 {
			return
		}
	}

	oldlen := len(step.cmd.Inputs)
	expanded := step.def.ExpandedInputs(ctx)
	inputs := make([]string, 0, oldlen+len(expanded))
	seen := make(map[string]bool)
	for _, in := range step.cmd.Inputs {
		if seen[in] {
			continue
		}
		seen[in] = true
		// labels are expanded in expanded,
		// so no need to preserve it in inputs.
		if strings.Contains(in, ":") {
			if runtime.GOOS == "windows" && filepath.IsAbs(in) {
				if strings.Contains(in[2:], ":") {
					continue
				}
			} else {
				continue
			}
		}
		if _, err := b.hashFS.Stat(ctx, b.path.ExecRoot, in); err != nil {
			clog.Warningf(ctx, "deps stat error %s: %v", in, err)
			continue
		}
		inputs = append(inputs, in)
	}
	for _, in := range expanded {
		if seen[in] {
			continue
		}
		seen[in] = true
		if _, err := b.hashFS.Stat(ctx, b.path.ExecRoot, in); err != nil {
			clog.Warningf(ctx, "deps stat error %s: %v", in, err)
			continue
		}
		inputs = append(inputs, in)
	}
	clog.Infof(ctx, "deps expands %d -> %d", len(step.cmd.Inputs), len(inputs))
	step.cmd.Inputs = make([]string, len(inputs))
	copy(step.cmd.Inputs, inputs)
}

// depsFixCmd checks the purity of the command by checking step inputs and deps.
// It also updates the command inputs when fast deps is enabled.
func depsFixCmd(ctx context.Context, b *Builder, step *Step, deps []string) {
	stepInputs := step.def.Inputs(ctx) // use ToolInputs?
	deps = step.def.ExpandedCaseSensitives(ctx, deps)
	inputs, err := fixInputsByDeps(ctx, b, stepInputs, deps)
	if err != nil {
		clog.Warningf(ctx, "fix inputs by deps: %v", err)
		step.cmd.Pure = false
		return
	}
	step.cmd.Pure = true
	if reason, ok := b.disableFastDeps.Load().(string); ok && reason != "" {
		// if fast deps is not used, scandeps is used for step.cmd
		// so cache will hit in next build.
		// no need to modify step.cmd's Inputs.
		return
	}
	// if fast deps is used, fix cmd to match with deps_log used
	// so that cache will hit in next build.
	step.cmd.Inputs = inputs
}

func depsCmd(ctx context.Context, b *Builder, step *Step) error {
	started := time.Now()
	defer func() {
		step.metrics.DepsScanTime = IntervalMetric(time.Since(started))
	}()

	ds, found := depsProcessors[step.cmd.Deps]
	if found {
		start := time.Now()
		// TODO: same as depsFastStep
		var stepInputs []string
		switch step.cmd.Deps {
		case "gcc", "msvc":
			// Inputs may contains unnecessary inputs.
			// just needs ToolInputs for deps=gcc, msvc.
			stepInputs = step.cmd.ToolInputs
		default:
			stepInputs = step.def.Inputs(ctx) // use ToolInputs?
		}
		depsIns, err := ds.DepsCmd(ctx, b, step)
		depsIns = step.def.ExpandedCaseSensitives(ctx, depsIns)
		inputs := uniqueFiles(stepInputs, depsIns)
		clog.Infof(ctx, "%s-deps %d %s: %v", step.cmd.Deps, len(inputs), time.Since(start), err)
		if err != nil {
			return err
		}
		if log.V(2) {
			clog.Infof(ctx, "inputs: %q", inputs)
		}
		step.cmd.Inputs = inputs
		// DepsCmd should set Pure=true or false.
	}
	return nil
}

func depsAfterRun(ctx context.Context, b *Builder, step *Step) ([]string, error) {
	ds, found := depsProcessors[step.cmd.Deps]
	if !found {
		if log.V(1) {
			clog.Infof(ctx, "update deps; unexpected deps=%q", step.cmd.Deps)
		}
		// deps= is not set, but depfile= is set.
		if step.cmd.Depfile != "" {
			err := checkDepfile(ctx, b, step)
			if err != nil {
				return nil, err
			}
		}
		return nil, nil
	}
	deps, err := ds.DepsAfterRun(ctx, b, step)
	if err != nil {
		return nil, err
	}
	return deps, nil
}

func fixInputsByDeps(ctx context.Context, b *Builder, stepInputs, depsIns []string) ([]string, error) {
	ctx, span := trace.NewSpan(ctx, "fix-inputs-by-deps")
	defer span.Close(nil)
	span.SetAttr("deps-inputs", len(depsIns))
	entries, err := b.hashFS.Entries(ctx, b.path.ExecRoot, depsIns)
	if err != nil {
		return nil, fmt.Errorf("failed to get entries: %w", err)
	}
	if len(entries) < len(depsIns) {
		// if deps inputs disappeared, it would be problematic
		// to use the depsIns.
		// don't use .siso_deps, but fallback to scandeps to
		// collect actual include files.
		return nil, fmt.Errorf("missing files in deps %d: %w", len(depsIns)-len(entries), fs.ErrNotExist)
	}
	inputs := stepInputs
	for _, ent := range entries {
		inputs = append(inputs, b.path.Intern(ent.Name))
		// no need to recursive expand?
		// log.Infof("%s fix-input %s => %s", step, ent.Name, ent.Target)
		if ent.Target != "" {
			inputs = append(inputs, b.path.Intern(filepath.Join(filepath.Dir(ent.Name), ent.Target)))
		}
	}
	inputs = uniqueFiles(inputs)
	return inputs, nil
}

func checkDepfile(ctx context.Context, b *Builder, step *Step) error {
	// need to write depfile on disk even if output_local_strategy skips downloading. b/355099718
	err := b.hashFS.Flush(ctx, b.path.ExecRoot, []string{step.cmd.Depfile})
	if err != nil {
		return fmt.Errorf("failed to fetch depfile %q: %w", step.cmd.Depfile, err)
	}
	fsys := b.hashFS.FileSystem(ctx, b.path.ExecRoot)
	deps, err := makeutil.ParseDepsFile(ctx, fsys, step.cmd.Depfile)
	if err != nil {
		return fmt.Errorf("failed to parse depfile %q: %w", step.cmd.Depfile, err)
	}
	err = checkDeps(ctx, b, step, deps)
	if err != nil {
		return fmt.Errorf("error in depfile %q: %w", step.cmd.Depfile, err)
	}
	return nil
}

func checkDeps(ctx context.Context, b *Builder, step *Step, deps []string) error {
	// TODO: implement check that deps output (target in depfile "<target>: <dependencyList>") matches build graph's output.

	var checkInputs []string

	platform := step.cmd.Platform
	if step.useReclient() && step.cmd.REProxyConfig != nil {
		// TODO: get platform for use_remote_exec_wrapper case.
		platform = step.cmd.REProxyConfig.Platform
	}
	relocatableReq := platform["InputRootAbsolutePath"] == ""
	for _, dep := range deps {
		// remote relocatableReq should not have absolute path dep.
		if filepath.IsAbs(dep) {
			if relocatableReq {
				clog.Warningf(ctx, "check deps: abs path in deps %s: platform=%v", dep, platform)
				if platform != nil {
					return fmt.Errorf("absolute path in deps %q of %q: use input_root_absolute_path=true for %q (siso config: %s): %w", dep, step, step.cmd.Outputs[0], step.def.RuleName(), errNotRelocatable)
				}
			}
			continue
		}
		// all dep (== inputs) should exist just after step ran.
		input := b.path.MaybeFromWD(ctx, dep)
		fi, err := b.hashFS.Stat(ctx, b.path.ExecRoot, input)
		if errors.Is(err, fs.ErrNotExist) {
			// file may be read by handler and not found
			// and generated after that (e.g. gn_logs.txt)
			// forget and check again.
			b.hashFS.Forget(ctx, b.path.ExecRoot, []string{input})
			fi, err = b.hashFS.Stat(ctx, b.path.ExecRoot, input)
		}
		if err != nil {
			return fmt.Errorf("deps input %q not exist: %w", dep, err)
		}
		// input should not be output.
		if slices.Contains(step.cmd.Outputs, input) {
			return fmt.Errorf("deps input %q is output", dep)
		}
		if len(fi.CmdHash()) == 0 {
			// source, not generated file
			// ok to be in deps even if it is not in step's input.
			continue
		}
		if dep == "build.ninja" {
			// build.ninja should have been updated at the beginning of the build.
			continue
		}
		checkInputs = append(checkInputs, input)
	}
	if experiments.Enabled("check-deps", "") || experiments.Enabled("fail-on-bad-deps", "") {
		unknownBadDep, err := step.def.CheckInputDeps(ctx, checkInputs)
		if err != nil {
			clog.Warningf(ctx, "deps error: %v", err)
			if unknownBadDep && experiments.Enabled("fail-on-bad-deps", "") {
				return fmt.Errorf("deps error: %w", err)
			}
			stderr := step.cmd.Stderr()
			w := step.cmd.StderrWriter()
			if len(stderr) != 0 && !bytes.HasSuffix(stderr, []byte("\n")) {
				fmt.Fprintf(w, "\n")
			}
			fmt.Fprintf(w, "deps error: %v\n", err)
		}
	}
	return nil
}
