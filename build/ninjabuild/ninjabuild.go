// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package ninjabuild provides build steps by ninja.
package ninjabuild

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"go.chromium.org/build/hashigo/digest"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/build/buildconfig"
	"go.chromium.org/build/siso/execute"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/o11y/trace"
	"go.chromium.org/build/siso/path"
	"go.chromium.org/build/siso/toolsupport/ninjautil"
)

var (
	errShellPathNotCleanOrLocal = errors.New("shell path must be clean and local")
	errInvalidShellBasename     = errors.New("shell basename must be 'sh' or 'bash'")
	errShellNotExecutable       = errors.New("shell must be marked executable")
)

// Graph holds build graph, i.e. all step defs described in build.ninja.
type Graph struct {
	fname string

	visited map[*ninjautil.Edge]*build.Edge

	globals *globals
}

type globals struct {
	nstate *ninjautil.State

	path    *build.Path
	hashFS  *hashfs.HashFS
	depsLog *DepsLog

	buildConfig *buildconfig.Config
	stepConfig  *StepConfig

	shellCmd    string
	shellInputs []path.Path

	// node id -> path.Path
	targetPaths []path.Path

	// deps path id -> *depsPath
	depsPaths []atomic.Pointer[depsPath]

	// node id -> edgeRuleHolder that produces the output
	edgeRules []edgeRuleHolder

	phony map[string]bool

	// caseSensitives lists all case sensitive input filenames.
	caseSensitives map[string][]path.Path

	// edge will be associated with the gn target.
	gnTargets map[*ninjautil.Edge]gnTarget

	// executables are files that need to set executable bit on Linux worker.
	// This field is used to upload Linux executables from Windows host.
	executables map[string]bool
}

type gnTarget struct {
	target string
	rule   string
	pref   int
}

var gnTargetsRulePrefs = []string{"phony", "stamp", "solink", "alink", "link"}

// rulePref returns preference for gn targets of the rule.
// 0 is most preferred. the bigger, the less preferred.
// rule may have target arch prefix, so just use the last term separated by _.
// e.g. "clang_newlib_x64_solink" -> pref of "solink".
func rulePref(rule string) int {
	r := strings.Split(rule, "_")
	rule = r[len(r)-1]
	for i, p := range gnTargetsRulePrefs {
		if p == rule {
			return i
		}
	}
	return len(gnTargetsRulePrefs)
}

func (g gnTarget) String() string {
	return g.target
}

// NewStepConfig creates new *StepConfig and stores it in .siso_config
// and .siso_filegroups.
func NewStepConfig(ctx context.Context, fn digest.Function, config *buildconfig.Config, p *build.Path, fname, stateDir string) (*StepConfig, error) {
	defer trace.Begin(ctx, "ninjabuild.NewStepConfig").End()
	// Use a temporary HashFS for config initialization and updating filegroups.
	// Config initialization is mostly CPU-bound (loading Starlark scripts, parsing config),
	// while HashFS initialization is Disk-bound (scanning directory state, computing digests).
	// Isolating config parsing avoids blocking the CPU execution phase on the long
	// background disk scan of the main HashFS, allowing them to run concurrently.
	tempHashFS, err := hashfs.New(ctx, hashfs.Option{DigestFunction: fn})
	if err != nil {
		return nil, err
	}
	defer tempHashFS.Close(ctx)
	s, strict, err := config.Init(ctx, tempHashFS, p)
	if err != nil {
		return nil, err
	}
	stepConfig := &StepConfig{}
	dec := json.NewDecoder(strings.NewReader(s))
	if strict {
		dec.DisallowUnknownFields()
	}
	err = dec.Decode(stepConfig)
	if err != nil {
		clog.Errorf(ctx, "Failed to parse init output:\n%s", s)
		return nil, fmt.Errorf("failed to parse init output: %w", err)
	}

	clog.Infof(ctx, "loaded %d platforms / %d input deps / %d rules", len(stepConfig.Platforms), len(stepConfig.InputDeps), len(stepConfig.Rules))
	stepConfig.StateDir = stateDir
	err = stepConfig.Init(ctx)
	if err != nil {
		return nil, err
	}
	buf, err := json.MarshalIndent(stepConfig, "", " ")
	if err != nil {
		return nil, fmt.Errorf("failed to marshal config: %w", err)
	}
	configFilename := filepath.Join(stateDir, ".siso_config")
	err = os.WriteFile(configFilename, buf, 0644)
	if err != nil {
		return nil, err
	}
	clog.Infof(ctx, "save to %s", configFilename)
	err = updateFilegroups(ctx, config, p, tempHashFS, fname, stepConfig)
	if err != nil {
		return nil, err
	}
	return stepConfig, nil
}

