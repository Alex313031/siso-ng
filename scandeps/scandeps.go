// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package scandeps

import (
	"context"
	"errors"
	"fmt"
	"hash/maphash"
	"slices"
	"strings"
	"time"

	log "github.com/golang/glog"

	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/o11y/trace"
)

// ScanDeps is a simple C/C++ dependency scanner.
type ScanDeps struct {
	fs *filesystem

	inputDeps map[string][]string

	inputsRequiringClangScandeps map[string]bool

	clangScandeps clangMode
}

// clangMode specifies when fallback to clang scandeps.
type clangMode int

const (
	// default clang mode. don't use clang scandeps.
	clangModeUnspecified clangMode = iota

	// use clang scandeps if it detects unsupported macro, e.g. func-type macro.
	clangModeUnsupportedMacro

	// use clang scandeps if scandeps failed.
	clangModeErr
)

// clangModeFromString converts string to ClangMode.
func clangModeFromString(ctx context.Context, s string) clangMode {
	switch s {
	case "":
		return clangModeUnspecified
	case "unsupported-macro":
		return clangModeUnsupportedMacro
	case "scandeps-err":
		return clangModeErr
	default:
		clog.Warningf(ctx, "unknown clangscandeps mode=%q", s)
		return clangModeUnspecified
	}
}

// ErrRequireClangScandeps is an error indicating that the request needs clang scandeps.
var ErrRequireClangScandeps = errors.New("scandeps: require clang scandeps")

// ErrTooSlow is an error that scandeps took long time.
var ErrTooSlow = errors.New("scandeps: too slow")

var errForTest error

// SetErrForTest sets error for test.
func SetErrForTest(err error) {
	errForTest = err
}

// Options is scandeps options
type Options struct {
	InputDeps map[string][]string

	InputsRequiringClangScandeps []string

	ClangMode string
}

// New creates new ScanDeps.
func New(ctx context.Context, hashfs *hashfs.HashFS, opts Options) *ScanDeps {
	s := &ScanDeps{
		fs: &filesystem{
			hashfs: hashfs,
			seed:   maphash.MakeSeed(),
		},
		inputDeps:                    opts.InputDeps,
		inputsRequiringClangScandeps: make(map[string]bool),
		clangScandeps:                clangModeFromString(ctx, opts.ClangMode),
	}
	for _, i := range opts.InputsRequiringClangScandeps {
		s.inputsRequiringClangScandeps[i] = true
	}
	hashfs.Notify(s.fs.update)
	return s
}

// Request is a request to scan deps.
type Request struct {
	// Defines are defined macros (on command line).
	// macro value would be `"path.h"` or `<path.h>`
	Defines map[string]string

	// Sources are source files.
	Sources []string

	// Includes are additional include files (i.e. -include or /FI).
	// it would be equivalent with `#include "fname"` in source.
	Includes []string

	// Dirs are include directories (search paths) or hmap paths.
	Dirs []string

	// QuoteDirs are include directories specified by -iquote.
	QuoteDirs []string

	// Frameworks are framework directories (search paths).
	Frameworks []string

	// Sysroots are sysroot directories.
	// It also includes toolchain root directory.
	Sysroots []string

	// To mitigate scanning that does not terminate.
	Timeout time.Duration
}

