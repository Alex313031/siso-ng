// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	log "github.com/golang/glog"

	"go.chromium.org/build/siso/execute"
	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/o11y/trace"
	"go.chromium.org/build/siso/reapi/merkletree"
	"go.chromium.org/build/siso/scandeps"
	"go.chromium.org/build/siso/toolsupport/gccutil"
	"go.chromium.org/build/siso/toolsupport/makeutil"
)

type depsGCC struct {
	// for unittest
	treeInput func(context.Context, string) (merkletree.TreeEntry, error)
}

func (gcc depsGCC) DepsFastCmd(ctx context.Context, b *Builder, cmd *execute.Cmd) (*execute.Cmd, error) {
	newCmd := &execute.Cmd{}
	*newCmd = *cmd
	inputs, err := gcc.fixCmdInputs(ctx, b, newCmd)
	if err != nil {
		return nil, err
	}
	// sets include dirs + sysroots to ToolInputs.
	// Inputs will be overridden by deps log data.
	newCmd.ToolInputs = append(newCmd.ToolInputs, inputs...)
	gcc.fixForSplitDwarf(ctx, newCmd)
	return newCmd, nil
}

func (gcc depsGCC) fixCmdInputs(ctx context.Context, b *Builder, cmd *execute.Cmd) ([]string, error) {
	params, err := gccutil.ExtractScanDepsParams(ctx, cmd.Args, cmd.Env, b.hashFS.FileSystem(ctx, filepath.Join(cmd.ExecRoot, cmd.Dir)))
	if err != nil {
		return nil, err
	}
	for i := range params.Files {
		params.Files[i] = b.path.MaybeFromWD(ctx, params.Files[i])
	}
	for i := range params.Dirs {
		params.Dirs[i] = b.path.MaybeFromWD(ctx, params.Dirs[i])
	}
	for i := range params.Frameworks {
		params.Frameworks[i] = b.path.MaybeFromWD(ctx, params.Frameworks[i])
	}
	for i := range params.Sysroots {
		params.Sysroots[i] = b.path.MaybeFromWD(ctx, params.Sysroots[i])
	}
	var inputs []string
	if len(params.Sources) == 0 {
		// If ExtractScanDepsParams doesn't return Sources, such action uses inputs from ninja build file directly, as the action doesn't need include scanning.
		// e.g. clang modules, rust and etc.
		inputs = slices.Clone(cmd.Inputs)
	}
	// include files detected by command line. i.e. sanitaizer ignore lists.
	// These would not be in depsfile, different from Sources.
	inputs = append(inputs, params.Files...)
	// include directory must be included, even if no include files there.
	// without the dir, it may fail for `#include "../config.h"`
	inputs = append(inputs, params.Dirs...)
	// also frameworks include dirs.
	inputs = append(inputs, params.Frameworks...)
	// sysroot directory must be included, even if no include files there.
	// or error with
	// clang++: error: no such sysroot directory: ...
	// [-Werror, -Wmissing-sysroot]
	inputs = append(inputs, params.Sysroots...)
	inputs = b.expandInputs(ctx, inputs)

	fn := func(ctx context.Context, dir string) (merkletree.TreeEntry, error) {
		return b.treeInput(ctx, dir, ":headers", nil)
	}
	if gcc.treeInput != nil {
		fn = gcc.treeInput
	}
	precomputedDirs := make([]string, 0, len(params.Sysroots)+len(params.Frameworks))
	precomputedDirs = append(precomputedDirs, params.Sysroots...)
	precomputedDirs = append(precomputedDirs, params.Frameworks...)
	cmd.TreeInputs = append(cmd.TreeInputs, treeInputs(ctx, fn, precomputedDirs, params.Dirs)...)
	return inputs, nil
}

// TODO: use handler?
func (depsGCC) fixForSplitDwarf(ctx context.Context, cmd *execute.Cmd) {
	hasSplitDwarf := slices.Contains(cmd.Args, "-gsplit-dwarf")
	if !hasSplitDwarf {
		return
	}
	dwo := ""
	for _, out := range cmd.Outputs {
		if strings.HasSuffix(out, ".o") { // TODO: or ".obj" for win?
			dwo = strings.TrimSuffix(out, ".o") + ".dwo"
			continue
		}
	}
	clog.Infof(ctx, "add %s", dwo)
	cmd.Outputs = uniqueFiles(cmd.Outputs, []string{dwo})
}