// updateFilegroups updates filegroups in *StepConfig and stores it in .siso_filegroups.
func updateFilegroups(ctx context.Context, config *buildconfig.Config, buildPath *build.Path, hashFS *hashfs.HashFS, fname string, sc *StepConfig) error {
	var fnameTime, fgTime time.Time
	fi, err := os.Stat(fname)
	if err != nil {
		return err
	}
	fnameTime = fi.ModTime()
	filegroupsFilename := filepath.Join(sc.StateDir, ".siso_filegroups")
	fi, err = os.Stat(filegroupsFilename)
	if err == nil {
		fgTime = fi.ModTime()
	}
	var fg buildconfig.Filegroups
	// initializes fg with valid cached result.
	if fnameTime.Before(fgTime) {
		buf, err := os.ReadFile(filegroupsFilename)
		if err != nil {
			clog.Warningf(ctx, "Failed to load %s: %v", filegroupsFilename, err)
		} else {
			err = json.Unmarshal(buf, &fg)
			if err != nil {
				clog.Warningf(ctx, "Failed to unmarshal filegroups: %v", err)
			} else {
				clog.Infof(ctx, "loaded %d filegroups", len(fg.Filegroups))
			}
		}
	}
	started := time.Now()
	defer func() {
		clog.Infof(ctx, "update filegroups in %s", time.Since(started))
	}()
	fg, err = config.UpdateFilegroups(ctx, hashFS, buildPath, fg)
	if err != nil {
		return err
	}
	clog.Infof(ctx, "updated %d filegroups", len(fg.Filegroups))
	buf, err := json.MarshalIndent(fg, "", " ")
	if err != nil {
		return fmt.Errorf("failed to marshal filegroups: %w", err)
	}
	err = os.WriteFile(filegroupsFilename, buf, 0644)
	if err != nil {
		return err
	}
	clog.Infof(ctx, "save to %s", filegroupsFilename)
	return sc.UpdateFilegroups(ctx, fg.Filegroups)
}

// Load loads build.ninja file specified by fname and returns parsed states.
func Load(ctx context.Context, fname string, buildPath *build.Path) (*ninjautil.State, error) {
	defer trace.Begin(ctx, "ninjabuild.Load").End()
	started := time.Now()
	state := ninjautil.NewState()
	state.AddBinding("workspace_root", buildPath.WorkspaceRoot)
	state.AddBinding("working_directory", buildPath.BaseDir)
	p := ninjautil.NewManifestParser(state)
	err := p.Load(ctx, fname)
	if err != nil {
		return nil, fmt.Errorf("failed to load %s: %w", fname, err)
	}
	if err := validateDirOutputs(state); err != nil {
		return nil, err
	}
	clog.Infof(ctx, "load %s %s", fname, time.Since(started))
	return state, nil
}

// validateDirOutputs rejects a graph where any output is nested under a
// directory output, even from the same step: a directory output owns its whole
// tree, so its members must not be declared as separate outputs.
func validateDirOutputs(state *ninjautil.State) error {
	// No directory outputs: nothing to check, and no node scan.
	dirNodes := state.DirOutputs()
	if len(dirNodes) == 0 {
		return nil
	}
	// Index the directory-output paths in a byte-trie so each node is matched
	// against all of them in one prefix walk.
	root := &dirTrieNode{}
	for _, d := range dirNodes {
		root.insert(d.Path())
	}
	numNodes := state.NumNodes()
	for id := range numNodes {
		n, ok := state.LookupNode(id)
		if !ok {
			continue
		}
		// Only an output (a node with a producing edge) can overlap a dir
		// output; a source under one is left alone.
		if _, ok := n.InEdge(); !ok {
			continue
		}
		if dirPath := root.containingDir(n.Path()); dirPath != "" {
			return fmt.Errorf("output %q is declared under directory output %q; a directory output owns its whole tree, so its members must not be declared as separate outputs", n.Path(), dirPath)
		}
	}
	return nil
}

