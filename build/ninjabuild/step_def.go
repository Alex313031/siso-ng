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

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/execute"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/o11y/trace"
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

	ruleReady bool
	rule      StepRule
	pure      bool

	// from depfile/depslog
	deps   func(yield func(string) bool) // exec root relative
	deperr error

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

// Args returns command line arguments of the step.
func (s *StepDef) Args(ctx context.Context) []string {
	args := stepArgs(s.edge)
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

func stepArgs(edge *ninjautil.Edge) []string {
	cmdline := edge.Binding("command")
	if runtime.GOOS == "windows" {
		args, err := cmdutil.Split(cmdline)
		if err != nil {
			return []string{"cmd.exe", "/C", cmdline}
		}
		return args
	}
	args, err := shutil.Split(cmdline)
	if err != nil {
		return []string{"/bin/sh", "-c", cmdline}
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
	case "no_fast_deps":
		if s.rule.NoFastDeps {
			return "true"
		}
		return ""
	case "remote_wrapper":
		return s.rule.RemoteWrapper
	case "remote_command":
		return s.rule.RemoteCommand
	case "canonicalize_dir":
		if s.rule.CanonicalizeDir {
			return "true"
		}
		return ""
	case "envfile":
		return s.envfile
	case "gn_target":
		return s.globals.gnTargets[s.edge].String()

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
		if s.rule.RestatContent {
			return "true"
		}
		return ""
	case "impure":
		if s.rule.Impure {
			return "true"
		}
		return ""
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

// Depfile returns exec-root relative depfile path or empty if not set.
func (s *StepDef) Depfile(ctx context.Context) string {
	depfile := s.edge.UnescapedBinding("depfile")
	if depfile == "" {
		return ""
	}
	return s.globals.path.MaybeFromWD(ctx, depfile)
}

// Rspfile returns exec-root relative rspfile path or empty if not set.
func (s *StepDef) Rspfile(ctx context.Context) string {
	rspfile := s.edge.UnescapedBinding("rspfile")
	if rspfile == "" {
		return ""
	}
	return s.globals.path.MaybeFromWD(ctx, rspfile)
}

func edgeSolibs(edge *ninjautil.Edge) []string {
	solibsStr := edge.Binding("solibs")
	if solibsStr == "" {
		return nil
	}
	var solibs []string
	for in := range strings.SplitSeq(solibsStr, " ") {
		in = strings.TrimSpace(in)
		if in == "" {
			continue
		}
		solibs = append(solibs, in)
	}
	return solibs
}

// Inputs returns inputs of the step.
func (s *StepDef) Inputs(ctx context.Context) []string {
	ctx, span := trace.NewSpan(ctx, "stepdef-inputs")
	defer span.Close(nil)

	seen := make(map[string]bool)
	var targets []string
	globals := s.globals
	for _, in := range s.edge.Inputs() {
		p := globals.targetPath(in)
		if seen[p] {
			continue
		}
		seen[p] = true
		targets = append(targets, p)
	}
	for _, p := range edgeSolibs(s.edge) {
		if s.rule.Debug {
			clog.Infof(ctx, "solib %s", p)
		}
		p := globals.path.MaybeFromWD(ctx, p)
		if seen[p] {
			continue
		}
		seen[p] = true
		targets = append(targets, p)
	}
	targets = fixInputs(ctx, s, targets, s.rule.ExcludeInputPatterns)
	targets = append(targets, s.ToolInputs(ctx)...)
	if s.rule.Debug {
		clog.Infof(ctx, "targets=%q", targets)
	}
	return uniqueFiles(targets)
}

// TriggerInputs returns inputs of the step that would trigger the step's action.
func (s *StepDef) TriggerInputs(ctx context.Context) []string {
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
	targets := make([]string, 0, len(inIDs))
	for _, i := range inIDs {
		in := inputs[i.idx]
		p := globals.targetPath(in)
		targets = append(targets, p)
	}
	return targets
}

// DepInputs returns inputs stored in depfile / depslog.
func (s *StepDef) DepInputs(ctx context.Context) (func(yield func(string) bool), error) {
	ctx, span := trace.NewSpan(ctx, "stepdef-dep-inputs")
	defer span.Close(nil)
	if s.deps == nil && s.deperr == nil {
		s.deps, s.deperr = depInputs(ctx, s)
	}
	return s.deps, s.deperr
}

// DepsLogState is a state of deps log entry.
type DepsLogState int

const (
	DepsLogUnknown DepsLogState = iota
	DepsLogStale
	DepsLogValid
)

func (s DepsLogState) String() string {
	switch s {
	case DepsLogUnknown:
		return "UNKNOWN"
	case DepsLogStale:
		return "STALE"
	case DepsLogValid:
		return "VALID"
	}
	return fmt.Sprintf("DepsLogState[%d]", int(s))
}

// CheckDepsLogState checks deps log state by its output file.
func CheckDepsLogState(ctx context.Context, hashFS *hashfs.HashFS, bpath *build.Path, target string, depsTime time.Time) (DepsLogState, string) {
	fi, err := hashFS.Stat(ctx, bpath.ExecRoot, bpath.MaybeFromWD(ctx, target))
	if err != nil {
		return DepsLogStale, fmt.Sprintf("not found deps output %q: %v", target, err)
	}
	if fi.ModTime().After(depsTime) {
		return DepsLogStale, fmt.Sprintf("output mtime %q: fs=%v depslog=%v", target, fi.ModTime(), depsTime)
	}
	return DepsLogValid, ""
}

type depsPath struct {
	path      string
	canonpath string
}

// depInputs returns deps inputs of the step.
func depInputs(ctx context.Context, s *StepDef) (func(yield func(string) bool), error) {
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
		var depsTime time.Time
		var depIDs []int
		depIDs, depsTime, err = s.globals.depsLog.RetrieveIDs(ctx, out)
		if err != nil {
			return nil, fmt.Errorf("%w: failed to lookup deps log %s: %w", build.ErrMissingDeps, out, err)
		}
		state, msg := CheckDepsLogState(ctx, s.globals.hashFS, s.globals.path, out, depsTime)
		switch state {
		case DepsLogStale:
			return nil, fmt.Errorf("%w: %s", build.ErrStaleDeps, msg)
		case DepsLogValid:
			// ok
		default:
			return nil, fmt.Errorf("wrong deps log state for %q: state=%s %s", out, state, msg)
		}
		if log.V(1) {
			clog.Infof(ctx, "depslog %s: %d", out, len(deps))
		}
		return func(yield func(string) bool) {
			for _, depID := range depIDs {
				if depID < 0 || depID >= len(s.globals.depsPaths) {
					in, err := s.globals.depsLog.Path(depID)
					if err != nil {
						clog.Warningf(ctx, "unexpected dep id=%d for %q: %v", depID, out, err)
						continue
					}
					in = s.globals.path.MaybeFromWD(ctx, in)
					if !yield(in) {
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
						canonpath: s.globals.path.MaybeFromWD(ctx, in),
					}
					// dp should be the same for depID, so
					// no need to use compareAndSwap.
					s.globals.depsPaths[depID].Store(dp)
				}
				in := dp.canonpath
				if !yield(in) {
					return
				}
			}
		}, nil

	case "":
		// deps info is in depfile
		depfile := s.edge.UnescapedBinding("depfile")
		if depfile == "" {
			return func(yield func(string) bool) {}, nil
		}
		df := s.globals.path.MaybeFromWD(ctx, depfile)
		if s.edge.Binding("generator") != "" {
			// e.g. rule gn.
			// generator runs locally, so believe a local file
			// rather than a file in hashfs.
			s.globals.hashFS.Forget(ctx, s.globals.path.ExecRoot, []string{df})
		}
		_, err := s.globals.hashFS.Stat(ctx, s.globals.path.ExecRoot, df)
		if err != nil {
			return nil, fmt.Errorf("%w: no depfile %s: %w", build.ErrMissingDeps, depfile, err)
		}
		fsys := s.globals.hashFS.FileSystem(ctx, s.globals.path.ExecRoot)
		deps, err = makeutil.ParseDepsFile(ctx, fsys, df)
		if err != nil {
			return nil, fmt.Errorf("%w: failed to load depfile %s: %w", build.ErrMissingDeps, df, err)
		}
		if log.V(1) {
			clog.Infof(ctx, "depfile %s: %d", depfile, len(deps))
		}
		return func(yield func(string) bool) {
			for _, in := range deps {
				in = s.globals.path.MaybeFromWD(ctx, in)
				if !yield(in) {
					return
				}
			}
		}, nil
	}
	return func(yield func(string) bool) {}, nil
}

// ToolInputs returns tool inputs of the step.
func (s *StepDef) ToolInputs(ctx context.Context) []string {
	ctx, span := trace.NewSpan(ctx, "stepdef-tool-inputs")
	defer span.Close(nil)

	var inputs []string
	for _, in := range s.rule.Inputs {
		inputs = append(inputs, fromConfigPath(ctx, s.globals.path, in))
	}
	return s.globals.stepConfig.ExpandInputs(ctx, s.globals.path, s.globals.hashFS, inputs)
}

func fixInputs(ctx context.Context, stepDef *StepDef, inputs, excludes []string) []string {
	if stepDef.rule.Debug {
		clog.Infof(ctx, "fix inputs=%d excludes=%d", len(inputs), len(excludes))
	}
	newInputs := make([]string, 0, len(inputs))
	for _, in := range inputs {
		if stepDef.globals.phony[in] {
			_, err := stepDef.globals.hashFS.Stat(ctx, stepDef.globals.path.ExecRoot, in)
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
	rm := make(map[string]bool)
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
			if m(in) {
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
	r := make([]string, 0, len(inputs)-len(rm))
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
func (s StepDef) ExpandedCaseSensitives(ctx context.Context, inputs []string) []string {
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
	m := make(map[string]bool)
	var expanded []string
	for _, f := range inputs {
		f = filepath.ToSlash(f)
		if m[f] {
			continue
		}
		expanded = append(expanded, f)
		m[f] = true
		csf, ok := s.globals.caseSensitives[strings.ToLower(f)]
		if !ok {
			continue
		}
		for _, f := range csf {
			if m[f] {
				continue
			}
			expanded = append(expanded, f)
			m[f] = true
		}
	}
	newLen := len(expanded)
	if oldLen != newLen {
		clog.Infof(ctx, "expand case-sensitive %d -> %d", oldLen, newLen)
	}
	return expanded
}

// expandLabels expands labels in given inputs.
func (s *StepDef) expandLabels(ctx context.Context, inputs []string) []string {
	ctx, span := trace.NewSpan(ctx, "stepdef-expand-labels")
	defer span.Close(nil)

	if s.rule.Debug {
		clog.Infof(ctx, "expands labels")
	}
	var hasLabel bool
	for _, input := range inputs {
		if strings.Contains(input, ":") {
			hasLabel = true
			break
		}
	}
	if !hasLabel {
		return uniqueFiles(inputs)
	}
	p := s.globals.path
	seen := make(map[string]bool)
	var expanded []string
	for i := 0; i < len(inputs); i++ {
		path := inputs[i]
		if seen[path] {
			continue
		}
		seen[path] = true
		if !strings.Contains(path, ":") {
			expanded = append(expanded, path)
			continue
		}
		cpath := toConfigPath(p, path)
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
			dep := fromConfigPath(ctx, p, dep)
			inputs = append(inputs, dep)
		}
	}
	return expanded
}

// ExpandedInputs returns expanded inputs
//   - Include indirect inputs.
//   - Add solibs for input (to execute the executable).
//   - Add the inputs from accumulate steps.
//   - Replace the inputs from replace steps.
//   - Exclude by ExcludeInputPatterns.
//     etc
func (s *StepDef) ExpandedInputs(ctx context.Context) []string {
	ctx, span := trace.NewSpan(ctx, "stepdef-expanded-inputs")
	defer span.Close(nil)

	if s.rule.Debug {
		clog.Infof(ctx, "expanded inputs")
	}
	// it takes too much memory in later build stages.
	// keep Inputs as is, and expand them when calculating digest ?
	seen := make(map[string]bool)
	var phonyEdges []*ninjautil.Edge
	var inputs []string
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
	for _, p := range edgeSolibs(s.edge) {
		p = globals.path.MaybeFromWD(ctx, p)
		if seen[p] {
			continue
		}
		if s.rule.Debug {
			clog.Infof(ctx, "input from ninja solibs: %s", p)
		}
		inputs = append(inputs, p)
	}
	for _, p := range s.rule.Inputs {
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
		iseen := make(map[string]bool)
		maps.Copy(iseen, seen)
		filter := s.rule.IndirectInputs.filter(ctx)
		for _, in := range s.edge.Inputs() {
			edge, ok := in.InEdge()
			if !ok {
				continue
			}
			inputs = s.appendIndirectInputs(ctx, filter, edge, inputs, iseen)
		}
		// and need to expand inputs for toolchain input etc.
	}
	for _, inEdge := range phonyEdges {
		// replace phony inputs here before ExpandInput,
		// since ExpandInputs removes non-exist inputs.
		p := globals.targetPath(inEdge.Outputs()[0])
		inputs = append(inputs, replacePhony(ctx, globals, seen, p, inEdge, s.rule.Debug)...)
	}

	inputs = globals.stepConfig.ExpandInputs(ctx, globals.path, globals.hashFS, inputs)
	var newInputs []string
	changed := false
	for i := 0; i < len(inputs); i++ {
		inpath := globals.path.MaybeToWD(ctx, inputs[i])
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
			clog.Infof(ctx, "check edgeRule for %s inputs=%d solibs=%d replace=%t accumulate=%t", inputs[i], len(er.edge.Inputs()), len(edgeSolibs(er.edge)), er.replace, er.accumulate)
		}
		var ins []string
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
		var solibsIns []string
		for _, in := range edgeSolibs(er.edge) {
			in = globals.path.MaybeFromWD(ctx, in)
			if seen[in] {
				continue
			}
			seen[in] = true
			solibsIns = append(solibsIns, in)
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
	newInputs = fixInputs(ctx, s, newInputs, s.rule.ExcludeInputPatterns)
	if changed {
		inputs = make([]string, len(newInputs))
		copy(inputs, newInputs)
	}
	if s.rule.Debug {
		clog.Infof(ctx, "expanded inputs -> %d", len(inputs))
	}
	return inputs
}

func replacePhony(ctx context.Context, globals *globals, seen map[string]bool, target string, edge *ninjautil.Edge, debug bool) []string {
	var inputs []string
	for _, in := range edge.Inputs() {
		p := globals.targetPath(in)
		if seen[p] {
			continue
		}
		seen[p] = true
		if debug {
			clog.Infof(ctx, "input from ninja(phony): %s -> %s", target, p)
		}
		if inEdge, ok := in.InEdge(); ok && inEdge.IsPhony() {
			inputs = append(inputs, replacePhony(ctx, globals, seen, p, inEdge, debug)...)
			continue
		}
		inputs = append(inputs, p)
	}
	return inputs
}

// appendIndirectInputs appends indirect inputs edge into inputs that matches with filter function, and updates seen.
func (s *StepDef) appendIndirectInputs(ctx context.Context, filter func(context.Context, string, bool) bool, edge *ninjautil.Edge, inputs []string, seen map[string]bool) []string {
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
			if !filter(ctx, filepath.ToSlash(p), s.rule.Debug) {
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
		if !filter(ctx, filepath.ToSlash(p), s.rule.Debug) {
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
func (s *StepDef) RemoteInputs() map[string]string {
	return s.rule.RemoteInputs
}

// CheckInputDeps checks dep can be found in its direct/indirect inputs.
// Returns true if it is unknown bad deps, false otherwise.
func (s *StepDef) CheckInputDeps(ctx context.Context, depInputs []string) (bool, error) {
	deps := make(map[string]bool)
	for _, dep := range depInputs {
		deps[dep] = true
	}
	seen := make(map[string]bool)
	// check input deps from s.edge's inputs.
	// it doesn't check output of s.edge.
	edges := checkInputDep(s.globals, s.edge, false, deps, seen)
	// avoid recursion for memory efficiency (e.g. avoid deep stack)
	for len(edges) > 0 {
		edge := edges[0]
		// reuse edege[0] space to minimize allocations.
		remain := edges[1:]
		copy(edges, remain)
		edges[len(edges)-1] = nil
		edges = edges[:len(edges)-1]
		// check input deps for edge's inputs and outputs.
		edges = append(edges, checkInputDep(s.globals, edge, true, deps, seen)...)
		if len(deps) == 0 {
			return false, nil
		}
	}
	if len(deps) == 0 {
		return false, nil
	}
	depInputs = depInputs[:0]
	bpath := s.globals.path
	for in := range deps {
		depInputs = append(depInputs, bpath.MaybeToWD(ctx, in))
	}
	sort.Strings(depInputs)
	var outputPath string
	if len(s.edge.Outputs()) > 0 {
		out := bpath.MaybeFromWD(ctx, s.edge.Outputs()[0].Path())
		outputPath = toConfigPath(bpath, out)
	}
	v, ok := s.globals.stepConfig.BadDeps[outputPath]
	if ok {
		return false, fmt.Errorf("deps inputs have no dependencies from %q to %q - %s", outputPath, depInputs, v)
	}
	return true, fmt.Errorf("deps inputs have no dependencies from %q to %q - unknown", outputPath, depInputs)
}

func checkInputDep(globals *globals, edge *ninjautil.Edge, checkOutputs bool, deps, seen map[string]bool) []*ninjautil.Edge {
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
		return inputs
	})
	if err != nil {
		return err
	}
	// handler may use labels in inputs, so expand here.
	// TODO(ukai): always need to expand labels here?
	cmd.Inputs = s.expandLabels(ctx, cmd.Inputs)

	// Add executables to REProxyConfig.ToolchainInputs to send Linux executables from Windows.
	if runtime.GOOS == "windows" && cmd.REProxyConfig != nil {
		var ok bool
		for _, in := range cmd.Inputs {
			_, ok = s.globals.executables[in]
			if ok {
				cmd.REProxyConfig.ToolchainInputs = append(cmd.REProxyConfig.ToolchainInputs, in)
			}
		}
	}
	return nil
}

// Outputs returns outputs of the step.
func (s *StepDef) Outputs(ctx context.Context) []string {
	seen := make(map[string]bool)
	var targets []string
	globals := s.globals
	for _, out := range s.edge.Outputs() {
		p := globals.targetPath(out)
		if seen[p] {
			continue
		}
		seen[p] = true
		targets = append(targets, p)
	}
	targets = append(targets, s.rule.Outputs...)
	return uniqueFiles(targets)
}

// LocalOutputs returns outputs of the step that should be written to local disk.
func (s *StepDef) LocalOutputs(ctx context.Context) []string {
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

// RecordDeps records deps of the step.
func (s *StepDef) RecordDeps(ctx context.Context, output string, t time.Time, deps []string) (bool, error) {
	if s.edge.Binding("deps") == "" {
		// no need to record deps
		return false, nil
	}
	return s.globals.depsLog.Record(ctx, output, t, deps)
}

// RuleFix shows suggested fix for the rule.
func (s *StepDef) RuleFix(ctx context.Context, inadds, outadds []string) []byte {
	rule := s.rule
	rule.ActionName = s.ActionName()
	var actionOut string
	if len(s.Outputs(ctx)) > 0 {
		actionOut = toConfigPath(s.globals.path, s.Outputs(ctx)[0])
	}
	rule.ActionOuts = nil
	rule.CommandPrefix = strings.Join(s.Args(ctx), " ")
	if rule.OutputsMap == nil {
		rule.OutputsMap = make(map[string]StepDeps)
	}
	rule.Inputs = nil
	deps := rule.OutputsMap[actionOut]
	for _, in := range inadds {
		in = toConfigPath(s.globals.path, in)
		deps.Inputs = append(deps.Inputs, in)
	}
	deps.Inputs = uniqueFiles(deps.Inputs)
	sort.Strings(deps.Inputs)
	for _, out := range outadds {
		out = toConfigPath(s.globals.path, out)
		deps.Outputs = append(deps.Outputs, out)
	}
	deps.Outputs = uniqueFiles(deps.Outputs)
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

func uniqueFiles(files []string) []string {
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

// REProxyConfig returns configuration options for using reproxy.
func (s *StepDef) REProxyConfig() *execute.REProxyConfig {
	return s.rule.REProxyConfig
}
