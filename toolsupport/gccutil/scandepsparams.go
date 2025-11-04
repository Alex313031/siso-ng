// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package gccutil

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"

	"go.chromium.org/build/siso/toolsupport/shutil"
)

// ScanDepsParams holds parameters used for scandeps.
type ScanDepsParams struct {
	// Sources are source files.
	Sources []string

	// Includes are include files by -include.
	Includes []string

	// Files are input files, such as sanitaizer ignore list.
	Files []string

	// Dirs are include directories.
	Dirs []string

	// Frameworks are framework directories.
	Frameworks []string

	// Sysroots are sysroot directories and toolchain root directories.
	Sysroots []string

	// Defines are defined macros.
	Defines map[string]string
}

// stepArgs in ninjabuild uses "/bin/sh -c $command" when
// $command is not simple command line.
// soong puts ${g.cc.relPwd} ("PWD=/proc/self/cwd")
// at the front of command line, so parse command line
// after dropping "PWD=/proc/self/cwd " if it exists.
// TODO: b/432599730 - remove the workaround
func normalizeArgs(args []string) ([]string, error) {
	if len(args) == 3 && args[0] == "/bin/sh" && args[1] == "-c" {
		if !strings.HasPrefix(args[2], "PWD=/proc/self/cwd ") {
			return nil, errors.New("unsupported commandline. no PWD=/proc/self/cwd prefix")
		}
		// TODO: b/432374760 - need to strip ${postCmd} part?
		cmdArgs, err := shutil.Split(strings.TrimPrefix(args[2], "PWD=/proc/self/cwd "))
		if err != nil {
			return nil, fmt.Errorf("failed to split %q: %v", args[2], err)
		}
		return cmdArgs, nil
	}
	return args, nil
}

// ExtractScanDepsParams parses args and returns ScanDepsParams for scandeps.
// It only parses major command line flags used in chromium and android.
// It reads @rspfile via fsys.
// Full set of command line flags for include dirs can be found in
// https://clang.llvm.org/docs/ClangCommandLineReference.html#include-path-management
func ExtractScanDepsParams(ctx context.Context, args, env []string, fsys fs.FS) (ScanDepsParams, error) {
	res := ScanDepsParams{
		Defines: make(map[string]string),
	}
	args, err := normalizeArgs(args)
	if err != nil {
		return res, fmt.Errorf("failed to normalize args: %w", err)
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			cmdname := filepath.Base(arg)
			switch {
			case strings.HasSuffix(cmdname, "clang"),
				strings.HasSuffix(cmdname, "clang++"),
				strings.HasSuffix(cmdname, "gcc"),
				strings.HasSuffix(cmdname, "g++"):
				// add toolchain top dir as sysroots too
				res.Sysroots = append(res.Sysroots, filepath.ToSlash(filepath.Dir(filepath.Dir(arg))))
			}
		}
		switch arg {
		case "-I", "--include-directory", "-isystem", "-iquote":
			i++
			res.Dirs = append(res.Dirs, args[i])
			continue
		case "-F", "-iframework":
			i++
			res.Frameworks = append(res.Frameworks, args[i])
			continue
		case "-include":
			i++
			res.Includes = append(res.Includes, args[i])
			continue
		case "-isysroot":
			i++
			res.Sysroots = append(res.Sysroots, args[i])
			continue
		case "--sysroot":
			i++
			res.Sysroots = append(res.Sysroots, args[i])
		case "-D":
			i++
			defineMacro(res.Defines, args[i])
			continue
		}
		switch {
		case strings.HasPrefix(arg, "@"):
			// https://llvm.org/docs/CommandLine.html#response-files
			rspfile := strings.TrimPrefix(arg, "@")
			res.Files = append(res.Files, rspfile)
			buf, err := fs.ReadFile(fsys, rspfile)
			if err != nil {
				return res, fmt.Errorf("failed to read @%q: %w", rspfile, err)
			}
			rspArgs, err := shutil.Split(string(buf))
			if err != nil {
				return res, fmt.Errorf("failed to split @%q: %w", rspfile, err)
			}
			// better to delete args[i], insert rspArgs at args[i], and check args[i] again?
			args = slices.Insert(args, i+1, rspArgs...)

		case strings.HasPrefix(arg, "-I"):
			res.Dirs = append(res.Dirs, strings.TrimPrefix(arg, "-I"))
		case strings.HasPrefix(arg, "--include="):
			res.Includes = append(res.Includes, strings.TrimPrefix(arg, "--include="))
		case strings.HasPrefix(arg, "--include-directory="):
			res.Dirs = append(res.Dirs, strings.TrimPrefix(arg, "--include-directory="))
		case strings.HasPrefix(arg, "-iquote"):
			res.Dirs = append(res.Dirs, strings.TrimPrefix(arg, "-iquote"))
		case strings.HasPrefix(arg, "-isystem"):
			res.Dirs = append(res.Dirs, strings.TrimPrefix(arg, "-isystem"))
		case strings.HasPrefix(arg, "-F"):
			res.Frameworks = append(res.Frameworks, strings.TrimPrefix(arg, "-F"))
		case strings.HasPrefix(arg, "-fmodule-file="):
			moduleFile := strings.TrimPrefix(arg, "-fmodule-file=")
			if _, after, found := strings.Cut(moduleFile, "="); found {
				res.Files = append(res.Files, after)
			} else {
				res.Files = append(res.Files, moduleFile)
			}
		case strings.HasPrefix(arg, "-fmodule-map-file="):
			res.Files = append(res.Files, strings.TrimPrefix(arg, "-fmodule-map-file="))
		case strings.HasPrefix(arg, "-fprofile-list="):
			res.Files = append(res.Files, strings.TrimPrefix(arg, "-fprofile-list="))
		case strings.HasPrefix(arg, "-fprofile-use="):
			res.Files = append(res.Files, strings.TrimPrefix(arg, "-fprofile-use="))
		case strings.HasPrefix(arg, "-fprofile-sample-use="):
			res.Files = append(res.Files, strings.TrimPrefix(arg, "-fprofile-sample-use="))
		case strings.HasPrefix(arg, "-fsanitize-ignorelist="):
			res.Files = append(res.Files, strings.TrimPrefix(arg, "-fsanitize-ignorelist="))
		case strings.HasPrefix(arg, "-iframework"):
			res.Frameworks = append(res.Frameworks, strings.TrimPrefix(arg, "-iframework"))
		case strings.HasPrefix(arg, "--gcc-toolchain="):
			res.Sysroots = append(res.Sysroots, strings.TrimPrefix(arg, "--gcc-toolchain="))
		case strings.HasPrefix(arg, "--sysroot="):
			res.Sysroots = append(res.Sysroots, strings.TrimPrefix(arg, "--sysroot="))
		case strings.HasPrefix(arg, "-D"):
			defineMacro(res.Defines, strings.TrimPrefix(arg, "-D"))

		case !strings.HasPrefix(arg, "-"):
			ext := filepath.Ext(arg)
			switch ext {
			case ".c", ".cc", ".cxx", ".cpp", ".m", ".mm", ".S":
				res.Sources = append(res.Sources, arg)
			}
		}
	}
	return res, nil
}

func defineMacro(defines map[string]string, arg string) {
	// arg: macro=value
	macro, value, ok := strings.Cut(arg, "=")
	if !ok {
		// just `-D MACRO`
		return
	}
	if value == "" {
		// `-D MACRO=`
		// no value
		return
	}
	switch value[0] {
	case '<', '"':
		// <path.h> or "path.h"?
		defines[macro] = value
	}
}