// dirTrieNode is a byte-trie over directory-output paths (each ending in "/").
type dirTrieNode struct {
	b        byte
	children []*dirTrieNode
	isDir    bool // a directory output ends at this node
}

func (t *dirTrieNode) child(b byte) *dirTrieNode {
	for _, c := range t.children {
		if c.b == b {
			return c
		}
	}
	return nil
}

func (t *dirTrieNode) insert(path string) {
	cur := t
	for i := range len(path) {
		next := cur.child(path[i])
		if next == nil {
			next = &dirTrieNode{b: path[i]}
			cur.children = append(cur.children, next)
		}
		cur = next
	}
	cur.isDir = true
}

// containingDir returns a directory output (path ends in "/") that strictly
// contains p, or "" if none.
func (t *dirTrieNode) containingDir(p string) string {
	cur := t
	for i := range len(p) {
		next := cur.child(p[i])
		if next == nil {
			return ""
		}
		cur = next
		if cur.isDir && i+1 < len(p) {
			// p[:i+1] reached a dir-output terminal, so it strictly contains p.
			return p[:i+1]
		}
	}
	return ""
}

// NewGraph creates new Graph from fname (usually "build.ninja") with stepConfig.
func NewGraph(ctx context.Context, fname string, nstate *ninjautil.State, config *buildconfig.Config, p *build.Path, hashFS *hashfs.HashFS, stepConfig *StepConfig, depsLog *DepsLog) (*Graph, error) {
	defer trace.Begin(ctx, "ninjabuild.NewGraph").End()
	graph := &Graph{
		fname: fname,

		visited: make(map[*ninjautil.Edge]*build.Edge),

		globals: &globals{
			nstate:         nstate,
			path:           p,
			hashFS:         hashFS,
			depsLog:        depsLog,
			buildConfig:    config,
			stepConfig:     stepConfig,
			targetPaths:    make([]path.Path, nstate.NumNodes()),
			depsPaths:      make([]atomic.Pointer[depsPath], depsLog.NumPaths()),
			edgeRules:      make([]edgeRuleHolder, nstate.NumNodes()),
			phony:          make(map[string]bool),
			caseSensitives: make(map[string][]path.Path),
			gnTargets:      make(map[*ninjautil.Edge]gnTarget),
			executables:    make(map[string]bool),
		},
	}
	err := graph.initGlobals(ctx)
	if err != nil {
		return nil, err
	}
	return graph, nil
}

// StateDir returns state dir.
func (g *Graph) StateDir() string {
	return g.globals.stepConfig.StateDir
}

// Shell returns the hermetic shell binary path, or empty if not set.
func (g *Graph) Shell() string {
	return g.globals.stepConfig.Shell
}

func (g *Graph) setShell(ctx context.Context) error {
	shell := g.globals.stepConfig.Shell
	if shell == "" {
		g.globals.shellCmd = ""
		g.globals.shellInputs = nil
		return nil
	}
	shellCmd, shellInput, err := resolveShellPath(ctx, g.globals.hashFS, g.globals.path, shell)
	if err != nil {
		return err
	}
	g.globals.shellCmd = shellCmd
	shellInputs := []path.Path{shellInput}
	shellInputs = g.globals.stepConfig.ExpandInputs(ctx, g.globals.path, g.globals.hashFS, shellInputs)
	if !slices.Contains(shellInputs, shellInput) {
		shellInputs = append([]path.Path{shellInput}, shellInputs...)
	}
	g.globals.shellInputs = shellInputs
	return nil
}

func resolveShellPath(ctx context.Context, hashFS *hashfs.HashFS, p *build.Path, shell string) (string, path.Path, error) {
	if shell == "" {
		return "", "", nil
	}
	if runtime.GOOS == "windows" {
		return "", "", fmt.Errorf("shell cannot be specified on windows, got %q: %w", shell, errors.ErrUnsupported)
	}
	if shell != filepath.Clean(shell) || !filepath.IsLocal(shell) {
		return "", "", fmt.Errorf("%w, got: %q", errShellPathNotCleanOrLocal, shell)
	}
	// Enforce that the shell's basename is either "sh" or "bash" to match the
	// hardcoded names in normalizeArgs().
	// TODO: b/432599730 - remove the workaround
	if base := filepath.Base(shell); base != "sh" && base != "bash" {
		return "", "", fmt.Errorf("%w, got: %q", errInvalidShellBasename, base)
	}

	shellPath := path.New(shell)

	if info, err := hashFS.Stat(ctx, p.WorkspaceRoot, shellPath); err != nil {
		return "", "", fmt.Errorf("could not stat shell %q: %w", shell, err)
	} else if info.Mode()&0o111 == 0 {
		return "", "", fmt.Errorf("shell %q: %w (mode: %O)", shell, errShellNotExecutable, info.Mode())
	}

	shellCmd := p.MaybeToRelative(ctx, shell)

	// Need a / in the path so that execve doesn't do $PATH lookup
	if !strings.Contains(shellCmd, "/") {
		shellCmd = "./" + shellCmd
	}

	return shellCmd, shellPath, nil
}

