// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninjabuild

import (
	"bufio"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"iter"
	"maps"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	log "github.com/golang/glog"
	"github.com/google/uuid"

	"go.chromium.org/build/hashigo/digest"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/execute"
	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/o11y/trace"
	"go.chromium.org/build/siso/path"
	"go.chromium.org/build/siso/toolsupport/cmdutil"
	"go.chromium.org/build/siso/toolsupport/makeutil"
	"go.chromium.org/build/siso/toolsupport/ninjautil"
	"go.chromium.org/build/siso/toolsupport/shutil"
)

// StepDef is a ninja build step definition.
type StepDef struct {
	id   string
	edge *ninjautil.Edge
	next build.StepDef

	shellOnce sync.Once
	needShell bool

	ruleReady bool
	rule      StepRule
	pure      bool

	// from depfile/depslog, parsed once under depsOnce so concurrent
	// callers (inputMtime, step.Inputs) share a single depfile read.
	depsOnce sync.Once
	deps     iter.Seq[path.Path] // workspace relative
	deperr   error

	envfile string // for ninja -t msvc -e <envfile> --

	globals *globals
}

type edgeRuleHolder struct {
	mu       sync.Mutex
	edgeRule *edgeRule
}

func (h *edgeRuleHolder) reset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.edgeRule = nil
}

func (h *edgeRuleHolder) set(er *edgeRule) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.edgeRule = er
}

func (h *edgeRuleHolder) get() *edgeRule {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.edgeRule
}

type edgeRule struct {
	edge *ninjautil.Edge

	mu          sync.Mutex
	ruleChecked bool
	// replace, accumulate are valid once ruleChecked.
	// these are StepConfig's Replace and Accumulate respectively.
	replace, accumulate bool
}

// ensure ensures edgeRule checks pure/replace/accumulate of siso rule for the edge.
// siso rule might not be checked for the edge when it was skipped.
func (er *edgeRule) ensure(ctx context.Context, globals *globals) (gotRule bool, rule StepRule, pure bool) {
	er.mu.Lock()
	defer er.mu.Unlock()
	if er.ruleChecked {
		return false, StepRule{}, false
	}
	er.ruleChecked = true
	rule, pure = globals.stepConfig.Lookup(ctx, globals.path, er.edge)
	if !pure && er.edge.Binding("solibs") == "" {
		// remove edgeRule for all outputs of the edge from edgeRules
		// for non-pure and no solibs step.
		for _, output := range er.edge.Outputs() {
			globals.edgeRules[output.ID()].reset()
		}
		return true, rule, false
	}
	er.replace = rule.Replace
	er.accumulate = rule.Accumulate
	return true, rule, true
}

var randReader = bufio.NewReader(rand.Reader)

func (g *Graph) newStepDef(ctx context.Context, edge *ninjautil.Edge, next build.StepDef) *StepDef {
	id := uuid.Must(uuid.NewRandomFromReader(randReader)).String()
	stepDef := &StepDef{
		id:      id,
		edge:    edge,
		next:    next,
		globals: g.globals,
	}
	if !edge.IsPhony() {
		// always sets edgeRule.
		// it will check siso_rule later.
		er := &edgeRule{
			edge: edge,
		}
		globals := g.globals
		for _, out := range edge.Outputs() {
			globals.edgeRules[out.ID()].set(er)
			if log.V(1) {
				outPath := globals.targetPath(out)
				clog.Infof(ctx, "add edgeRule for %s [newStepDef]", outPath)
			}
		}
	}
	return stepDef
}

// EnsureRule ensures siso rule for StepDef when it needs to run.
// It may not be called when skipped.
func (s *StepDef) EnsureRule(ctx context.Context) {
	if s.ruleReady {
		return
	}
	s.ruleReady = true
	if s.edge.IsPhony() {
		return
	}
	outputs := s.edge.Outputs()
	if len(outputs) == 0 {
		return
	}
	// *edgeRule is stored in newStepDef for all outputs of the edge.
	er := s.globals.edgeRules[outputs[0].ID()].get()
	if er == nil {
		// *edgeRule was removed by er.ensure by other output of the edge?
		outPath := s.globals.targetPath(outputs[0])
		clog.Warningf(ctx, "edgeRule not found for %s: pure=%t", outPath, s.pure)
		return
	}
	var gotRule bool
	gotRule, s.rule, s.pure = er.ensure(ctx, s.globals)
	if !gotRule {
		s.rule, s.pure = s.globals.stepConfig.Lookup(ctx, s.globals.path, s.edge)
		if log.V(1) {
			clog.Infof(ctx, "rule:%t", s.pure)
		}
	}
}

// String returns step id.
func (s *StepDef) String() string {
	if s == nil {
		return ""
	}
	return s.id
}

// Next returns next step def.
func (s *StepDef) Next() build.StepDef {
	if s == nil {
		return nil
	}
	if s.next == nil {
		return nil
	}
	return s.next
}

// RuleName returns rule name of the step.
func (s *StepDef) RuleName() string {
	return s.rule.Name
}

// ActionName returns action name of the step.
func (s *StepDef) ActionName() string {
	return s.edge.RuleName()
}

// Returns if we need to use the hermetic global shell because
// the command line is too complicated to exec directly.
func (s *StepDef) usesShell() bool {
	s.shellOnce.Do(func() {
		if s.edge == nil {
			return
		}
		if runtime.GOOS == "windows" {
			// hermetic shells are not supported on windows currently.
			s.needShell = false
		} else {
			cmdline := s.edge.Binding("command")
			_, err := shutil.Split(cmdline)
			s.needShell = err != nil
		}
	})
	return s.needShell
}

// Adds the inputs for the global shell to inputs and returns the modified slice.
// Seen is optional, if provided, it will be used to deduplicate paths. Otherwise,
// an n^2 deduplication will be used.
func (s *StepDef) addShellInputs(inputs []path.Path, seen map[path.Path]bool) []path.Path {
	if s.usesShell() {
		for _, in := range s.globals.shellInputs {
			if seen != nil {
				if !seen[in] {
					seen[in] = true
					inputs = append(inputs, in)
				}
			} else if !slices.Contains(inputs, in) {
				inputs = append(inputs, in)
			}
		}
	}
	return inputs
}