func (depsGCC) DepsAfterRun(ctx context.Context, b *Builder, step *Step) (_ []string, err error) {
	ctx, span := trace.NewSpan(ctx, "gcc-deps")
	defer span.Close(nil)
	if step.cmd.Deps != "gcc" {
		return nil, fmt.Errorf("gcc-deps; unexpected deps=%q %s", step.cmd.Deps, step)
	}
	defer func() {
		// don't remove depfile if it is used as output.
		if slices.Contains(step.cmd.Outputs, step.cmd.Depfile) {
			return
		}
		if err != nil {
			clog.Warningf(ctx, "preserve depfile=%q: %v", step.cmd.Depfile, err)
			return
		}
		if !b.keepDepfile {
			b.hashFS.Remove(ctx, step.cmd.ExecRoot, step.cmd.Depfile)
		}

		b.hashFS.Flush(ctx, step.cmd.ExecRoot, []string{step.cmd.Depfile})
	}()
	buf, err := b.hashFS.ReadFile(ctx, step.cmd.ExecRoot, step.cmd.Depfile)
	if err != nil {
		return nil, fmt.Errorf("gcc-deps: failed to get depfile %q of %s: %w", step.cmd.Depfile, step, err)
	}
	span.SetAttr("depfile", step.cmd.Depfile)
	span.SetAttr("deps-file-size", len(buf))

	_, dspan := trace.NewSpan(ctx, "parse-deps")
	deps, err := makeutil.ParseDeps(ctx, buf)
	if err != nil {
		return nil, fmt.Errorf("gcc-deps: failed to parse depfile %q: %w", step.cmd.Depfile, err)
	}
	err = checkDeps(ctx, b, step, deps)
	if err != nil {
		return nil, fmt.Errorf("error in depfile %q: %w", step.cmd.Depfile, err)
	}
	dspan.SetAttr("deps", len(deps))
	dspan.Close(nil)
	return deps, nil
}

func (gcc depsGCC) DepsCmd(ctx context.Context, b *Builder, step *Step) ([]string, error) {
	depsIns, err := gcc.depsInputs(ctx, b, step)
	if err != nil {
		return nil, err
	}
	if step.def.Binding("use_remote_exec_wrapper") == "" && b.reapiclient != nil {
		// no need to upload precomputed subtree in inputs
		// when remote exec wrapper is used or not using reapi.
		// b/283867642
		inputs, err := gcc.fixCmdInputs(ctx, b, step.cmd)
		if err != nil {
			return nil, err
		}
		depsIns = append(depsIns, inputs...)
	}
	gcc.fixForSplitDwarf(ctx, step.cmd)
	return depsIns, err
}

func (gcc depsGCC) depsInputs(ctx context.Context, b *Builder, step *Step) ([]string, error) {
	ins, err := gcc.scandeps(ctx, b, step)
	if errors.Is(err, scandeps.ErrRequireClangScandeps) {
		step.metrics.ClangScandeps = true
		ins, err = gcc.scandepsByClang(ctx, b, step)
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil, err
		}
		step.metrics.ScandepsErr = true
		return nil, err
	}
	return ins, nil
}