// Reload reloads hashfs, filegroups and build.ninja.
func (g *Graph) Reload(ctx context.Context) error {
	stateDir := g.globals.stepConfig.StateDir
	var newState *ninjautil.State
	eg, ctx := errgroup.WithContext(ctx)
	eg.Go(func() error {
		// need to refresh cached entries as `gn gen` updated files
		// but ninja manifest doesn't know what files are updated.
		return g.globals.hashFS.Refresh(ctx)
	})
	eg.Go(func() error {
		var err error
		g.globals.stepConfig, err = NewStepConfig(ctx, g.globals.hashFS.DigestFunction(), g.globals.buildConfig, g.globals.path, g.fname, stateDir)
		return err
	})
	eg.Go(func() error {
		var err error
		newState, err = Load(ctx, g.fname, g.globals.path)
		return err
	})
	err := eg.Wait()
	if err != nil {
		return err
	}
	g.globals.nstate = newState
	return g.reset(ctx)
}

// Reset resets graph status and hashfs.
func (g *Graph) Reset(ctx context.Context) error {
	// need to refresh hashfs to clear dirty state.
	err := g.globals.hashFS.Refresh(ctx)
	if err != nil {
		return err
	}
	return g.reset(ctx)
}

func (g *Graph) reset(ctx context.Context) error {
	g.visited = make(map[*ninjautil.Edge]*build.Edge)
	g.globals.depsLog.Reset()
	g.globals.targetPaths = make([]path.Path, g.globals.nstate.NumNodes())
	g.globals.depsPaths = make([]atomic.Pointer[depsPath], g.globals.depsLog.NumPaths())
	g.globals.edgeRules = make([]edgeRuleHolder, g.globals.nstate.NumNodes())
	g.globals.phony = make(map[string]bool)
	g.globals.caseSensitives = make(map[string][]path.Path)
	g.globals.gnTargets = make(map[*ninjautil.Edge]gnTarget)
	g.globals.executables = make(map[string]bool)
	return g.initGlobals(ctx)
}

