// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package scandeps

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	log "github.com/golang/glog"
	"github.com/kelindar/bitmap"

	"go.chromium.org/build/siso/o11y/clog"
)

// scanner is a C++ dependency scanner per request.
type scanner struct {
	pt     *PathTable
	fsview *fsview

	// dir stack for #include "...".
	dirstack    []string
	maxDirstack int

	// input stack
	inputs []string

	// macros for "xxx" or <xxx>.
	// macro may have various values
	// e.g.
	// #ifdef NDEBUG
	// # define MACRO X
	// #else
	// # define MACRO Y
	// #endif
	//  MACRO -> [X, Y]
	macros map[string][]string

	// name -> dirIndex -> visited
	// Uses uint32 for dirIndex.
	included map[string]*bitmap.Bitmap

	// macro name -> value -> used?
	macroUsed map[string]map[string]bool

	// filename -> has #include MACRO
	macroInclude map[string]bool

	// incname -> dirs
	// include of incname should be processed again for dirs
	// as incname contains #include MACRO and MACRO may have been changed.
	macroDirs map[string][]string

	// incname -> index of dirs
	// include of incname was processed by the index of dirs,
	// so no need to process it again.
	nameDirs map[string]int

	// hmap data: incpath -> filenames.
	hmaps map[string][]string

	// allocation
	ds    []string
	names []string

	// stats
	nextInputsCount int // count of nextInputs call
	findCount       int // count of find call
	// duration buckets of find: [<10ms][<100ms][<1s][<10s][<1m][>=1m]
	findDurs   [6]int
	slowest    string // slowest include name
	slowestDur time.Duration
}

func (s *scanner) stats() string {
	return fmt.Sprintf("ds:%d i:%d n:%d find:%d 10ms %d 100ms %d 1s %d 10s %d 1m %d slowest:%s %s",
		s.maxDirstack,
		s.nextInputsCount, s.findCount,
		s.findDurs[0], s.findDurs[1], s.findDurs[2], s.findDurs[3], s.findDurs[4], s.findDurs[5],
		s.slowest, s.slowestDur)
}

func (s *scanner) reset(fsys *filesystem, workspaceRoot string, inputDeps map[string][]string, precomputedTrees []string) {
	s.pt.Reset()
	s.fsview.reset(fsys, workspaceRoot, inputDeps, precomputedTrees)
	s.dirstack = s.dirstack[:0]
	s.maxDirstack = 0
	s.inputs = s.inputs[:0]
	clear(s.macros)
	clear(s.included)
	clear(s.macroUsed)
	clear(s.macroInclude)
	clear(s.macroDirs)
	clear(s.nameDirs)
	clear(s.hmaps)
	s.ds = s.ds[:0]
	s.names = s.names[:0]
	s.nextInputsCount = 0
	s.findCount = 0
	s.findDurs = [6]int{}
	s.slowest = ""
	s.slowestDur = 0
}

var scannerPool = sync.Pool{
	New: func() any {
		return &scanner{
			pt: NewPathTable(),
			fsview: &fsview{
				visited: make(map[string]bool),
				dirs:    make(map[string]bool),
				files:   make(map[string]*scanResult),
				topEnts: make(map[string]*sync.Map),
			},
			macros:       make(map[string][]string),
			included:     make(map[string]*bitmap.Bitmap),
			macroUsed:    make(map[string]map[string]bool),
			macroInclude: make(map[string]bool),
			macroDirs:    make(map[string][]string),
			nameDirs:     make(map[string]int),
			hmaps:        make(map[string][]string),
		}
	},
}

// scanResult contains includes, defines directives required for scandeps
// for an include file.
type scanResult struct {
	mu sync.Mutex

	done     bool
	includes []string
	defines  map[string][]string

	// symlinkTargets are file paths used to access the file
	// resolving symlinks, so need these paths to get access
	// to the requesting include file.
	symlinkTargets []string

	err error
}