// Args returns command line arguments of the step.
func (s *StepDef) Args(ctx context.Context) []string {
	args := s.stepArgs()
	if len(args) > 3 && args[0] == "ninja" && args[1] == "-t" && args[2] == "msvc" {
		flagSet := flag.NewFlagSet("ninja-msvc", flag.ContinueOnError)
		tool := "msvc"
		flagSet.StringVar(&tool, "t", tool, "ninja tool name")
		flagSet.StringVar(&s.envfile, "e", s.envfile, "load environment block from ENVFILE as environment")
		// -o FILE and -p PREFIX is not used?
		err := flagSet.Parse(args[1:])
		if err != nil {
			clog.Warningf(ctx, "%s failed to parse ninja flags %q: %v", s, args, err)
			return args
		}
		if tool != "msvc" {
			return args
		}
		log.V(1).Infof("%s envfile=%q", s, s.envfile)
		return flagSet.Args()
	}
	return args
}

func (s *StepDef) stepArgs() []string {
	if s.edge == nil {
		return nil
	}
	cmdline := s.edge.Binding("command")
	if runtime.GOOS == "windows" {
		args, err := cmdutil.Split(cmdline) //nolint:staticcheck // SA4023: cmdutil.Split can return nil on windows.
		if err != nil {                     //nolint:staticcheck // SA4023: cmdutil.Split can return nil on windows.
			return []string{"cmd.exe", "/C", cmdline}
		}
		return args
	}
	args, err := shutil.Split(cmdline)
	if err != nil {
		shell := "/bin/sh"
		if s.globals.shellCmd != "" {
			shell = s.globals.shellCmd
		}
		return []string{shell, "-c", cmdline}
	}
	return args
}

// IsPhony returns whether the step if phony or not.
func (s *StepDef) IsPhony() bool {
	return s.edge.IsPhony()
}

// CmdHash returns command hash of the step.
func (s *StepDef) CmdHash() []byte {
	return s.edge.CmdHash()
}

// Binding returns a binding of the step.
//
// Ninja bindings are explained in https://ninja-build.org/manual.html#ref_rule:~:text=bindings
// StepDef may overwrites Ninja bindings. e.g. deps, restat.
// StepDef also has custom bindings. e.g. remote_wrapper, remote_command.
func (s *StepDef) Binding(name string) string {
	switch name {
	case "deps":
		if s.rule.Deps == "none" {
			return ""
		}
		if s.rule.Deps == "depfile" && s.edge.Binding("depfile") == "" {
			return ""
		}
		if s.rule.Deps != "" {
			return s.rule.Deps
		}
		return s.edge.Binding(name)
	case "remote_wrapper":
		return s.rule.RemoteWrapper
	case "remote_command":
		return s.rule.RemoteCommand
	case "canonicalize_dir":
		if s.rule.CanonicalizeDir == nil {
			return ""
		}
		if *s.rule.CanonicalizeDir {
			return "true"
		}
		return "false"
	case "envfile":
		return s.envfile
	case "gn_target":
		return s.globals.gnTargets[s.edge].String()

	case "strict_remote":
		if s.rule.StrictRemote {
			return "true"
		}
		return ""
	case "use_remote_exec_wrapper":
		if s.rule.UseRemoteExecWrapper {
			return "true"
		}
		return ""
	case "use_system_input":
		if s.rule.UseSystemInput {
			return "true"
		}
		return ""
	case "ignore_extra_input_pattern":
		return s.rule.IgnoreExtraInputPattern
	case "ignore_extra_output_pattern":
		return s.rule.IgnoreExtraOutputPattern
	case "restat":
		if s.rule.Restat {
			return "true"
		}
		return s.edge.Binding(name)
	case "restat_content":
		if s.rule.RestatContent != nil {
			if *s.rule.RestatContent {
				return "true"
			}
			return "false"
		}
		switch s.edge.Binding(name) {
		case "":
			return ""
		case "false", "0":
			return "false"
		default:
			return "true"
		}
	case "impure":
		if s.rule.Impure {
			return "true"
		}
		return ""
	case "sandbox_disabled":
		if s.rule.SandboxDisabled {
			return "true"
		}
		return s.edge.Binding(name)
	case "cache_disabled":
		if s.rule.CacheDisabled {
			return "true"
		}
		return s.edge.Binding(name)
	case "timeout":
		return s.rule.Timeout
	case "exec_timeout":
		return s.rule.ExecTimeout
	case "pool":
		pool := s.edge.Pool()
		return pool.Name()

	case "debug":
		if s.rule.Debug {
			return "true"
		}
		return ""
	}
	return s.edge.Binding(name)
}

// Depfile returns workspace relative depfile path or empty if not set.
func (s *StepDef) Depfile(ctx context.Context) path.Path {
	depfile := s.edge.UnescapedBinding("depfile")
	if depfile == "" {
		return ""
	}
	return path.New(s.globals.path.MaybeFromRelative(ctx, depfile))
}

// Rspfile returns workspace relative rspfile path or empty if not set.
func (s *StepDef) Rspfile(ctx context.Context) path.Path {
	rspfile := s.edge.UnescapedBinding("rspfile")
	if rspfile == "" {
		return ""
	}
	return path.New(s.globals.path.MaybeFromRelative(ctx, rspfile))
}

func edgeSolibs(edge *ninjautil.Edge) iter.Seq[string] {
	return func(yield func(string) bool) {
		solibsStr := edge.Binding("solibs")
		if solibsStr == "" {
			return
		}
		for in := range strings.SplitSeq(solibsStr, " ") {
			in = strings.TrimSpace(in)
			if in == "" {
				continue
			}
			if !yield(in) {
				return
			}
		}
	}
}