func (g *Graph) initGlobals(ctx context.Context) error {
	// initialize caseSensitives.
	for _, f := range g.globals.stepConfig.CaseSensitiveInputs {
		cif := strings.ToLower(f)
		g.globals.caseSensitives[cif] = append(g.globals.caseSensitives[cif], path.FromClean(f))
	}
	// initialize :inputs label
	inputsLabels := make(map[string]bool)
	for _, v := range g.globals.stepConfig.InputDeps {
		for _, s := range v {
			if !strings.HasSuffix(s, ":inputs") {
				continue
			}
			inputsLabels[s] = true
		}
	}
	for _, label := range slices.Sorted(maps.Keys(inputsLabels)) {
		if _, ok := g.globals.stepConfig.InputDeps[label]; ok {
			// already registered
			clog.Warningf(ctx, "%q already in input_deps", label)
			continue
		}
		target := g.globals.path.MaybeToRelative(ctx, strings.TrimSuffix(label, ":inputs"))
		n, ok := g.globals.nstate.LookupNodeByPath(target)
		if !ok {
			clog.Warningf(ctx, "no target for %q (%q)", label, target)
			continue
		}
		edge, ok := n.InEdge()
		if !ok {
			clog.Warningf(ctx, "no inputs for %q (%q)", label, target)
			continue
		}
		visited := make(map[*ninjautil.Edge]bool)
		seen := make(map[path.Path]bool)
		stepInputs := replacePhony(ctx, g.globals, visited, seen, g.globals.targetPath(n), edge, false, nil)
		inputs := make([]string, 0, len(stepInputs))
		for _, n := range stepInputs {
			inputs = append(inputs, string(n))
		}
		clog.Infof(ctx, "add %q (%q) in input_deps: %d", label, target, len(inputs))
		g.globals.stepConfig.InputDeps[label] = inputs
	}
	// initialize executables.
	hfsExecutables := make(map[path.Path]bool)
	for _, f := range g.globals.stepConfig.Executables {
		g.globals.executables[f] = true
		var absPath path.Path
		if filepath.IsAbs(f) {
			absPath = path.New(f)
		} else {
			absPath = path.JoinRoot(g.globals.path.WorkspaceRoot, path.New(f))
		}
		hfsExecutables[absPath] = true
		clog.Infof(ctx, "set executable %q %q", f, absPath)
	}
	g.globals.hashFS.SetExecutables(ctx, hfsExecutables)

	err := g.setShell(ctx)
	if err != nil {
		return err
	}

	// TODO: find a better way to distinguish between gn build.ninja and other generator's one.
	if _, ok := g.globals.nstate.LookupNodeByPath(g.fname); !ok {
		// no node for g.fname (e.g. "build.ninja").
		// so probably not build.ninja generated by gn.
		clog.Infof(ctx, "no %s target. not gn?", g.fname)
		return nil
	}

	// infer gn target.
	// gn target will be node that is phony target and contains ":"
	// in its name.
	// e.g.
	//
	//   build base$:base: phony obj/base/libbase.a
	//
	//   build obj/base/libbase.a: alink obj/base/base/allocator_check.o ...
	//   build obj/base/base/allocator_check.o: cxx ../../base/allocator/allocator_check.cc
	//
	// A edge generating the `target` (e.g. alink for `obj/base/libbase.a`)
	// will have gn target `base:base`.
	// Also explicit inputs of the edge also have the gn target.
	// e.g. cxx for `obj/base/base/allocator_check.o` have `base:base`.
	//
	// If there are several paths from gn targets, choose based on
	// rule's preference.
	//
	// e.g.
	//   build v8/tools/wasm$:wami: phony ./wami
	//
	//   build ./wami: link obj/v8/tools/wasm/wami/module-inspector.o ..
	//       ... obj/v8/v8_compiler/access-builder.o ..
	//
	//   build v8$:v8_compiler: phony obj/v8/v8_compiler.stamp
	//   build obj/v8/v8_compiler.stamp: stamp obj/v8/v8_compiler/access-builder.o o ..
	//
	// link for ./wami:        gn target v8/tools/wasm:wami
	// cxx for obj/v8/tools/wasm/wami/module-inspector.o:
	//                         gn target v8/tools/wasm:wami
	// stamp for obj/v8/v8_compiler.stamp:
	//                         gn target v8:v8_compiler
	// cxx for obj/v8/v8_compiler/access-builder.o:
	//                         gn target v8:v8_compiler
	//                         (prefer stamp over link)
	started := time.Now()
	nGNTargets := 0
	for _, p := range g.globals.nstate.PhonyNodes() {
		target := p.Path()
		if !strings.Contains(target, ":") {
			continue
		}
		e, ok := p.InEdge()
		if !ok {
			continue
		}
		nGNTargets++
		// gnTarget's phony inputs will use the gn target.
		for _, n := range e.Ins() {
			ne, ok := n.InEdge()
			if !ok {
				continue
			}
			g.globals.gnTargets[ne] = gnTarget{
				target: target,
				rule:   "phony",
				pref:   0,
			}

			rule := ne.RuleName()
			pref := rulePref(rule)
			for _, cn := range ne.Ins() {
				ce, ok := cn.InEdge()
				if !ok {
					continue
				}
				gt, ok := g.globals.gnTargets[ce]
				if ok && gt.pref <= pref {
					continue
				}
				g.globals.gnTargets[ce] = gnTarget{
					target: target,
					rule:   rule,
					pref:   pref,
				}
			}
		}
	}
	clog.Infof(ctx, "gn_targets=%d edges=%d in %s", nGNTargets, len(g.globals.gnTargets), time.Since(started))
	return nil
}

// Filename returns filename of build manifest (e.g. build.ninja).
func (g *Graph) Filename() string {
	return g.fname
}

// Binding returns top level binding.
func (g *Graph) Binding(name string) string {
	return g.globals.nstate.Binding(name)
}

