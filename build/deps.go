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
	// paths are workspace relative.
	DepsCmd(context.Context, *Builder, *Step) ([]string, error)

	// collects deps after cmd run.
	// paths are relative to the out dir.
	DepsAfterRun(context.Context, *Builder, *Step) ([]string, error)

	// cleans up deps file.
	DepsClean(context.Context, *Builder, *Step, error)
}

var depsProcessors = map[string]depsProcessor{
	"depfile": depsDepfile{},
	"gcc":     depsGCC{},
	"msvc":    depsMSVC{},
}

// DepfileAddsUnsandboxedFileError is error type when a depfile adds a file as a dep
// that was not part of the original sandbox. Sandboxed actions can only use depfiles
// to promote order-only deps to regular implicit deps.
type DepfileAddsUnsandboxedFileError struct {
	Inputs []string
}

func (e DepfileAddsUnsandboxedFileError) Error() string {
	return fmt.Sprintf("sandboxed action has depfile that adds dependencies that are not listed in its inputs. When sandboxing, depfiles can only be used to promote order-only deps to implicit deps. The files are:\n  %s", strings.Join(e.Inputs, "\n  "))
}

func (e DepfileAddsUnsandboxedFileError) Is(target error) bool {
	t, ok := target.(DepfileAddsUnsandboxedFileError)
	if !ok {
		return false
	}
	return slices.Equal(e.Inputs, t.Inputs)
}

// depsExpandInputs expands step.cmd.Inputs.
// result will not contain labels nor non-existing files.
func depsExpandInputs(ctx context.Context, b *Builder, step *Step) {
	ctx, span := trace.NewSpan(ctx, "deps-expand-inputs")
	defer span.Close(nil)

	fsys := b.hashFS.FileSystem(ctx, filepath.Join(step.cmd.WorkspaceRoot, step.cmd.WorkDir))

	oldlen := len(step.cmd.Inputs)
	var expanded []string
	// deps=gcc,msvc with sources doesn't need to expand inputs,
	// but need to use DepsBaseInputs to get expand phony in build graph inputs.
	includeOrderOnly := false
	sandbox, _ := selectSandbox(ctx, step)
	if sandbox == "nsjail" {
		// for nsjail sandbox, we need to include order-only files.
		// action will use subset of inputs and record them in depfile.
		includeOrderOnly = true
	}
	switch step.cmd.Deps {
	case "gcc":
		params, err := gccutil.ExtractScanDepsParams(ctx, step.cmd.Args, step.cmd.Env, fsys)
		if err == nil && len(params.Sources) > 0 {
			expanded = step.def.DepsBaseInputs(ctx, step.cmd.ToolInputs, includeOrderOnly)
		} else {
			clog.Infof(ctx, "failed to extract scandeps param source=%d: %v", len(params.Sources), err)
			expanded = step.def.ExpandedInputs(ctx)
		}
	case "msvc":
		params, err := msvcutil.ExtractScanDepsParams(ctx, step.cmd.Args, step.cmd.Env, fsys)
		if err == nil && len(params.Sources) > 0 {
			expanded = step.def.DepsBaseInputs(ctx, step.cmd.ToolInputs, includeOrderOnly)
		} else {
			clog.Infof(ctx, "failed to extract scandeps param source=%d: %v", len(params.Sources), err)
			expanded = step.def.ExpandedInputs(ctx)
		}
	default:
		expanded = step.def.ExpandedInputs(ctx)
	}
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
		if _, err := b.hashFS.Stat(ctx, b.path.WorkspaceRoot, in); err != nil {
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
		if _, err := b.hashFS.Stat(ctx, b.path.WorkspaceRoot, in); err != nil {
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
func depsFixCmd(ctx context.Context, b *Builder, step *Step, deps []string) {
	deps = step.def.ExpandedCaseSensitives(ctx, deps)
	err := checkDepsExist(ctx, b, deps)
	if err != nil {
		clog.Warningf(ctx, "check deps exist: %v", err)
		step.cmd.Pure = false
		return
	}
	step.cmd.Pure = true
}

func depsCmd(ctx context.Context, b *Builder, step *Step) error {
	started := time.Now()
	defer func() {
		step.metrics.DepsScanTime = IntervalMetric(time.Since(started))
	}()

	ds, found := depsProcessors[step.cmd.Deps]
	if found {
		start := time.Now()
		includeOrderOnly := false
		stepInputs := step.def.DepsBaseInputs(ctx, step.cmd.ToolInputs, includeOrderOnly)
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

func depsClean(ctx context.Context, b *Builder, step *Step, err error) {
	ds, found := depsProcessors[step.cmd.Deps]
	if !found {
		return
	}
	ds.DepsClean(ctx, b, step, err)
}

func checkDepsExist(ctx context.Context, b *Builder, depsIns []string) error {
	ctx, span := trace.NewSpan(ctx, "check-deps-exist")
	defer span.Close(nil)

	span.SetAttr("deps-inputs", len(depsIns))
	entries, err := b.hashFS.Entries(ctx, b.path.WorkspaceRoot, depsIns)
	if err != nil {
		return fmt.Errorf("failed to get entries: %w", err)
	}
	if len(entries) < len(depsIns) {
		// if deps inputs disappeared, it would be problematic to use the depsIns.
		// don't use .siso_deps, but fallback to scandeps to collect actual include files.
		return fmt.Errorf("missing files in deps %d: %w", len(depsIns)-len(entries), fs.ErrNotExist)
	}

	return nil
}

func checkDepfile(ctx context.Context, b *Builder, step *Step) error {
	// need to write depfile on disk even if output_local_strategy skips downloading. b/355099718
	err := b.hashFS.Flush(ctx, b.path.WorkspaceRoot, []string{step.cmd.Depfile})
	if err != nil {
		return fmt.Errorf("failed to fetch depfile %q: %w", step.cmd.Depfile, err)
	}
	fsys := b.hashFS.FileSystem(ctx, b.path.WorkspaceRoot)
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

	if len(step.cmd.Outputs) == 0 {
		return fmt.Errorf("check deps: no cmd outputs")
	}

	var ninjaInputs map[string]bool
	if step.enforceDepfileOnlyPromotes {
		expInputs := step.def.ExpandedInputs(ctx)
		ninjaInputs = make(map[string]bool, len(expInputs))
		for _, in := range expInputs {
			ninjaInputs[in] = true
		}
	}

	var checkInputs []string
	var unsandboxed []string

	platform := step.cmd.Platform
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
		input := b.path.MaybeFromRelative(ctx, dep)

		// Sandboxed actions can only use depfiles to promote order-only to implicit deps
		if step.enforceDepfileOnlyPromotes && !ninjaInputs[input] {
			unsandboxed = append(unsandboxed, dep)
			continue
		}

		fi, err := b.hashFS.Stat(ctx, b.path.WorkspaceRoot, input)
		if errors.Is(err, fs.ErrNotExist) {
			// file may be read by handler and not found
			// and generated after that (e.g. gn_logs.txt)
			// forget and check again.
			b.hashFS.Forget(ctx, b.path.WorkspaceRoot, []string{input})
			fi, err = b.hashFS.Stat(ctx, b.path.WorkspaceRoot, input)
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
	if len(unsandboxed) > 0 {
		slices.Sort(unsandboxed)
		return DepfileAddsUnsandboxedFileError{Inputs: unsandboxed}
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