// Inputs returns inputs of the step.
func (s *StepDef) Inputs(ctx context.Context) []path.Path {
	ctx, span := trace.NewSpan(ctx, "stepdef-inputs")
	defer span.Close(nil)

	seen := make(map[path.Path]bool)
	var targets []path.Path
	globals := s.globals
	for _, in := range s.edge.Inputs() {
		p := globals.targetPath(in)
		if seen[p] {
			continue
		}
		seen[p] = true
		targets = append(targets, p)
	}
	for p := range edgeSolibs(s.edge) {
		if s.rule.Debug {
			clog.Infof(ctx, "solib %s", p)
		}
		pp := path.New(globals.path.MaybeFromRelative(ctx, p))
		if seen[pp] {
			continue
		}
		seen[pp] = true
		targets = append(targets, pp)
	}
	targets = fixInputs(ctx, s, targets, s.rule.ExcludeInputPatterns)
	targets = append(targets, s.ToolInputs(ctx)...)
	targets = s.addShellInputs(targets, seen)
	if s.rule.Debug {
		clog.Infof(ctx, "targets=%q", targets)
	}
	return uniqueFiles(targets)
}

// TriggerInputs returns inputs of the step that would trigger the step's action.
func (s *StepDef) TriggerInputs(ctx context.Context) []path.Path {
	globals := s.globals
	inputs := s.edge.TriggerInputs()
	// map dedup is not fast enough here.
	// dedup by sort+compact.
	type idxid struct {
		idx, id int
	}
	inIDs := make([]idxid, 0, len(inputs))
	for i, in := range inputs {
		inIDs = append(inIDs, idxid{idx: i, id: in.ID()})
	}
	slices.SortStableFunc(inIDs, func(a, b idxid) int {
		return cmp.Compare(a.id, b.id)
	})
	inIDs = slices.CompactFunc(inIDs, func(a, b idxid) bool {
		return a.id == b.id
	})
	// sort by idx to preserve original order.
	slices.SortFunc(inIDs, func(a, b idxid) int {
		return cmp.Compare(a.idx, b.idx)
	})
	targets := make([]path.Path, 0, len(inIDs)+len(globals.shellInputs))
	for _, i := range inIDs {
		in := inputs[i.idx]
		p := globals.targetPath(in)
		targets = append(targets, p)
	}
	targets = s.addShellInputs(targets, nil)
	return targets
}

// ExpandedTriggerInputs returns trigger inputs of the step with phony targets
// expanded recursively to their underlying trigger inputs.
func (s *StepDef) ExpandedTriggerInputs(ctx context.Context) []path.Path {
	globals := s.globals
	inputs := s.edge.TriggerInputs()
	type idxid struct {
		idx, id int
	}
	inIDs := make([]idxid, 0, len(inputs))
	for i, in := range inputs {
		inIDs = append(inIDs, idxid{idx: i, id: in.ID()})
	}
	slices.SortStableFunc(inIDs, func(a, b idxid) int {
		return cmp.Compare(a.id, b.id)
	})
	inIDs = slices.CompactFunc(inIDs, func(a, b idxid) bool {
		return a.id == b.id
	})
	slices.SortFunc(inIDs, func(a, b idxid) int {
		return cmp.Compare(a.idx, b.idx)
	})
	targets := make([]path.Path, 0, len(inIDs)+len(globals.shellInputs))
	var phonyEdges []*ninjautil.Edge
	for _, i := range inIDs {
		in := inputs[i.idx]
		if inEdge, ok := in.InEdge(); ok && inEdge.IsPhony() {
			phonyEdges = append(phonyEdges, inEdge)
			continue
		}
		p := globals.targetPath(in)
		targets = append(targets, p)
	}
	if len(phonyEdges) > 0 {
		seen := seenSetPool.Get().(map[path.Path]bool)
		defer func() {
			clear(seen)
			seenSetPool.Put(seen)
		}()
		for _, p := range targets {
			seen[p] = true
		}
		visited := make(map[*ninjautil.Edge]bool)
		for _, inEdge := range phonyEdges {
			p := globals.targetPath(inEdge.Outputs()[0])
			targets = replacePhony(ctx, globals, visited, seen, p, inEdge, s.rule.Debug, targets)
		}
	}
	targets = s.addShellInputs(targets, nil)
	return targets
}

// DepInputs returns inputs stored in depfile / depslog. The parse runs
// once per StepDef; concurrent callers share the result via depsOnce.
func (s *StepDef) DepInputs(ctx context.Context) (iter.Seq[path.Path], error) {
	ctx, span := trace.NewSpan(ctx, "stepdef-dep-inputs")
	defer span.Close(nil)
	s.depsOnce.Do(func() {
		s.deps, s.deperr = depInputs(ctx, s)
	})
	return s.deps, s.deperr
}

type depsPath struct {
	path      string
	canonpath path.Path
}