// Filenames returns filenames of build manifest (all files loaded by build.ninja).
func (g *Graph) Filenames() []string {
	return g.globals.nstate.Filenames()
}

// ConfigProperties returns configuration properties.
func (g *Graph) ConfigProperties() map[string]string {
	return g.globals.stepConfig.Properties
}

func (g *Graph) NumTargets() int {
	return g.globals.nstate.NumNodes()
}

// Targets returns targets for ninja args.
// If args is not given, returns default build targets.
// It may return known targets even if some target is unknown.
func (g *Graph) Targets(ctx context.Context, args ...string) ([]build.Target, error) {
	nodes, err := g.globals.nstate.Targets(args)
	targets := make([]build.Target, 0, len(nodes))
	names := make([]string, 0, len(nodes))
	for _, n := range nodes {
		targets = append(targets, build.Target(n.ID()))
		names = append(names, n.Path())
	}
	clog.Infof(ctx, "targets %q -> %q: %v", args, names, err)
	return targets, err
}

// LookupTarget returns the target ID for a build-dir-relative target path.
func (g *Graph) LookupTarget(target string) (build.Target, bool) {
	n, ok := g.globals.nstate.LookupNodeByPath(target)
	if !ok {
		return 0, false
	}
	return build.Target(n.ID()), true
}

// SpellcheckTarget finds closest target name from t.
func (g *Graph) SpellcheckTarget(t string) (string, error) {
	return g.globals.nstate.SpellcheckTarget(t)
}

// TargetPath returns workspace relative path of the target.
func (g *Graph) TargetPath(ctx context.Context, target build.Target) (string, error) {
	node, ok := g.globals.nstate.LookupNode(int(target))
	if !ok {
		return "", fmt.Errorf("invalid target %v", target)
	}
	return string(g.globals.targetPath(node)), nil
}

func (g *globals) targetPath(node *ninjautil.Node) path.Path {
	p := g.targetPaths[node.ID()]
	if p != "" {
		return p
	}
	raw := node.Path()
	if !filepath.IsAbs(raw) {
		p = path.Path(filepath.ToSlash(filepath.Join(g.path.BaseDir, raw)))
	} else {
		p = path.Path(raw)
	}
	// filepath.Join strips a directory artifact's trailing slash; re-append it.
	if strings.HasSuffix(raw, "/") && !strings.HasSuffix(string(p), "/") {
		p += "/"
	}
	g.targetPaths[node.ID()] = p
	return p
}

// Edge returns a new Edge to build target (workspace relative), needed for next.
// top-level target will use nil for next.
func (g *Graph) Edge(ctx context.Context, target build.Target, next build.StepDef) (*build.Edge, error) {
	n, ok := g.globals.nstate.LookupNode(int(target))
	if !ok {
		return nil, build.ErrNoTarget
	}
	edge, ok := n.InEdge()
	if !ok {
		return nil, build.ErrTargetIsSource
	}
	v := g.visited[edge]
	if v != nil {
		return v, build.ErrDuplicateStep
	}
	if edge.IsPhony() {
		g.globals.phony[string(g.globals.targetPath(n))] = true
	}
	stepDef := g.newStepDef(ctx, edge, next)
	edgeInputs := edge.TriggerInputs()
	inputs := make([]build.Target, 0, len(edgeInputs))
	for _, in := range edgeInputs {
		inputs = append(inputs, build.Target(in.ID()))
	}
	edgeOrderOnly := edge.Inputs()[len(edgeInputs):]
	orderOnly := make([]build.Target, 0, len(edgeOrderOnly))
	for _, in := range edgeOrderOnly {
		orderOnly = append(orderOnly, build.Target(in.ID()))
	}
	edgeOutputs := edge.Outputs()
	outputs := make([]build.Target, 0, len(edgeOutputs))
	for _, out := range edgeOutputs {
		outputs = append(outputs, build.Target(out.ID()))
	}
	edgeValidations := edge.Validations()
	validations := make([]build.Target, 0, len(edgeValidations))
	for _, v := range edgeValidations {
		validations = append(validations, build.Target(v.ID()))
	}
	edgeStepDef := &build.Edge{
		StepDef:     stepDef,
		Inputs:      inputs,
		OrderOnly:   orderOnly,
		Outputs:     outputs,
		Validations: validations,
	}
	g.visited[edge] = edgeStepDef
	return edgeStepDef, nil
}