func (depsGCC) scandeps(ctx context.Context, b *Builder, step *Step) ([]string, error) {
	var ins []string
	err := b.scanDepsSema.Do(ctx, step.weight, func(ctx context.Context) error {
		debug := step.def.Binding("debug") == "true"
		// fastDeps + remote execution may have already run.
		// In this case, do not change ActionStartTime set by the remote exec.
		b.actionStarted(step)
		params, err := gccutil.ExtractScanDepsParams(ctx, step.cmd.Args, step.cmd.Env, b.hashFS.FileSystem(ctx, filepath.Join(step.cmd.ExecRoot, step.cmd.Dir)))
		if err != nil {
			return err
		}
		if len(params.Sources) == 0 {
			// If ExtractScanDepsParams doesn't return Sources, such action uses inputs from ninja build file directly, as the action doesn't need include scanning.
			// e.g. clang modules, rust and etc.
			if bool(log.V(1)) || debug {
				clog.Infof(ctx, "no source extracted")
			}
			return nil
		}

		// externals stores non local paths.
		// usually error, but can be used for scandeps for cros chroot case.
		var externals []string
		for i := range params.Sources {
			params.Sources[i] = b.path.MaybeFromWD(ctx, params.Sources[i])
			if !filepath.IsLocal(params.Sources[i]) {
				externals = append(externals, params.Sources[i])
			}
		}
		for i := range params.Includes {
			params.Includes[i] = b.path.MaybeFromWD(ctx, params.Includes[i])
			if !filepath.IsLocal(params.Includes[i]) {
				externals = append(externals, params.Includes[i])
			}
		}
		for i := range params.Files {
			params.Files[i] = b.path.MaybeFromWD(ctx, params.Files[i])
			if !filepath.IsLocal(params.Files[i]) {
				externals = append(externals, params.Files[i])
			}
		}
		for i := range params.Dirs {
			params.Dirs[i] = b.path.MaybeFromWD(ctx, params.Dirs[i])
			if !filepath.IsLocal(params.Dirs[i]) {
				externals = append(externals, params.Dirs[i])
			}
		}
		for i := range params.Frameworks {
			params.Frameworks[i] = b.path.MaybeFromWD(ctx, params.Frameworks[i])
			if !filepath.IsLocal(params.Frameworks[i]) {
				externals = append(externals, params.Frameworks[i])
			}
		}
		for i := range params.Sysroots {
			params.Sysroots[i] = b.path.MaybeFromWD(ctx, params.Sysroots[i])
			if !filepath.IsLocal(params.Sysroots[i]) {
				externals = append(externals, params.Sysroots[i])
			}
		}
		execRoot := b.path.ExecRoot
		if len(externals) > 0 {
			if !step.cmd.RemoteChroot() {
				n := len(externals)
				v := externals[:min(len(externals), 5)]
				return fmt.Errorf("inputs are not under exec root %d %q...: platform=%q", n, v, step.cmd.Platform)
			}
			// Convert paths from relative to exec root to relative to /
			// e.g.
			//  execRoot: /path/to/chromium/src
			//     path:  ../../../../usr/include
			// ->
			//  execRoot: /
			//     path:  usr/include
			execRoot = "/"
			for i := range params.Sources {
				params.Sources[i] = filepath.Join(b.path.ExecRoot, params.Sources[i])[1:]
			}
			for i := range params.Includes {
				params.Includes[i] = filepath.Join(b.path.ExecRoot, params.Includes[i])[1:]
			}
			for i := range params.Files {
				params.Files[i] = filepath.Join(b.path.ExecRoot, params.Files[i])[1:]
			}
			for i := range params.Dirs {
				params.Dirs[i] = filepath.Join(b.path.ExecRoot, params.Dirs[i])[1:]
			}
			for i := range params.Frameworks {
				params.Frameworks[i] = filepath.Join(b.path.ExecRoot, params.Frameworks[i])[1:]
			}
			for i := range params.Sysroots {
				params.Sysroots[i] = filepath.Join(b.path.ExecRoot, params.Sysroots[i])[1:]
			}
		}
		req := scandeps.Request{
			Defines:    params.Defines,
			Sources:    params.Sources,
			Includes:   params.Includes,
			Dirs:       params.Dirs,
			Frameworks: params.Frameworks,
			Sysroots:   params.Sysroots,
			Timeout:    step.cmd.Timeout,
		}
		if !b.localFallbackEnabled() {
			// no-fallback has longer timeout for scandeps
			req.Timeout = 2 * req.Timeout
		}
		if bool(log.V(1)) || debug {
			buf, berr := json.Marshal(req)
			if berr != nil {
				return berr
			}
			clog.Infof(ctx, "scandeps req=%s", buf)
		}
		started := time.Now()
		ins, err = b.scanDeps.Scan(ctx, execRoot, req)
		if bool(log.V(1)) || debug {
			clog.Infof(ctx, "scandeps %d %s: %v", len(ins), time.Since(started), err)
		}
		if err != nil {
			buf, berr := json.Marshal(req)
			clog.Warningf(ctx, "scandeps failed Request %s %v: %v", buf, berr, err)
			return err
		}
		ins = append(ins, params.Files...)
		if len(externals) > 0 {
			// make ins[i] full absolute paths.
			for i := range ins {
				ins[i] = filepath.Join(execRoot, ins[i])
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for i := range ins {
		ins[i] = b.path.Intern(ins[i])
	}
	return ins, nil
}

func (gcc depsGCC) scandepsByClang(ctx context.Context, b *Builder, step *Step) ([]string, error) {
	cwd := b.path.AbsFromWD(".")
	err := b.prepareLocalInputs(ctx, step)
	if err != nil {
		return nil, fmt.Errorf("prepare for gcc deps: %w", err)
	}
	dargs, err := gccutil.DepsArgs(step.cmd.Args)
	if err != nil {
		return nil, err
	}
	ins, err := gccutil.Deps(ctx, dargs, nil, cwd)
	if err != nil {
		return nil, err
	}
	var inputs []string
	for _, in := range ins {
		// TODO: need to preserve intermediate dirs
		// e.g.
		//  /usr/local/google/home/ukai/src/chromium/src/native_client/toolchain/linux_x86/nacl_x86_glibc/bin/../lib/gcc/x86_64-nacl/4.4.3/../../../../x86_64-nacl/include/stdint.h
		inpath := b.path.MaybeFromWD(ctx, in)
		fi, err := b.hashFS.Stat(ctx, b.path.ExecRoot, inpath)
		if err != nil {
			clog.Warningf(ctx, "missing inputs? %s: %v", inpath, err)
			continue
		}
		inputs = append(inputs, inpath)
		if target := fi.Target(); target != "" {
			// checks all intermediate symlink dirs if leaf is symlink.
			inputs = append(inputs, gcc.expandSymlinkDirs(ctx, b, inpath)...)
		}
	}
	sort.Strings(inputs)
	return inputs, nil
}

func (depsGCC) expandSymlinkDirs(ctx context.Context, b *Builder, inpath string) []string {
	fsys := b.hashFS.FileSystem(ctx, b.path.ExecRoot)
	return fsys.ExpandSymlinks(inpath)
}