// depInputs returns deps inputs of the step.
func depInputs(ctx context.Context, s *StepDef) (iter.Seq[path.Path], error) {
	var deps []string
	var err error
	switch s.edge.Binding("deps") {
	case "gcc", "msvc":
		// deps info is stored in deps log.
		outputs := s.edge.Outputs()
		if len(outputs) == 0 {
			return nil, fmt.Errorf("%w: no outputs", build.ErrMissingDeps)
		}
		out := outputs[0].Path()
		var key DepsLogKey
		var depIDs []int
		depIDs, key, err = s.globals.depsLog.RetrieveIDs(ctx, out)
		if err != nil {
			return nil, fmt.Errorf("%w: failed to lookup deps log %s: %w", build.ErrMissingDeps, out, err)
		}
		state, err := s.globals.depsLog.CheckKey(ctx, s.globals.hashFS, s.globals.path, key)
		if log.V(1) {
			clog.Infof(ctx, "depslog %s: %s %d", out, state, len(deps))
		}
		switch state {
		case DepsLogStale:
			return nil, fmt.Errorf("%w: %v", build.ErrStaleDeps, err)
		case DepsLogValid, DepsLogValidDigest:
			// ok
		default:
			return nil, fmt.Errorf("wrong deps log state for %q: state=%s %v", out, state, err)
		}
		return func(yield func(path.Path) bool) {
			for _, depID := range depIDs {
				if depID < 0 || depID >= len(s.globals.depsPaths) {
					in, err := s.globals.depsLog.Path(depID)
					if err != nil {
						clog.Warningf(ctx, "unexpected dep id=%d for %q: %v", depID, out, err)
						continue
					}
					if !yield(s.globals.path.FromRelativePath(ctx, in)) {
						return
					}
					continue
				}
				dp := s.globals.depsPaths[depID].Load()
				if dp == nil {
					in, err := s.globals.depsLog.Path(depID)
					if err != nil {
						clog.Warningf(ctx, "unexpected depid=%d for %q: %v", depID, out, err)
						continue
					}
					dp = &depsPath{
						path:      in,
						canonpath: s.globals.path.FromRelativePath(ctx, in),
					}
					// dp should be the same for depID, so
					// no need to use compareAndSwap.
					s.globals.depsPaths[depID].Store(dp)
				}
				if !yield(dp.canonpath) {
					return
				}
			}
		}, nil

	case "":
		// deps info is in depfile
		depfile := s.edge.UnescapedBinding("depfile")
		if depfile == "" {
			return func(yield func(path.Path) bool) {}, nil
		}
		df := path.New(s.globals.path.MaybeFromRelative(ctx, depfile))
		if s.edge.Binding("generator") != "" {
			// e.g. rule gn.
			// generator runs locally, so believe a local file
			// rather than a file in hashfs.
			s.globals.hashFS.Forget(ctx, s.globals.path.WorkspaceRoot, []path.Path{df})
		}
		_, err := s.globals.hashFS.Stat(ctx, s.globals.path.WorkspaceRoot, df)
		if err != nil {
			return nil, fmt.Errorf("%w: no depfile %s: %w", build.ErrMissingDeps, depfile, err)
		}
		fsys := s.globals.hashFS.FileSystem(ctx, s.globals.path.WorkspaceRoot)
		deps, err = makeutil.ParseDepsFile(ctx, fsys, string(df))
		if err != nil {
			return nil, fmt.Errorf("%w: failed to load depfile %s: %w", build.ErrMissingDeps, df, err)
		}
		if log.V(1) {
			clog.Infof(ctx, "depfile %s: %d", depfile, len(deps))
		}
		return func(yield func(path.Path) bool) {
			for _, in := range deps {
				if !yield(s.globals.path.FromRelativePath(ctx, in)) {
					return
				}
			}
		}, nil
	}
	return func(yield func(path.Path) bool) {}, nil
}

// DepsBaseInputs returns inputs of the step.
// If includeOrderOnly is false, it will be combined with scandeps results.
// If includeOrderOnly is true, it will be trimmed down by scandeps results, or depfile.
func (s *StepDef) DepsBaseInputs(ctx context.Context, toolInputs []path.Path, includeOrderOnly bool) []path.Path {
	var inputs []path.Path
	// always use toolInputs.
	if s.rule.Debug {
		clog.Infof(ctx, "deps base tool inputs: %q order_only=%t", toolInputs, includeOrderOnly)
	}
	inputs = append(inputs, toolInputs...)
	// TODO: per rule?
	filter := s.globals.stepConfig.Scandeps.stepInputsFilter
	seen := make(map[path.Path]bool)
	var stepInputs []path.Path
	var nodes []*ninjautil.Node
	// need to expand phony targets here
	if includeOrderOnly {
		stepInputs = s.Inputs(ctx)
		nodes = s.edge.Inputs()
	} else {
		stepInputs = s.TriggerInputs(ctx)
		nodes = s.edge.TriggerInputs()
	}
	visited := make(map[*ninjautil.Edge]bool)
	for _, in := range nodes {
		p := s.globals.targetPath(in)
		if inEdge, ok := in.InEdge(); ok && inEdge.IsPhony() {
			stepInputs = replacePhony(ctx, s.globals, visited, seen, p, inEdge, s.rule.Debug, stepInputs)
			if s.rule.Debug {
				clog.Infof(ctx, "deps base input: expand phony %q", in)
			}
		} else {
			if s.rule.Debug {
				if !ok {
					clog.Infof(ctx, "deps base inputs: ignore no inEdge %q", in)
				} else {
					clog.Infof(ctx, "deps base inputs: not phony %q", in)
				}
			}
		}
	}
	seen = make(map[path.Path]bool)
	for _, in := range stepInputs {
		if seen[in] {
			continue
		}
		seen[in] = true
		if !filter(ctx, string(in), s.rule.Debug) {
			if s.rule.Debug {
				clog.Infof(ctx, "deps base inputs ignored: %s", in)
			}
			continue
		}
		if s.rule.Debug {
			clog.Infof(ctx, "deps base inputs preserve %s", in)
		}
		inputs = append(inputs, in)
	}
	return inputs
}

// ToolInputs returns tool inputs of the step.
func (s *StepDef) ToolInputs(ctx context.Context) []path.Path {
	ctx, span := trace.NewSpan(ctx, "stepdef-tool-inputs")
	defer span.Close(nil)

	inputs := make([]path.Path, 0, len(s.rule.Inputs)+len(s.globals.shellInputs))
	for _, in := range s.rule.Inputs {
		inputs = append(inputs, fromConfigPath(ctx, s.globals.path, in))
	}
	inputs = s.globals.stepConfig.ExpandInputs(ctx, s.globals.path, s.globals.hashFS, inputs)
	inputs = s.addShellInputs(inputs, nil)
	return inputs
}