// InputDeps returns input deps.
func (g *Graph) InputDeps(ctx context.Context) map[string][]string {
	return g.globals.stepConfig.InputDeps
}

// InputsRequiringClangScandeps returns inputs that requires clang scandeps.
func (g *Graph) InputsRequiringClangScandeps(ctx context.Context) []string {
	if g.globals.stepConfig.Scandeps == nil {
		return nil
	}
	return g.globals.stepConfig.Scandeps.InputsRequiringClang
}

// ClangScandeps returns clang scandeps mode.
func (g *Graph) ClangScandeps(ctx context.Context) string {
	if g.globals.stepConfig.Scandeps == nil {
		return ""
	}
	return g.globals.stepConfig.Scandeps.UseClang
}

// EnvConfig returns environment configuration for build steps.
func (g *Graph) EnvConfig(ctx context.Context) *build.EnvConfig {
	return g.globals.stepConfig.Env
}

// StepLimits returns a map of maximum number of concurrent steps by pool name.
func (g *Graph) StepLimits(ctx context.Context) map[string]int {
	m := make(map[string]int)
	for k, v := range g.globals.nstate.Pools() {
		if v == nil || v.Depth() == 0 {
			continue
		}
		m[k] = v.Depth()
	}
	return m
}

// CleanDead cleans dead generated files, and
// returns number of removed files and number of last generated files.
func (g *Graph) CleanDead(ctx context.Context) (int, int, error) {
	started := time.Now()
	var deads []path.Path
	dir := path.New(g.globals.path.AbsBase())
	genFiles := g.globals.hashFS.PreviouslyGeneratedFiles()

	// Normalize generated files to workspace-relative slash paths, dropping
	// any that escape the out dir (e.g. through a symlinked directory;
	// b/336667052).
	rels := make([]path.Path, 0, len(genFiles))
	for _, genFile := range genFiles {
		rel, err := genFile.Rel(dir)
		if err != nil {
			return len(deads), len(genFiles), err
		}
		if !filepath.IsLocal(string(rel)) {
			clog.Warningf(ctx, "skip generated file not in out dir: %s", genFile)
			continue
		}
		rels = append(rels, rel)
	}

	// Judge only the outermost entries: a directory output is owned as a
	// unit, so its contents must never be judged on their own. RemoveAll on
	// a dead root takes its whole subtree. Sort only for deterministic deads.
	slices.Sort(rels)
	for _, rel := range execute.OutermostPaths(rels) {
		if g.isDead(string(rel)) {
			// RemoveAll so a dead dir output goes with its contents; a plain
			// Remove would fail on a non-empty directory.
			err := g.globals.hashFS.RemoveAll(ctx, string(dir), rel)
			if err != nil {
				// Best-effort: a dead output that can't be removed (e.g. held
				// open by another process on Windows) is left behind rather
				// than failing the build.
				clog.Warningf(ctx, "cleandead: leave %s: %v", rel, err)
				continue
			}
			deads = append(deads, rel)
			clog.Infof(ctx, "deadfile %s", rel)
		}
	}
	var err error
	if len(deads) > 0 {
		err = g.globals.hashFS.Flush(ctx, string(dir), deads)
	}
	if err != nil {
		clog.Warningf(ctx, "cleandead %d/%d %s: %v", len(deads), len(genFiles), time.Since(started), err)
	} else {
		clog.Infof(ctx, "cleandead %d/%d %s", len(deads), len(genFiles), time.Since(started))
	}
	return len(deads), len(genFiles), err
}

// isDead reports fname is dead generated file or not.
// i.e. it is considered as dead if one of the following conditions is met.
//   - it is not used in current ninja build graph.
//   - if it is not generated by any step and not used by any steps.
//
// https://github.com/ninja-build/ninja/blob/a524bf3f6bacd1b4ad85d719eed2737d8562f27a/src/clean.cc#L141
func (g *Graph) isDead(fname string) bool {
	n, ok := g.globals.nstate.LookupNodeByPath(fname)
	if !ok {
		// Directory-output nodes are keyed with a trailing slash ("gen/")
		// but reach here slash-stripped ("gen"). Retry the slash form so a
		// referenced dir node is not reported dead and RemoveAll'd.
		n, ok = g.globals.nstate.LookupNodeByPath(fname + "/")
		if !ok {
			return true
		}
	}
	_, ok = n.InEdge()
	return !ok && !n.HasOut()
}