func (s *scanResult) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fmt.Sprintf("done=%t includes=%q defines=%q symlinkTargets=%q: %v", s.done, s.includes, s.defines, s.symlinkTargets, s.err)
}

func (fsys *filesystem) scanner(ctx context.Context, workspaceRoot string, inputDeps map[string][]string, precomputedTrees []string) *scanner {
	s := scannerPool.Get().(*scanner)
	s.reset(fsys, workspaceRoot, inputDeps, precomputedTrees)
	for _, dir := range precomputedTrees {
		s.fsview.addDir(ctx, dir, noSearchPath)
	}
	return s
}

func (s *scanner) Close() {
	scannerPool.Put(s)
}

func (s *scanner) pushInputs(ins ...string) {
	for i := len(ins) - 1; i >= 0; i-- {
		s.inputs = append(s.inputs, ins[i])
	}
}

func (s *scanner) pushInputsWithDir(ctx context.Context, dir string, ins ...string) {
	s.pushDir(ctx, dir)
	s.inputs = append(s.inputs, "") // "" will trigger popDir
	s.pushInputs(ins...)
}

func (s *scanner) pushMacroInputs(ctx context.Context, dir string, ins ...string) {
	s.pushDir(ctx, dir)
	s.inputs = append(s.inputs, "") // pop dir
	// only include macro again. i.e. no need to include non-macro path.
	for i := len(ins) - 1; i >= 0; i-- {
		if isMacro(ins[i]) {
			s.inputs = append(s.inputs, ins[i])
		}
	}
}

func (s *scanner) hasInputs() bool {
	return len(s.inputs) > 0
}

func (s *scanner) popInput() string {
	in := s.inputs[len(s.inputs)-1]
	s.inputs = s.inputs[:len(s.inputs)-1]
	return in
}

func (s *scanner) nextInputs(ctx context.Context) ([]string, error) {
	s.nextInputsCount++
	for s.hasInputs() {
		incname := s.popInput()
		if incname == "" {
			s.popDir(ctx)
			continue
		}
		s.names = s.names[:0]
		var err error
		s.names, err = cppExpandMacros(ctx, s.names, incname, s.macros)
		return s.names, err
	}
	return nil, nil
}

func (s *scanner) addInputs(ins ...string) {
	s.inputs = slices.Concat(ins, s.inputs)
}

func (s *scanner) setMacros(macros map[string]string) {
	for k, v := range macros {
		s.macros[k] = append(s.macros[k], v)
	}
}

func (s *scanner) updateMacros(macros map[string][]string) {
	for k, vs := range macros {
		seen := make(map[string]bool)
		for _, v := range s.macros[k] {
			seen[v] = true
		}
		for _, v := range vs {
			if seen[v] {
				continue
			}
			seen[v] = true
			s.macros[k] = append(s.macros[k], v)
		}
	}
}

func (s *scanner) addInclude(ctx context.Context, fname string) {
	// -include or /FI is equivalent with `#include "filename"`
	s.pushInputs(`"` + fname + `"`)
	if log.V(1) {
		clog.Infof(ctx, "include %q", fname)
	}
}

func (s *scanner) addSource(ctx context.Context, fname string) {
	// add dir and include as if #include "basename".
	base := filepath.Base(fname)
	s.pushInputsWithDir(ctx, filepath.ToSlash(filepath.Dir(fname)), `"`+base+`"`)
	if log.V(1) {
		clog.Infof(ctx, "source %q", fname)
	}
}

func (s *scanner) addDir(ctx context.Context, dir string) {
	s.fsview.addDir(ctx, dir, includeSearchPath)
}

func (s *scanner) addQuoteDir(ctx context.Context, dir string) {
	s.fsview.addDir(ctx, dir, quoteSearchPath)
}

func (s *scanner) addFrameworkDir(ctx context.Context, dir string) {
	s.fsview.addDir(ctx, dir, frameworkSearchPath)
}