func fixInputs(ctx context.Context, stepDef *StepDef, inputs []path.Path, excludes []string) []path.Path {
	if stepDef.rule.Debug {
		clog.Infof(ctx, "fix inputs=%d excludes=%d", len(inputs), len(excludes))
	}
	newInputs := make([]path.Path, 0, len(inputs))
	for _, in := range inputs {
		if stepDef.globals.phony[string(in)] {
			_, err := stepDef.globals.hashFS.Stat(ctx, stepDef.globals.path.WorkspaceRoot, in)
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					if log.V(1) {
						clog.Infof(ctx, "input %s is phony", in)
					}
					continue
				}
				clog.Warningf(ctx, "input %s is phony: %v", in, err)
				continue
			}
			clog.Infof(ctx, "input %s is phony, but exists", in)
		}
		newInputs = append(newInputs, in)
	}
	inputs = newInputs
	if len(excludes) == 0 {
		return inputs
	}
	rm := make(map[path.Path]bool)
	for _, e := range excludes {
		var m func(in string) bool
		if !strings.Contains(e, "/") {
			// Most exclude_input_patterns are "*.stamp" or so.
			// Just use strings.HasSuffix for such special case.
			if suffix, ok := strings.CutPrefix(e, "*"); ok {
				if !strings.ContainsAny(suffix, `*?[\`) {
					// special case just suffix match.
					m = func(in string) bool {
						return strings.HasSuffix(in, suffix)
					}
				}
			}
			if m == nil {
				// no "/", but has meta character of file name pattern, so use filepath.Match for basename.
				m = func(in string) bool {
					ok, _ := filepath.Match(e, filepath.Base(in))
					return ok
				}
			}
		} else {
			// pattern includes "/". full path match
			m = func(in string) bool {
				ok, _ := filepath.Match(e, in)
				return ok
			}
		}
		for _, in := range inputs {
			if m(string(in)) {
				rm[in] = true
				if stepDef.rule.Debug {
					clog.Infof(ctx, "fix exclude %s by %s", in, e)
				}
			}
		}
	}
	if len(rm) == 0 {
		return inputs
	}
	r := make([]path.Path, 0, len(inputs)-len(rm))
	for _, in := range inputs {
		if rm[in] {
			continue
		}
		r = append(r, in)
	}
	if stepDef.rule.Debug {
		clog.Infof(ctx, "fixed inputs=%d excludes=%d -> inputs=%d", len(inputs), len(excludes), len(r))
	}
	return r
}

// ExpandedCaseSensitives returns expanded filenames if platform is case-sensitive.
func (s *StepDef) ExpandedCaseSensitives(ctx context.Context, inputs []path.Path) []path.Path {
	if s.Platform()["OSFamily"] == "Windows" {
		// Nothing to do on case-insensitive platform.
		return inputs
	}
	ctx, span := trace.NewSpan(ctx, "stepdef-expand-case-sensitives")
	defer span.Close(nil)
	if len(s.globals.caseSensitives) == 0 {
		return inputs
	}
	oldLen := len(inputs)
	m := make(map[path.Path]bool)
	var expanded []path.Path
	for _, f := range inputs {
		if m[f] {
			continue
		}
		expanded = append(expanded, f)
		m[f] = true
		csf, ok := s.globals.caseSensitives[strings.ToLower(string(f))]
		if !ok {
			continue
		}
		for _, cf := range csf {
			if m[cf] {
				continue
			}
			expanded = append(expanded, cf)
			m[cf] = true
		}
	}
	newLen := len(expanded)
	if oldLen != newLen {
		clog.Infof(ctx, "expand case-sensitive %d -> %d", oldLen, newLen)
	}
	return expanded
}

// expandLabels expands labels in given inputs.
func (s *StepDef) expandLabels(ctx context.Context, inputs []path.Path) []path.Path {
	ctx, span := trace.NewSpan(ctx, "stepdef-expand-labels")
	defer span.Close(nil)

	if s.rule.Debug {
		clog.Infof(ctx, "expands labels")
	}
	var hasLabel bool
	for _, input := range inputs {
		if strings.Contains(string(input), ":") {
			hasLabel = true
			break
		}
	}
	if !hasLabel {
		return uniqueFiles(inputs)
	}
	p := s.globals.path
	seen := make(map[path.Path]bool)
	var expanded []path.Path
	for i := 0; i < len(inputs); i++ {
		pp := inputs[i]
		if seen[pp] {
			continue
		}
		seen[pp] = true
		if !strings.Contains(string(pp), ":") {
			expanded = append(expanded, pp)
			continue
		}
		cpath := toConfigPath(p, pp)
		deps, ok := s.globals.stepConfig.InputDeps[cpath]
		if !ok {
			// TODO(b/266759797): make it hard error?
			clog.Warningf(ctx, "unknown label %q", cpath)
			continue
		}
		if s.rule.Debug {
			clog.Infof(ctx, "expand %s", cpath)
		}
		for _, dep := range deps {
			inputs = append(inputs, fromConfigPath(ctx, p, dep))
		}
	}
	return expanded
}

// seenSetPool pools the map[path.Path]bool dedup scratches used by
// ExpandedInputs (~60k calls per build, ~1k entries each). Callers
// must clear the map before returning it to the pool.
var seenSetPool = sync.Pool{
	New: func() any { return make(map[path.Path]bool, 1024) },
}

// ExpandedInputs returns expanded inputs
//   - Include indirect inputs.
//   - Add solibs for input (to execute the executable).
//   - Add the inputs from accumulate steps.
//   - Replace the inputs from replace steps.
//   - Exclude by ExcludeInputPatterns.
//     etc
func (s *StepDef) ExpandedInputs(ctx context.Context) []path.Path {
	ctx, span := trace.NewSpan(ctx, "stepdef-expanded-inputs")
	defer span.Close(nil)

	if s.rule.Debug {
		clog.Infof(ctx, "expanded inputs")
	}
	// it takes too much memory in later build stages.
	// keep Inputs as is, and expand them when calculating digest ?
	seen := seenSetPool.Get().(map[path.Path]bool)
	defer func() {
		clear(seen)
		seenSetPool.Put(seen)
	}()
	var phonyEdges []*ninjautil.Edge
	inputs := make([]path.Path, 0, 2*len(s.edge.Inputs()))
	globals := s.globals
	for _, in := range s.edge.Inputs() {
		p := globals.targetPath(in)
		if seen[p] {
			continue
		}
		seen[p] = true
		if inEdge, ok := in.InEdge(); ok && inEdge.IsPhony() {
			// replace later after indirect_inputs
			if s.rule.Debug {
				clog.Infof(ctx, "input from ninja(phony deferred): %s", p)
			}
			phonyEdges = append(phonyEdges, inEdge)
			continue
		}
		if s.rule.Debug {
			clog.Infof(ctx, "input from ninja: %s", p)
		}
		inputs = append(inputs, p)
	}
	for sp := range edgeSolibs(s.edge) {
		p := path.New(globals.path.MaybeFromRelative(ctx, sp))
		if seen[p] {
			continue
		}
		if s.rule.Debug {
			clog.Infof(ctx, "input from ninja solibs: %s", p)
		}
		inputs = append(inputs, p)
	}
	for _, rp := range s.rule.Inputs {
		p := path.Path(rp)
		if seen[p] {
			continue
		}
		seen[p] = true
		if s.rule.Debug {
			clog.Infof(ctx, "input from rule: %s", p)
		}
		inputs = append(inputs, p)
	}
	if s.rule.IndirectInputs.enabled() {
		if s.rule.Debug {
			clog.Infof(ctx, "indirect inputs")
		}
		// need to use different seen, so that replaces/accumulates
		// works even if indirect inputs see/ignore the inputs.
		iseen := seenSetPool.Get().(map[path.Path]bool)
		defer func() {
			clear(iseen)
			seenSetPool.Put(iseen)
		}()
		maps.Copy(iseen, seen)
		filter := s.rule.IndirectInputs.Filter(ctx, "indirect_inputs")
		for _, in := range s.edge.Inputs() {
			edge, ok := in.InEdge()
			if !ok {
				continue
			}
			inputs = s.appendIndirectInputs(ctx, filter, edge, inputs, iseen)
		}
		// and need to expand inputs for toolchain input etc.
	}
	if len(phonyEdges) > 0 {
		visited := make(map[*ninjautil.Edge]bool)
		for _, inEdge := range phonyEdges {
			// replace phony inputs here before ExpandInput,
			// since ExpandInputs removes non-exist inputs.
			p := globals.targetPath(inEdge.Outputs()[0])
			inputs = replacePhony(ctx, globals, visited, seen, p, inEdge, s.rule.Debug, inputs)
		}
	}

	inputs = globals.stepConfig.ExpandInputs(ctx, globals.path, globals.hashFS, inputs)
	var newInputs []path.Path
	changed := false
	for i := 0; i < len(inputs); i++ {
		inpath := globals.path.MaybeToRelative(ctx, string(inputs[i]))
		innode, ok := globals.nstate.LookupNodeByPath(inpath)
		if !ok {
			newInputs = append(newInputs, inputs[i])
			continue
		}
		er := globals.edgeRules[innode.ID()].get()
		if er == nil {
			newInputs = append(newInputs, inputs[i])
			continue
		}
		// check siso rule if it was not checked when input's step was skipped
		er.ensure(ctx, globals)
		if s.rule.Debug {
			solibs := 0
			for range edgeSolibs(er.edge) {
				solibs += 1
			}
			clog.Infof(ctx, "check edgeRule for %s inputs=%d solibs=%d replace=%t accumulate=%t", inputs[i], len(er.edge.Inputs()), solibs, er.replace, er.accumulate)
		}
		var ins []path.Path
		for _, in := range er.edge.Inputs() {
			p := globals.targetPath(in)
			if seen[p] {
				continue
			}
			seen[p] = true
			ins = append(ins, p)
		}
		if er.replace {
			if s.rule.Debug {
				clog.Infof(ctx, "replace %q -> %q", inputs[i], ins)
			}
			// some step may want to expand inputs by deps recursively.
			// inputs=*.stamp
			// *.stamp's inputs includes some binary
			// some binary has solibs.
			inputs = append(inputs, ins...)
			changed = true
			continue
		}
		newInputs = append(newInputs, inputs[i])
		var solibsIns []path.Path
		for in := range edgeSolibs(er.edge) {
			p := path.New(globals.path.MaybeFromRelative(ctx, in))
			if seen[p] {
				continue
			}
			seen[p] = true
			solibsIns = append(solibsIns, p)
		}
		if len(solibsIns) > 0 {
			// solibsIns need to check recursively.
			inputs = append(inputs, solibsIns...)
			if s.rule.Debug {
				clog.Infof(ctx, "solibs %q -> %q", inputs[i], solibsIns)
			}
			changed = true
		}
		if !er.accumulate {
			continue
		}
		// when accumulate expands inputs/outputs.
		edgeOuts := er.edge.Outputs()
		if inputs[i] == globals.targetPath(edgeOuts[0]) {
			// associates additional outputs to main output.
			// so step depends on the main output of this step can access
			// additional outputs of this step (in local run).
			for _, out := range edgeOuts[1:] {
				o := globals.targetPath(out)
				if seen[o] {
					continue
				}
				seen[o] = true
				ins = append(ins, o)
			}
		}
		inputs = append(inputs, ins...)
		if s.rule.Debug {
			clog.Infof(ctx, "accumulate %q -> %q", inputs[i], ins)
		}
		if !changed && len(ins) > 0 {
			changed = true
		}
	}
	newInputsPaths := fixInputs(ctx, s, newInputs, s.rule.ExcludeInputPatterns)
	newInputsPaths = s.addShellInputs(newInputsPaths, nil)
	if changed {
		result := make([]path.Path, len(newInputsPaths))
		copy(result, newInputsPaths)
		return result
	}
	if s.rule.Debug {
		clog.Infof(ctx, "expanded inputs -> %d", len(newInputsPaths))
	}
	return newInputsPaths
}

func replacePhony(ctx context.Context, globals *globals, visited map[*ninjautil.Edge]bool, seen map[path.Path]bool, target path.Path, edge *ninjautil.Edge, debug bool, inputs []path.Path) []path.Path {
	if visited[edge] {
		return inputs
	}
	visited[edge] = true
	edgeInputs := edge.TriggerInputs()
	if len(edgeInputs) == 0 {
		// empty phony target.
		// https://ninja-build.org/manual.html#_the_literal_phony_literal_rule
		// phony can also be used to create dummy targets for files
		// which may not exist at build time.
		// If a phony build statement is written without any
		// dependencies, the target will be considered out of date
		// if it does not exist.
		// so, phony target may exist as file, or may not.
		_, err := globals.hashFS.Stat(ctx, globals.path.WorkspaceRoot, target)
		if err == nil {
			inputs = append(inputs, target)
		}
		return inputs
	}
	for _, in := range edgeInputs {
		p := globals.targetPath(in)
		if seen[p] {
			continue
		}
		seen[p] = true
		if debug {
			clog.Infof(ctx, "input from ninja(phony): %s -> %s", target, p)
		}
		if inEdge, ok := in.InEdge(); ok && inEdge.IsPhony() {
			inputs = replacePhony(ctx, globals, visited, seen, p, inEdge, debug, inputs)
			continue
		}
		inputs = append(inputs, p)
	}
	return inputs
}

// appendIndirectInputs appends indirect inputs edge into inputs that matches with filter function, and updates seen.
func (s *StepDef) appendIndirectInputs(ctx context.Context, filter func(context.Context, string, bool) bool, edge *ninjautil.Edge, inputs []path.Path, seen map[path.Path]bool) []path.Path {
	edgeName := edge.RuleName()
	globals := s.globals
	if !edge.IsPhony() {
		// allow to use outputs of the edge.
		for _, out := range edge.Outputs() {
			p := globals.targetPath(out)
			if seen[p] {
				continue
			}
			seen[p] = true
			if !filter(ctx, string(p), s.rule.Debug) {
				if s.rule.Debug {
					clog.Infof(ctx, "input from ninja indirect[output] ignored: %s: %s", edgeName, p)
				}
				continue
			}
			if s.rule.Debug {
				clog.Infof(ctx, "input from ninja indirect[output] %s: %s", edgeName, p)
			}
			inputs = append(inputs, p)
		}
	}
	var nextEdges []*ninjautil.Edge
	for _, in := range edge.Inputs() {
		p := globals.targetPath(in)
		if seen[p] {
			continue
		}
		seen[p] = true
		edge, ok := in.InEdge()
		if ok {
			nextEdges = append(nextEdges, edge)
		}
		if !filter(ctx, string(p), s.rule.Debug) {
			if s.rule.Debug {
				clog.Infof(ctx, "input from ninja indirect[input] ignored: %s: %s", edgeName, p)
			}
			continue
		}
		if s.rule.Debug {
			clog.Infof(ctx, "input from ninja indirect[input] %s: %s", edgeName, p)
		}
		inputs = append(inputs, p)
	}
	for _, edge := range nextEdges {
		inputs = s.appendIndirectInputs(ctx, filter, edge, inputs, seen)
	}
	return inputs
}

// RemoteInputs returns remote input mappings.
func (s *StepDef) RemoteInputs() map[path.Path]path.Path {
	if len(s.rule.RemoteInputs) == 0 {
		return nil
	}
	m := make(map[path.Path]path.Path, len(s.rule.RemoteInputs))
	for k, v := range s.rule.RemoteInputs {
		m[path.FromClean(k)] = path.FromClean(v)
	}
	return m
}

// CheckMissingDeps checks if depInputs can be found in direct/indirect inputs.
// It returns whether any missing dependency is a known issue, and an error describing
// any missing dependencies found.
func (s *StepDef) CheckMissingDeps(ctx context.Context, depInputs []path.Path) (known bool, err error) {
	deps := make(map[path.Path]bool)
	for _, dep := range depInputs {
		deps[dep] = true
	}
	visited := make(map[*ninjautil.Edge]bool)
	seen := make(map[path.Path]bool)
	// resolve deps from s.edge's inputs.
	// it doesn't check output of s.edge.
	edges := resolveEdgeDeps(s.globals, s.edge, false, deps, visited, seen)
	// avoid recursion for memory efficiency (e.g. avoid deep stack)
	for len(edges) > 0 {
		edge := edges[0]
		// reuse edege[0] space to minimize allocations.
		remain := edges[1:]
		copy(edges, remain)
		edges[len(edges)-1] = nil
		edges = edges[:len(edges)-1]
		// resolve deps for edge's inputs and outputs.
		edges = append(edges, resolveEdgeDeps(s.globals, edge, true, deps, visited, seen)...)
		if len(deps) == 0 {
			return false, nil
		}
	}
	if len(deps) == 0 {
		return false, nil
	}
	bpath := s.globals.path
	var remaining []string
	for in := range deps {
		remaining = append(remaining, bpath.MaybeToRelative(ctx, string(in)))
	}
	sort.Strings(remaining)
	var outputPath string
	if len(s.edge.Outputs()) > 0 {
		out := path.New(bpath.MaybeFromRelative(ctx, s.edge.Outputs()[0].Path()))
		outputPath = toConfigPath(bpath, out)
	}
	reason := "unknown"
	v, known := s.globals.stepConfig.MissingDeps[outputPath]
	if known {
		reason = v
	}
	desc := fmt.Sprintf("deps inputs have no dependencies from %q to %q - %s", outputPath, remaining, reason)
	return known, errors.New(desc)
}

func resolveEdgeDeps(globals *globals, edge *ninjautil.Edge, checkOutputs bool, deps map[path.Path]bool, visited map[*ninjautil.Edge]bool, seen map[path.Path]bool) []*ninjautil.Edge {
	if len(deps) == 0 {
		return nil
	}
	var edges []*ninjautil.Edge
	for _, in := range edge.Inputs() {
		p := globals.targetPath(in)
		if deps[p] {
			delete(deps, p)
			if len(deps) == 0 {
				return nil
			}
		}
		if seen[p] {
			continue
		}
		seen[p] = true
		inEdge, ok := in.InEdge()
		if !ok {
			continue
		}
		if _, ok := visited[inEdge]; ok {
			continue
		}
		visited[inEdge] = true
		edges = append(edges, inEdge)
	}
	if checkOutputs {
		for _, out := range edge.Outputs() {
			p := globals.targetPath(out)
			if deps[p] {
				delete(deps, p)
				if len(deps) == 0 {
					return nil
				}
				continue
			}
			if seen[p] {
				continue
			}
			seen[p] = true
		}
	}
	return edges
}

// Handle runs a handler for the cmd.
func (s *StepDef) Handle(ctx context.Context, cmd *execute.Cmd) error {
	handler := s.rule.Handler
	if handler == "" {
		return nil
	}
	err := s.globals.buildConfig.Handle(ctx, handler, s.globals.path, cmd, func() []string {
		// expand here if handler calls cmd.expand_inputs().
		// if handler is not set, expand later by depsExpandInput in build/builder.go
		inputs := s.ExpandedInputs(ctx)
		clog.Infof(ctx, "cmd.expandedInputs %d", len(inputs))
		return path.Strings(inputs)
	})
	if err != nil {
		return err
	}
	// handler may use labels in inputs, so expand here.
	// TODO(ukai): always need to expand labels here?
	cmd.Inputs = s.expandLabels(ctx, cmd.Inputs)
	return nil
}

// Outputs returns outputs of the step.
func (s *StepDef) Outputs(ctx context.Context) []path.Path {
	seen := make(map[path.Path]bool)
	var targets []path.Path
	globals := s.globals
	for _, out := range s.edge.Outputs() {
		p := globals.targetPath(out)
		if seen[p] {
			continue
		}
		seen[p] = true
		targets = append(targets, p)
	}
	targets = append(targets, path.Paths(s.rule.Outputs)...)
	return uniqueFiles(targets)
}

// LocalOutputs returns outputs of the step that should be written to local disk.
func (s *StepDef) LocalOutputs(ctx context.Context) []path.Path {
	if s.rule.OutputLocal {
		return s.Outputs(ctx)
	}
	return nil
}

// Pure checks if the step is pure or not.
func (s *StepDef) Pure() bool {
	return s.pure
}

// Platform returns platform properties for remote execution.
func (s *StepDef) Platform() map[string]string {
	return s.rule.Platform
}

// IsRemoteRule reports whether the step is declared remote-executable
// in the siso stepconfig.
func (s *StepDef) IsRemoteRule() bool {
	return s.rule.Remote
}

// Sandbox returns properties for action sandboxing.
func (s *StepDef) Sandbox() map[string]string {
	// disable sandbox for impure step, inputs/outputs are not fully specified.
	if s.rule.Impure {
		return nil
	}
	// disable sandbox for console step for stdout/stderr handling.
	// TODO: support sandbox for pool=console
	if s.edge.Pool().Name() == "console" {
		return nil
	}
	// disable sandbox if "sandbox_disabled" is set in the ninja file
	// or in the siso config rule.
	if s.Binding("sandbox_disabled") == "true" {
		return nil
	}
	return s.globals.stepConfig.Sandbox
}

// RecordDeps records deps of the step.
func (s *StepDef) RecordDeps(ctx context.Context, output string, mtime time.Time, dg digest.Digest, deps []string) (bool, error) {
	if s.edge.Binding("deps") == "" {
		// no need to record deps
		return false, nil
	}
	key := DepsLogKey{
		Target: output,
		Mtime:  mtime,
		Digest: dg,
	}
	return s.globals.depsLog.Record(ctx, key, deps)
}

// RuleFix shows suggested fix for the rule.
func (s *StepDef) RuleFix(ctx context.Context, inadds, outadds []string) []byte {
	rule := s.rule
	rule.ActionName = s.ActionName()
	var actionOut string
	if outs := s.Outputs(ctx); len(outs) > 0 {
		actionOut = toConfigPath(s.globals.path, outs[0])
	}
	rule.ActionOuts = nil
	rule.CommandPrefix = strings.Join(s.Args(ctx), " ")
	if rule.OutputsMap == nil {
		rule.OutputsMap = make(map[string]StepDeps)
	}
	rule.Inputs = nil
	deps := rule.OutputsMap[actionOut]
	for _, in := range inadds {
		deps.Inputs = append(deps.Inputs, toConfigPath(s.globals.path, path.FromClean(in)))
	}
	deps.Inputs = uniqueFileStrings(deps.Inputs)
	sort.Strings(deps.Inputs)
	for _, out := range outadds {
		deps.Outputs = append(deps.Outputs, toConfigPath(s.globals.path, path.FromClean(out)))
	}
	deps.Outputs = uniqueFileStrings(deps.Outputs)
	sort.Strings(deps.Outputs)
	rule.OutputsMap = map[string]StepDeps{
		actionOut: deps,
	}
	ruleBuf, err := json.MarshalIndent(rule, "", " ")
	if err != nil {
		ruleBuf = []byte(err.Error())
	}
	return ruleBuf
}

func uniqueFiles(files []path.Path) []path.Path {
	seen := make(map[path.Path]bool)
	ret := files[:0] // reuse the same backing store.
	for _, f := range files {
		if seen[f] {
			continue
		}
		seen[f] = true
		ret = append(ret, f)
	}
	return ret
}

func uniqueFileStrings(files []string) []string {
	seen := make(map[string]bool)
	ret := files[:0] // reuse the same backing store.
	for _, f := range files {
		if seen[f] {
			continue
		}
		seen[f] = true
		ret = append(ret, f)
	}
	return ret
}

// AuxiliaryLogOutputFiles returns output files that siso explicitly logs digest of.
func (s *StepDef) AuxiliaryLogOutputFiles(ctx context.Context) []path.Path {
	return path.Paths(s.rule.AuxiliaryLogOutputFiles)
}

// AuxiliaryLogOutputDirs returns output directories that siso explicitly logs digest of.
func (s *StepDef) AuxiliaryLogOutputDirs(ctx context.Context) []path.Path {
	return path.Paths(s.rule.AuxiliaryLogOutputDirs)
}
