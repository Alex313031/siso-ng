// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package gccutil

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"time"

	"go.chromium.org/build/siso/execute"
	"go.chromium.org/build/siso/execute/localexec"
	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/sync/semaphore"
	"go.chromium.org/build/siso/toolsupport/makeutil"
)

// Semaphore is a semaphore to control concurrent `gcc -M` invocations.
var Semaphore = semaphore.New("deps-gcc", runtime.NumCPU()*2)

// DepsArgs returns command line args to get deps for args.
func DepsArgs(args []string) ([]string, error) {
	args, err := normalizeArgs(args)
	if err != nil {
		return nil, fmt.Errorf("failed to normalize args: %v", err)
	}
	var dargs []string
	skip := false
	for _, arg := range args {
		if skip {
			skip = false
			continue
		}
		switch arg {
		case "-MD", "-MMD", "-c":
			continue
		case "-MF", "-o":
			skip = true
			continue
		}
		if strings.HasPrefix(arg, "-MF") {
			continue
		}
		if strings.HasPrefix(arg, "-o") {
			continue
		}
		dargs = append(dargs, arg)
	}
	dargs = append(dargs, "-M")
	return dargs, nil
}

// Deps runs command specified by args, env, cwd and returns deps.
func Deps(ctx context.Context, args, env []string, cwd string) ([]string, error) {
	s := time.Now()
	cmd := &execute.Cmd{
		Args:     args,
		Env:      env,
		ExecRoot: cwd,
	}
	var wait time.Duration
	err := Semaphore.Do(ctx, func(ctx context.Context) error {
		wait = time.Since(s)
		return localexec.Run(ctx, cmd)
	})
	if err != nil {
		clog.Warningf(ctx, "failed to run %q: %v\n%s\n%s", args, err, cmd.Stdout(), cmd.Stderr())
		return nil, err
	}
	stdout := cmd.Stdout()
	if len(stdout) == 0 {
		clog.Warningf(ctx, "failed to run gcc deps? stdout:0 args:%q\nstderr:%s", cmd.Args, cmd.Stderr())
	}
	deps, err := makeutil.ParseDeps(ctx, stdout)
	clog.Infof(ctx, "gcc deps stdout:%d -> deps:%d: %s (wait:%s): %v", len(stdout), len(deps), time.Since(s), wait, err)
	if err != nil {
		return nil, err
	}
	return deps, nil
}