func (s *scanner) pushDir(ctx context.Context, dir string) {
	s.dirstack = append(s.dirstack, dir)
	if len(s.dirstack) > s.maxDirstack {
		s.maxDirstack = len(s.dirstack)
	}
	s.fsview.addDir(ctx, dir, noSearchPath)
	if log.V(1) {
		clog.Infof(ctx, "push dir <- %s", dir)
	}
}

func (s *scanner) popDir(ctx context.Context) {
	if len(s.dirstack) == 0 {
		return
	}
	dir := s.dirstack[len(s.dirstack)-1]
	s.dirstack = s.dirstack[:len(s.dirstack)-1]
	if log.V(1) {
		clog.Infof(ctx, "pop dir -> %s", dir)
	}
}

func (s *scanner) addHmap(ctx context.Context, hmap string) bool {
	m, ok := s.fsview.getHmap(ctx, hmap)
	if !ok {
		return false
	}
	for k, v := range m {
		s.hmaps[k] = append(s.hmaps[k], v)
	}
	return true
}

func (s *scanner) find(ctx context.Context, name string) (string, error) {
	s.findCount++
	if name == "" {
		return "", io.EOF
	}
	started := time.Now()
	defer func() {
		dur := time.Since(started)
		if dur >= s.slowestDur {
			s.slowest = name
			s.slowestDur = dur
		}
		switch {
		case dur < 10*time.Millisecond:
			s.findDurs[0]++
		case dur < 100*time.Millisecond:
			s.findDurs[1]++
		case dur < 1*time.Second:
			s.findDurs[2]++
		case dur < 10*time.Second:
			s.findDurs[3]++
		case dur < 1*time.Minute:
			s.findDurs[4]++
		default:
			s.findDurs[5]++
		}
	}()
	form := name[0] // '"' or '<'
	name = name[1 : len(name)-1]
	included, ok := s.included[name]
	if !ok {
		included = &bitmap.Bitmap{}
		s.included[name] = included
	}
	if filepath.IsAbs(name) {
		rel, err := filepath.Rel(s.fsview.workspaceRoot, name)
		if err != nil || !filepath.IsLocal(rel) {
			return "", fmt.Errorf("unacceptable abs include path %q: %w", name, err)
		}
		if s.fsview.topEnts["."] == nil {
			s.fsview.addDir(ctx, ".", includeSearchPath)
		}
		incpath, sr, err := s.fsview.get(ctx, ".", rel)
		if err != nil {
			return "", err
		}
		dirIndex := s.pt.Index(".")
		s.macroCheck(ctx, dirIndex, rel, incpath, sr.includes)
		dir := path.Dir(incpath)
		s.updateMacros(sr.defines)
		s.pushInputsWithDir(ctx, dir, sr.includes...)
		return incpath, nil
	}

	ds := s.ds[:0]
	if form == '"' {
		for i := len(s.dirstack) - 1; i >= 0; i-- {
			ds = append(ds, s.dirstack[i])
		}
		ds = append(ds, s.fsview.quotePaths...)
	}
	qi := len(ds)
	ds = append(ds, s.macroDirs[name]...)
	mi := len(ds)
	ds = append(ds, s.fsview.searchPaths[s.nameDirs[name]:]...)
	if len(ds) > 0 {
		if log.V(1) {
			clog.Infof(ctx, "find %q dirs:%d", name, len(ds))
			clog.Infof(ctx, "dirs %d %d %q", qi, mi, ds)
		}
		// TODO: lookup hmap appropriately.
		s.ds = ds
		for i, dir := range ds {
			dirIndex := s.pt.Index(dir)
			if included.Contains(dirIndex) {
				continue
			}
			included.Set(dirIndex)
			if log.V(1) {
				clog.Infof(ctx, "find check %s/%s", dir, name)
			}
			incpath, sr, err := s.fsview.get(ctx, dir, name)
			if log.V(1) {
				clog.Infof(ctx, "fsview get %s/%s -> %q: %v", dir, name, incpath, err)
			}
			if err != nil {
				continue
			}
			s.macroCheck(ctx, dirIndex, name, incpath, sr.includes)
			// `#include "xx"` in incpath may include "xx"
			// from the dir of incpath.
			dir := path.Dir(incpath)
			if log.V(1) {
				clog.Infof(ctx, "find %s -> includes:%q defines:%q", incpath, sr.includes, sr.defines)
			}

			s.updateMacros(sr.defines)
			if i >= qi && i < mi {
				s.pushMacroInputs(ctx, dir, sr.includes...)
			} else {
				s.pushInputsWithDir(ctx, dir, sr.includes...)
			}
			if i > mi {
				s.nameDirs[name] += i - mi
			}
			return incpath, nil
		}
		s.nameDirs[name] = len(ds) - mi
	}
	if len(s.fsview.frameworkPaths) > 0 {
		fwdir, base, found := strings.Cut(name, "/")
		if found {
			// framework import "Foo/Bar.h" -> "Foo.framework/Headers/Bar.h"
			fwname := path.Join(fwdir+".framework", "Headers", base)
			if log.V(1) {
				clog.Infof(ctx, "check framework %s -> %s : %s", name, fwname, s.fsview.frameworkPaths)
			}
			for _, dir := range s.fsview.frameworkPaths {
				dirIndex := s.pt.Index(dir)
				if included.Contains(dirIndex) {
					continue
				}
				included.Set(dirIndex)
				if log.V(1) {
					clog.Infof(ctx, "find check %s/%s", dir, fwname)
				}
				incpath, sr, err := s.fsview.get(ctx, dir, fwname)
				if log.V(1) {
					clog.Infof(ctx, "fsview get %s/%s -> %q: %v", dir, name, incpath, err)
				}
				if err != nil {
					continue
				}
				s.macroCheck(ctx, dirIndex, name, incpath, sr.includes)

				// `#include "xx"` in incpath may include "xx"
				// from the dir of incpath.
				dir := path.Dir(incpath)
				if log.V(1) {
					clog.Infof(ctx, "find %s -> includes:%q defines:%q", incpath, sr.includes, sr.defines)
				}

				s.updateMacros(sr.defines)
				s.pushInputsWithDir(ctx, dir, sr.includes...)
				return incpath, nil
			}
		}
	}
	if log.V(1) {
		clog.Infof(ctx, "find %s %v", name, fs.ErrNotExist)
	}
	return "", fs.ErrNotExist
}

