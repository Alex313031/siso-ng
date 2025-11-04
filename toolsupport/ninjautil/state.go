// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjautil

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"go.chromium.org/build/siso/scandeps"
)

var (
	// The default pool is used when no other pool is specified. It has no limit on the number of concurrent actions.
	defaultPool = newPool("", 0)
	// The console pool allows tasks access to stdin/stdout/stderr, thus is limited to one concurrent action.
	consolePool = newPool("console", 1)
	// The phony rule is used to "alias" targets to another name. We implement this as a "rule" that just takes
	// the specified targets as inputs, produces no outputs, runs no command.
	phonyRule = &rule{name: "phony"}
)

// A Pool limits the number of concurrently running actions.
type Pool struct {
	// Name identifies how this pool is referred to from rules or build statements.
	name string
	// Depth is the maximum number of actions allowed to run concurrently.
	depth int
}

// Name returns the pool name.
func (p *Pool) Name() string {
	return p.name
}

// Depth returns the depth of the pool.
func (p *Pool) Depth() int {
	return p.depth
}

func newPool(name string, depth int) *Pool {
	return &Pool{name: name, depth: depth}
}

// State contains all information in a scope about Ninja primitives relevant during execution time,
// such as pools, edges (= actions) and variable bindings.
type State struct {
	// Pools map from pool name to *Pool.
	pools sync.Map // string:poolName -> *Pool

	// Paths map from target name (or pathname used in build's inputs/outputs) to *Node.
	// nodeMap is used during parse and lookup by path, and freeze it in nodes.
	nodeMap *nodeMap
	nodes   []*Node

	scope *fileScope

	mu sync.Mutex // protects edges, defaults, filenames

	// Edges is a list of all actions defined in the Ninja file.
	edges []*Edge
	// Defaults is a list of default targets, i.e. those built if the user does not specify targets to build.
	defaults []*Node
	// Filenames parsed by the parser (e.g. build.ninja and its subninja etc.)
	filenames []string
}

// NewState creates new state.
func NewState() *State {
	s := &State{
		nodeMap: newNodeMap(),
		scope:   newFileScope(nil),
	}
	s.pools.Store("", defaultPool)
	s.pools.Store("console", consolePool)
	return s
}

func (s *State) addPool(pool *Pool) {
	s.pools.Store(pool.name, pool)
}

// LookupPool looks up pool.
func (s *State) LookupPool(poolName string) (*Pool, bool) {
	return s.lookupPool([]byte(poolName))
}

func (s *State) lookupPool(poolName []byte) (*Pool, bool) {
	p, ok := s.pools.Load(string(poolName))
	if !ok {
		return nil, false
	}
	return p.(*Pool), ok
}

// Pools returns a map of pools
func (s *State) Pools() map[string]*Pool {
	m := make(map[string]*Pool)
	s.pools.Range(func(key, value any) bool {
		poolName := key.(string)
		pool := value.(*Pool)
		m[poolName] = pool
		return true
	})
	return m
}

// NumNodes returns a number of nodes.
func (s *State) NumNodes() int {
	return len(s.nodes)
}

// LookupNode returns a node.
func (s *State) LookupNode(id int) (*Node, bool) {
	if id <= 0 || id >= len(s.nodes) {
		return nil, false
	}
	return s.nodes[id], true
}

// LookupNodeByPath returns a node.
func (s *State) LookupNodeByPath(path string) (*Node, bool) {
	n, ok := s.nodeMap.lookup(path)
	return n, ok
}

// AllNodes returns all nodes.
func (s *State) AllNodes() []*Node {
	var nodes []*Node
	for _, node := range s.nodes {
		if node == nil {
			continue
		}
		nodes = append(nodes, node)
	}
	return nodes
}

// RootNodes returns root nodes, that are nodes without output actions.
// (Hence can be considered final artifacts, i.e. other nodes will not use root nodes as inputs.)
func (s *State) RootNodes() ([]*Node, error) {
	var roots []*Node
	for _, e := range s.edges {
		for _, out := range e.outputs {
			if len(out.outs) == 0 {
				roots = append(roots, out)
			}
		}
	}
	if len(s.edges) != 0 && len(roots) == 0 {
		return nil, fmt.Errorf("could not determine root nodes of build graph")
	}
	return roots, nil
}

// DefaultNodes returns default nodes.
func (s *State) DefaultNodes() ([]*Node, error) {
	ns := s.defaults
	if len(ns) > 0 {
		return ns, nil
	}
	return s.RootNodes()
}

// Targets returns target nodes for given args.
// If args is empty, returns default nodes.
// If arg has ^-suffix, it is treated as source of target.
// e.g. "foo.cc^" will be "foo.o", which is generated from foo.cc
func (s *State) Targets(args []string) ([]*Node, error) {
	if len(args) == 0 {
		return s.DefaultNodes()
	}
	var errs []error
	nodes := make([]*Node, 0, len(args))
	for _, t := range args {
		t := filepath.ToSlash(filepath.Clean(t))
		if strings.HasSuffix(t, "^") {
			seen := make(map[string]bool)
			n, ok := s.hatTarget(t, seen)
			if !ok {
				errs = append(errs, fmt.Errorf("unknown target %q", t))
				continue
			}
			outs := n.OutEdges()
			if len(outs) == 0 {
				// TODO(b/289309062): deps log first reverse deps node?
				errs = append(errs, fmt.Errorf("no outs for %q", t))
				continue
			}
			// incompatible with ninja
			// - https://ninja-build.org/manual.html#_running_ninja
			//   ninja just uses the first output of some rule
			//   containing the source
			// siso uses all outputs.
			// b/396522989
			for _, edge := range outs {
				outputs := edge.Outputs()
				if len(outputs) == 0 {
					return nil, fmt.Errorf("out edge of %q has no output", t)
				}
				nodes = append(nodes, outputs[0])
			}
			continue
		}
		n, ok := s.LookupNodeByPath(t)
		if !ok {
			errs = append(errs, fmt.Errorf("unknown target %q", t))
			continue
		}
		nodes = append(nodes, n)
	}
	return nodes, errors.Join(errs...)
}

