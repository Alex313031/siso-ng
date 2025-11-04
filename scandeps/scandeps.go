// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package scandeps

import (
	"context"
	"errors"
	"fmt"
	"hash/maphash"
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
}

var ErrRequireClangScandeps = errors.New("scandeps: require clang scandeps")

var errForTest error

// SetErrForTest sets error for test.
func SetErrForTest(err error) {
	errForTest = err
}

// New creates new ScanDeps.
func New(hashfs *hashfs.HashFS, inputDeps map[string][]string, inputsRequiringClangScandeps []string) *ScanDeps {
	s := &ScanDeps{
		fs: &filesystem{
			hashfs: hashfs,
			seed:   maphash.MakeSeed(),
		},
		inputDeps:                    inputDeps,
		inputsRequiringClangScandeps: make(map[string]bool),
	}
	for _, i := range inputsRequiringClangScandeps {
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

	// Frameworks are framework directories (search paths).
	Frameworks []string

	// Sysroots are sysroot directories.
	// It also includes toolchain root directory.
	Sysroots []string

	// To mitigate scanning that does not terminate.
	Timeout time.Duration
}

// Scan scans C/C++ source/header files for req to get C/C++ dependencies.
func (s *ScanDeps) Scan(ctx context.Context, execRoot string, req Request) ([]string, error) {
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

	scanner := s.fs.scanner(ctx, execRoot, s.inputDeps, precomputedTrees)
	scanner.setMacros(req.Defines)

	for _, s := range req.Includes {
		scanner.addInclude(ctx, s)
	}
	for _, s := range req.Sources {
		scanner.addSource(ctx, s)
	}
	for _, dir := range req.Dirs {
		if strings.HasSuffix(dir, ".hmap") && scanner.addHmap(ctx, dir) {
			continue
		}
		scanner.addDir(ctx, dir)
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

	icnt := 0
	ncnt := 0
	for scanner.hasInputs() {
		icnt++
		dur := time.Since(started)
		if dur > scanTimeout {
			return nil, fmt.Errorf("too slow scandeps: dirs:%d ds:%d i:%d n:%d %s %s", len(req.Dirs), scanner.maxDirstack, icnt, ncnt, setupDur, dur)
		}
		// ctx.Err() requires mutex lock, so not call so often.
		if time.Since(lastCtxCheck) > 500*time.Millisecond {
			// check whether ctx is canceled.
			if ctx.Err() != nil {
				return nil, fmt.Errorf("ctx err in scandeps dirs:%d ds:%d i:%d n:%d %s %s: %w", len(req.Dirs), scanner.maxDirstack, icnt, ncnt, setupDur, time.Since(started), ctx.Err())
			}
			lastCtxCheck = time.Now()
		}
		names := scanner.nextInputs(ctx)
		if log.V(1) {
			logNames := names
			clog.Infof(ctx, "try include %q", logNames)
		}
		for _, name := range names {
			ncnt++
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