// Scan scans C/C++ source/header files for req to get C/C++ dependencies.
func (s *ScanDeps) Scan(ctx context.Context, workspaceRoot string, req Request) (_ []string, retErr error) {
	defer func() {
		if retErr != nil && s.clangScandeps == clangModeErr && !errors.Is(retErr, ErrRequireClangScandeps) {
			retErr = fmt.Errorf("%w: %v", ErrRequireClangScandeps, retErr)
		}
	}()
	if errForTest != nil {
		return nil, errForTest
	}
	ctx, span := trace.NewSpan(ctx, "scandeps")
	defer span.Close(nil)

	started := time.Now()

	// Assume sysroots use precomputed tree.
	var precomputedTrees []string
	precomputedTrees = append(precomputedTrees, req.Sysroots...)
	// framework, or some system include dirs may also use precomputed tree
	// if precomputed tree is defined for the dir (in addDir later).

	scanner := s.fs.scanner(ctx, workspaceRoot, s.inputDeps, precomputedTrees)
	defer scanner.Close()
	scanner.setMacros(req.Defines)

	// scanner.addSource is stack, and need to parse include (i.e.
	// include file by -include on command line) before source file
	// to get macro definition in include may be needed for include
	// in source file. b/481105408
	for _, s := range slices.Backward(req.Sources) {
		scanner.addSource(ctx, s)
	}
	for _, s := range slices.Backward(req.Includes) {
		scanner.addInclude(ctx, s)
	}
	for _, dir := range req.Dirs {
		if strings.HasSuffix(dir, ".hmap") && scanner.addHmap(ctx, dir) {
			continue
		}
		scanner.addDir(ctx, dir)
	}
	for _, dir := range req.QuoteDirs {
		scanner.addQuoteDir(ctx, dir)
	}
	for _, dir := range req.Frameworks {
		scanner.addFrameworkDir(ctx, dir)
	}

	setupDur := time.Since(started)
	if setupDur > 500*time.Millisecond {
		clog.Infof(ctx, "scan setup dirs:%d %s", len(req.Dirs), setupDur)
	}
	started = time.Now()

	// max scandeps time in chromium/linux all build on P920 is 12s
	// as of 2023-06-26
	// but we see some timeout with 20s on linux-build-perf-developer builder
	// as of 2023-08-31 b/298142575, 2023-10-23 b/307202429
	// it was introduced to mitigate scanning that does not terminate,
	// but we see such scan recently, so set sufficient large timeout
	// to avoid scan failure due to timed out.
	scanTimeout := max(req.Timeout, 60*time.Second)
	lastCtxCheck := time.Now()

	for scanner.hasInputs() {
		dur := time.Since(started)
		if dur > scanTimeout {
			return nil, fmt.Errorf("%w: dirs:%d %s setup:%s total:%s", ErrTooSlow, len(req.Dirs), scanner.stats(), setupDur, dur)
		}
		// ctx.Err() requires mutex lock, so not call so often.
		if time.Since(lastCtxCheck) > 500*time.Millisecond {
			// check whether ctx is canceled.
			if ctx.Err() != nil {
				return nil, fmt.Errorf("ctx err in scandeps dirs:%d %s setup:%s %s: %w", len(req.Dirs), scanner.stats(), setupDur, time.Since(started), ctx.Err())
			}
			lastCtxCheck = time.Now()
		}
		names, err := scanner.nextInputs(ctx)
		if log.V(1) {
			logNames := names
			clog.Infof(ctx, "try include %q: %v", logNames, err)
		}
		if err != nil {
			if log.V(1) {
				clog.Infof(ctx, "nextInputs %v", err)
			}
			switch s.clangScandeps {
			case clangModeUnspecified:
				// ignore error
			case clangModeUnsupportedMacro:
				if errors.Is(err, errUnsupportedMacro) {
					return nil, fmt.Errorf("%w: %v", ErrRequireClangScandeps, err)
				}
				return nil, err
			case clangModeErr:
				// will wrap ErrRequireClangScandeps in defer
				return nil, err
			}
		}
		for _, name := range names {
			incpath, err := scanner.find(ctx, name)
			if err != nil {
				if log.V(2) {
					lv := struct {
						name string
						err  error
					}{name, err}
					clog.Infof(ctx, "name %s not found: %v", lv.name, lv.err)
				}
				continue
			}
			if incpath == "" {
				// already read?
				continue
			}
			if log.V(1) {
				clog.Infof(ctx, "include %s -> %s", name, incpath)
			}
			if s.inputsRequiringClangScandeps[incpath] {
				return nil, ErrRequireClangScandeps
			}
			if deps, ok := s.inputDeps[incpath]; ok {
				if log.V(1) {
					logDeps := deps
					clog.Infof(ctx, "add inputDeps %q", logDeps)
				}
				scanner.addInputs(deps...)
			}
			// TODO: check name in precomputed subtrees (i.e. sysroots etc)?
			// if not found, fallback to `clang -M`?
		}
	}
	results := scanner.results()
	if log.V(1) {
		clog.Infof(ctx, "results=%q", results)
	}
	return results, nil
}