func (s *scanner) macroCheck(ctx context.Context, dirIndex uint32, name, incpath string, incnames []string) {
	for _, iname := range incnames {
		if isMacro(iname) && !s.macroAllUsed(ctx, iname) {
			// incname uses macro.
			// need to try include again
			// because macro value may have been changed.
			if !s.macroInclude[incpath] {
				dir, err := s.pt.Path(dirIndex)
				if err != nil {
					clog.Warningf(ctx, "failed to get path for index %d: %v", dirIndex, err)
					return
				}
				s.macroDirs[name] = append(s.macroDirs[name], dir)
			}
			s.macroInclude[incpath] = true
			s.included[name].Remove(dirIndex)
			return
		}
	}
}

func (s *scanner) macroAllUsed(ctx context.Context, macro string) bool {
	if s.macroUsed[macro] == nil {
		s.macroUsed[macro] = make(map[string]bool)
	}
	values, ok := s.macros[macro]
	if !ok {
		return true
	}
	allUsed := true
	for _, v := range values {
		if !s.macroUsed[macro][v] {
			if log.V(1) {
				clog.Infof(ctx, "macro %s=%s not used yet", macro, v)
			}
			s.macroUsed[macro][v] = true
			allUsed = false
		}
	}
	return allUsed
}

func (s *scanner) results() []string {
	res := s.fsview.results()
	// add all files referred by hmap
	// TODO: add only used header in hmap.
	for _, v := range s.hmaps {
		res = append(res, v...)
	}
	return res
}
