// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package msvcutil

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"go.chromium.org/build/siso/execute"
	"go.chromium.org/build/siso/execute/localexec"
	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/sync/semaphore"
)

// msvc may localize text, but we assume developers don't use that.
const depsPrefix = "Note: including file: "

// ParseShowIncludes parses /showIncludes outputs, and returns a list of inputs and other outputs.
func ParseShowIncludes(b []byte) ([]string, []byte) {
	// showIncludes contents
	//  Note: including file:  <pathname>\r\n
	//
	// other lines will be normal stdout/stderr (e.g. compiler error message)
	var deps []string
	var outs []byte
	var seenShowIncludes bool
	s := b
	for len(s) > 0 {
		line := s
		i := bytes.IndexAny(s, "\r\n")
		if i >= 0 {
			line = line[:i]
			s = s[i:]
		} else {
			s = nil
		}
		if bytes.HasPrefix(line, []byte(depsPrefix)) {
			seenShowIncludes = true
			line = bytes.TrimPrefix(line, []byte(depsPrefix))
			line = bytes.TrimSpace(line)
			deps = append(deps, string(line))
			if bytes.HasPrefix(s, []byte("\r")) {
				s = s[1:]
			}
			if bytes.HasPrefix(s, []byte("\n")) {
				s = s[1:]
			}
			continue
		} else if !seenShowIncludes && filterInputFilename(line) {
			// drop it.
			if bytes.HasPrefix(s, []byte("\r")) {
				s = s[1:]
			}
			if bytes.HasPrefix(s, []byte("\n")) {
				s = s[1:]
			}
			continue
		}
		outs = append(outs, line...)
		if bytes.HasPrefix(s, []byte("\r")) {
			outs = append(outs, '\r')
			s = s[1:]
		}
		if bytes.HasPrefix(s, []byte("\n")) {
			outs = append(outs, '\n')
			s = s[1:]
		}
	}
	return deps, outs
}

func filterInputFilename(line []byte) bool {
	i := bytes.LastIndexByte(line, '.')
	if i < 0 {
		return false
	}
	switch ext := string(bytes.ToLower(line[i:])); ext {
	case ".c", ".cc", ".cxx", ".cpp", ".c++":
		return true
	}
	return false
}

// Semaphore is a semaphore to control concurrent `clang-cl /showIncludes` invocations.
var Semaphore = semaphore.New("deps-msvc", runtime.NumCPU()*2)

// DepsArgs returns command line args to get deps for args.
func DepsArgs(args []string) []string {
	var dargs []string
	hasShowIncludes := false
	for _, arg := range args {
		switch arg {
		case "/showIncludes:user":
			dargs = append(dargs, "/showIncludes")
			hasShowIncludes = true
			continue
		case "/showIncludes":
			hasShowIncludes = true
		case "/c":
			dargs = append(dargs, "/P")
			continue
		}
		switch {
		case strings.HasPrefix(arg, "/Fo"):
			continue
		case strings.HasPrefix(arg, "/Fd"):
			continue
		}
		dargs = append(dargs, arg)
	}
	if !hasShowIncludes {
		dargs = append(dargs, "/showIncludes")
	}
	return dargs
}

// Deps runs command specified by args, env, cwd and returns deps.
func Deps(ctx context.Context, args, env []string, cwd string) ([]string, error) {
	s := time.Now()
	var src, out string
	for _, arg := range args {
		// /P generates *.i in the current dir
		switch ext := filepath.Ext(arg); ext {
		case ".cpp", ".cc", ".cxx", ".c", ".S", ".s":
			src = arg
			out = strings.TrimSuffix(filepath.Base(arg), ext) + ".i"
		}
	}
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
	if rerr := os.Remove(filepath.Join(cwd, out)); rerr != nil {
		clog.Warningf(ctx, "failed to remove %s: %v", filepath.Join(cwd, out), rerr)
	}
	if err != nil {
		clog.Warningf(ctx, "failed to run %q: %v\n%s\n%s", args, err, cmd.Stdout(), cmd.Stderr())
		return nil, err
	}
	// Note: RBE merges stdout and stderr. b/149501385
	stderr := cmd.Stderr()
	deps, extra := ParseShowIncludes(stderr)
	clog.Infof(ctx, "msvc deps stderr:%d -> deps:%d extra:%q %s (wait:%s)", len(stderr), len(deps), extra, time.Since(s), wait)
	deps = append(deps, src)
	return deps, nil
}