// Special syntax: "foo.cc^" means "the first output of foo.cc".
// for header file, try to find one of the source file which has
// a direct #include for the header.
// i.e. "foo.h^" will be equivalent with "foo.cc^"
func (s *State) hatTarget(t string, seen map[string]bool) (*Node, bool) {
	ctx := context.Background() // TODO: take from caller.
	if seen[t] {
		return nil, false
	}
	seen[t] = true
	t = strings.TrimSuffix(t, "^")
	n, ok := s.LookupNodeByPath(t)
	if ok {
		return n, true
	}
	switch filepath.Ext(t) {
	case ".h", ".hxx", ".hpp", ".inc":
	default:
		return nil, false
	}
	// special handling for header file.
	_, err := os.Stat(t)
	if err != nil {
		// file not exist
		return nil, false
	}
	incname := filepath.Base(t)
	dirname := filepath.Dir(t)
	// check the file include the header file.
	// TODO: use hashfs here?
	dirents, err := os.ReadDir(dirname)
	if err != nil {
		return nil, false
	}
	files := make(map[string]bool)
	for _, dirent := range dirents {
		files[dirent.Name()] = true
	}
	tbase := strings.TrimSuffix(t, filepath.Ext(t))

	checkNode := func(fname string) (*Node, bool) {
		buf, err := os.ReadFile(fname)
		if err != nil {
			return nil, false
		}
		includes, _, err := scandeps.CPPScan(ctx, fname, buf)
		if err != nil {
			return nil, false
		}
		for _, inc := range includes {
			// inc should be `"foo.h"` or `<foo.h>`
			switch inc[len(inc)-1] {
			case '"', '>':
				inc = inc[1 : len(inc)-1]
			default:
				continue
			}
			if filepath.Base(inc) == incname {
				n, ok := s.LookupNodeByPath(fname)
				if ok {
					return n, true
				}
				switch filepath.Ext(fname) {
				case ".h", ".hxx", ".hpp", ".inc":
					n, ok := s.hatTarget(fname, seen)
					if ok {
						return n, true
					}
				}
			}
		}
		return nil, false
	}
	// prefer same stem, test or unittest.
	sourceExts := []string{".cc", "_test.cc", "_unittest.cc", ".c", ".cxx", ".cpp", ".m", ".mm", ".S"}
	for _, ext := range sourceExts {
		tt := tbase + ext
		delete(files, filepath.Base(tt))
		n, ok := checkNode(tt)
		if ok {
			return n, true
		}
	}
	var filenames []string
	for fname := range files {
		filenames = append(filenames, fname)
	}
	sort.Strings(filenames)
	for _, fname := range filenames {
		n, ok := checkNode(filepath.ToSlash(filepath.Join(dirname, fname)))
		if ok {
			return n, true
		}
	}
	return nil, false
}

// SpellcheckTarget returns the most similar target from given target.
func (s *State) SpellcheckTarget(t string) (string, error) {
	const maxEditDistance = 3
	minDistance := maxEditDistance + 1
	var similar, submatch string
	for _, n := range s.nodes {
		if n == nil {
			continue
		}
		d := editDistance(t, n.Path(), maxEditDistance)
		if d < minDistance {
			minDistance = d
			similar = n.Path()
		}
		if (submatch == "" || len(n.Path()) < len(submatch)) && strings.Contains(n.Path(), t) {
			submatch = n.Path()
		}
	}
	if similar != "" {
		return similar, nil
	}
	if submatch != "" {
		return submatch, nil
	}
	return "", fmt.Errorf("no target similar to %q in edit distance %d, or contains %q as substring", t, maxEditDistance, t)
}

func (s *State) mergeFileState(fstate *fileState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.edges = append(s.edges, fstate.edges...)
	s.defaults = append(s.defaults, fstate.defaults...)
	s.filenames = append(s.filenames, fstate.filenames...)
}

// PhonyNodes returns phony's output nodes.
func (s *State) PhonyNodes() []*Node {
	var phony []*Node
	for _, e := range s.edges {
		if e.IsPhony() {
			phony = append(phony, e.Outputs()...)
		}
	}
	return phony
}

// AddBinding adds bindings.
func (s *State) AddBinding(name, value string) {
	s.scope.setVar([]byte(name), evalString{v: []byte(value)})
}

// Binding returns top level binding.
func (s *State) Binding(name string) string {
	val, ok := s.scope.lookupVar(-1, []byte(name))
	if !ok {
		return ""
	}
	var buf bytes.Buffer
	value, err := evaluate(s.scope, &buf, val)
	if err != nil {
		return ""
	}
	return string(value)
}

// Filenames returns files parsed by the parser (e.g. build.ninja and its subninja etc.)
func (s *State) Filenames() []string {
	return s.filenames
}
