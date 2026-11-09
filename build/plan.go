// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/golang/glog"

	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/o11y/trace"
	"go.chromium.org/build/siso/path"
	"go.chromium.org/build/siso/ui"
)

var (
	// ErrNoTarget is an error when target is not found in the Graph.
	ErrNoTarget = errors.New("target not found")

	// ErrTargetIsSource is an error when target is the source in the Graph.
	ErrTargetIsSource = errors.New("target is source")

	// ErrDuplicateStep is an error when step for the target is already created.
	ErrDuplicateStep = errors.New("duplicate step")

	// ErrMissingDeps is an error when step failed to get deps.
	ErrMissingDeps = errors.New("missing deps in depfile/depslog")

	// ErrStaleDeps is an error when deps log is stale.
	ErrStaleDeps = errors.New("stale deps in depslog")
)

// Target is a build target used in Graph.
// 0 means no target.
// valid range is [0, NumTargets).
type Target int

// Graph provides a build graph, i.e. step definitions.
type Graph interface {
	// NumTargets returns the number of valid target IDs.
	NumTargets() int

	// Targets returns target paths for given args.
	// Targets may return known targets even if err != nil.
	Targets(context.Context, ...string) ([]Target, error)

	// LookupTarget returns the target ID for a build-dir-relative target path.
	LookupTarget(string) (Target, bool)

	// SpellcheckTarget returns the most similar target from given target.
	SpellcheckTarget(string) (string, error)

	// TargetPath returns workspace relative path of target.
	TargetPath(context.Context, Target) (string, error)

	// Edge creates new Edge for the target.
	// if err is ErrTargetIsSource, target is source and no step to
	// generate the target.
	// if err is ErrDuplicateStep, a step that generates the target
	// is already processed.
	Edge(context.Context, Target, StepDef) (*Edge, error)

	// InputDeps returns input dependencies.
	// input dependencies is a map from input path or label to
	// other files or labels needed for the key.
	// path is workspace relative and label contains ':'.
	// it's "input_deps" in Starlark config.
	InputDeps(context.Context) map[string][]string

	// InputsRequiringClangScandeps returns inputs that requires
	// clang scandeps.
	InputsRequiringClangScandeps(context.Context) []string

	// ClangScandeps returns clang scandeps mode.
	ClangScandeps(context.Context) string

	// EnvConfig returns environment configuration for build steps.
	EnvConfig(context.Context) *EnvConfig

	// StepLimits returns a map of maximum number of concurrent
	// steps by pool name.
	StepLimits(context.Context) map[string]int

	// Binding returns top-level binding
	Binding(name string) string

	// Filenames returns filenames of build manifest (all files loaded by build.ninja).
	Filenames() []string
}

// TargetError is an error of unknown target for build.
type TargetError struct {
	err      error
	Suggests []string
}

func (e TargetError) Unwrap() error {
	return e.err
}

func (e TargetError) Error() string {
	return e.err.Error()
}

// MissingSourceError is an error of missing source needed for build.
type MissingSourceError struct {
	Target   string
	NeededBy string
	Err      error
	Deps     []string
}

func (e MissingSourceError) Error() string {
	deps := slices.Clone(e.Deps)
	slices.Reverse(deps)
	errmsg := "missing and no known rule to make it"
	if !errors.Is(e.Err, fs.ErrNotExist) {
		errmsg = fmt.Sprintf("no known rule to make it: %v", e.Err)
	}
	if e.NeededBy != "" {
		return fmt.Sprintf("deps chain:\n  %s\n %q, needed by %q, %s", strings.Join(deps, " ->\n  "), e.Target, e.NeededBy, errmsg)
	}
	return fmt.Sprintf("deps chain:\n  %s\n %q %s", strings.Join(deps, " ->\n  "), e.Target, errmsg)
}

type scanState int

const (
	scanStateNotVisited scanState = iota
	scanStateVisiting
	scanStateIgnored
	scanStateDone
)

func (s scanState) String() string {
	switch s {
	case scanStateNotVisited:
		return "not-visited"
	case scanStateVisiting:
		return "visiting"
	case scanStateIgnored:
		return "ignored"
	case scanStateDone:
		return "done"
	default:
		return fmt.Sprintf("unknown=%d", int(s))
	}
}

// LocallyNeededSet records the remote outputs a local step will read, so
// -output_local_strategy=graph materializes just those. The scheduler fills it
// at plan finalization (add); the hashfs OutputLocal predicate reads it (the
// OutputLocal method, backed by Has).
//
// Keys are hashfs.MakeFullpath form: both add and Has go through MakeFullpath,
// so they agree even when a target path is itself absolute.
//
// Until Freeze, Has returns false. The state load predicate is consulted
// before the scheduler can fill the set; returning false keeps the CAS-only
// outputs a prior graph build left off disk valid across invocations, instead
// of forgetting them and re-running their producers. A genuinely-needed
// missing output is still re-materialized at build time by the mtimecheck
// skip-flush (or re-run if its CAS blob is gone). Freeze publishes the map.
type LocallyNeededSet struct {
	m     map[string]bool
	ready atomic.Bool
}

// NewLocallyNeededSet returns an empty, not-yet-ready set.
func NewLocallyNeededSet() *LocallyNeededSet {
	return &LocallyNeededSet{m: make(map[string]bool)}
}

// OutputLocal is the hashfs OutputLocal predicate face. Bind it
// (set.OutputLocal) so the predicate and the scheduler share this exact set.
func (s *LocallyNeededSet) OutputLocal(_ context.Context, path string) bool {
	return s.Has(path)
}

// Has reports whether path (hashfs.MakeFullpath form) is locally-needed. It
// returns false until the set is frozen (see the type doc); a nil set (no
// graph strategy) returns true.
func (s *LocallyNeededSet) Has(path string) bool {
	if s == nil {
		return true
	}
	if !s.ready.Load() {
		return false
	}
	return s.m[path]
}

// add records a locally-needed path (hashfs.MakeFullpath form). Single-
// threaded classifier use only, before Freeze.
func (s *LocallyNeededSet) add(path string) {
	s.m[path] = true
}

// Freeze publishes the map; after it, Has reads the map instead of the
// not-yet-frozen default (false).
func (s *LocallyNeededSet) Freeze() {
	s.ready.Store(true)
}

type targetInfo struct {
	// scanState of the target.
	scan scanState
	// list of steps that wait the target becomes ready.
	waits []*Step
	// true if the target is source (no step generates the target).
	source bool
	// true if the target is generated output.
	output bool
	// true if explicitly requested; graph materializes a requested output
	// even when an edge sibling has consumers.
	requested bool
	// true if the target is phony_output.
	phonyOutput bool
	// pointer to Step.
	step *Step
	// pointer to Edge.
	edge *Edge
	// weight of the target.
	weight int
	// accumulated weight of the target.
	criticalPathWeight int
}

// plan maintains which step to execute next.
type plan struct {
	mu     sync.Mutex
	closed bool
	// q is a channel where selected ready setps are pushed to.
	q chan *Step
	// ready is a queue of ready steps prioritized with weights.
	ready priorityQueue
	// indexed by Target
	targets []targetInfo
	// targets sorted topologically.
	// Used to calculate weights efficiently.
	targetsSorted []Target
	npendings     int
}

// priorityQueue is a pool of ready steps.
// It implements https://pkg.go.dev/container/heap#Interface to prioritize
// steps with larger weights.
// See also https://pkg.go.dev/container/heap#example-package-PriorityQueue
type priorityQueue []*Step

// Len is the number of ready steps in the queue.
func (pq priorityQueue) Len() int {
	return len(pq)
}

// Less reports whether the element i has higher priority than the element j.
func (pq priorityQueue) Less(i, j int) bool {
	// We want to prioritize a step with a larger weight.
	return pq[i].weight > pq[j].weight
}

// Swap swaps the steps with indexes i and j.
func (pq priorityQueue) Swap(i, j int) {
	pq[i], pq[j] = pq[j], pq[i]
}

// Push pushes a ready step to the end.
func (pq *priorityQueue) Push(x any) {
	s := x.(*Step)
	*pq = append(*pq, s)
}

// Pop removes and returns the last step from the priority queue.
func (pq *priorityQueue) Pop() any {
	old := *pq
	n := len(old)
	s := old[n-1]
	old[n-1] = nil
	*pq = old[0 : n-1]
	return s
}

// schedulerOption is scheduler option.
type schedulerOption struct {
	NumTargets  int
	Path        *Path
	HashFS      *hashfs.HashFS
	Prepare     bool
	EnableTrace bool

	// StepWeight, if non-nil, returns the intrinsic weight for a non-phony
	// step; values < 1 are clamped to 1. When nil, non-phony steps have
	// weight 1.
	StepWeight func(*Step) int

	// LocallyNeeded, if non-nil, is filled at plan finalization with the
	// outputs a downstream local step consumes. Must be unfrozen when passed.
	LocallyNeeded *LocallyNeededSet
}

// scheduler creates a plan.
type scheduler struct {
	path   *Path
	hashFS *hashfs.HashFS

	actionOutputDirs ensureActionOutputDirs

	plan *plan

	locallyNeeded *LocallyNeededSet

	stepWeight func(*Step) int

	// number of steps scheduled.
	total int

	lastProgress time.Time
	visited      int

	// prepare runs steps to generate inputs for the requested targets,
	// but not run steps to generate requested targets.
	prepare bool

	// prepareHeaderOnly runs steps to generate headers needed for
	// the requested targets.
	prepareHeaderOnly bool

	enableTrace bool
}

func targetPath(ctx context.Context, g Graph, t Target) path.Path {
	p, err := g.TargetPath(ctx, t)
	if err != nil {
		return path.FromClean(fmt.Sprint(err))
	}
	return path.FromClean(p)
}

// schedule schedules build plans for args from graph into sched.
func schedule(ctx context.Context, sched *scheduler, graph Graph, args ...string) (retErr error) {
	defer trace.Begin(ctx, "build.schedule").End()
	targets, err := graph.Targets(ctx, args...)
	started := time.Now()
	clog.Infof(ctx, "schedule targets: %v [%d]: %v", targets, graph.NumTargets(), err)
	if err != nil {
		if !experiments.Enabled("ignore-missing-targets", "") {
			if len(args) <= 7 {
				return TargetError{err: err, Suggests: suggestTargets(ctx, sched, graph, args...)}
			}
			return TargetError{err: fmt.Errorf("%w\ntarget error in too many targets. use `SISO_EXPERIMENTS=ignore-missing-targets` to ignore missing targets", err)}
		}
		ui.Default.PrintLines(ui.SGR(ui.Yellow, fmt.Sprintf("WARNING: ignore missing targets: %v\n\n", err)))
	}
	if len(targets) == 0 {
		if len(args) > 0 {
			return TargetError{err: errors.New("no targets")}
		}
		// No explicit targets requested and the build file has no
		// default/root targets. Return nil so the caller prints
		// "ninja: no work to do." and exits 0, matching Ninja behavior.
		return nil
	}
	if len(args) > 0 {
		var targetNames []string
		for _, t := range targets {
			targetNames = append(targetNames, sched.path.MaybeToRelative(ctx, string(targetPath(ctx, graph, t))))
		}
		if !slices.Equal(args, targetNames) {
			ui.Default.PrintLines(fmt.Sprintf("target: %q\n    ->  %q\n\n", args, targetNames))
		}
	}
	// Mark requested outputs so the graph classifier materializes them per
	// output (a requested target need not be a leaf). Safe here: nothing else
	// touches plan.targets yet.
	for _, t := range targets {
		sched.plan.targets[t].requested = true
	}

	if !experiments.Enabled("disable-ensure-output-dirs", "") {
		sched.actionOutputDirs.init()
		go sched.actionOutputDirs.run(ctx, sched.path)
	}

	// validationQueue collects validation targets found during scheduling.
	var validationQueue []Target
	for _, t := range targets {
		switch sched.plan.targets[t].scan {
		case scanStateNotVisited:
		case scanStateVisiting:
			return fmt.Errorf("scan state %q: visiting", sched.path.MaybeToRelative(ctx, string(targetPath(ctx, graph, t))))
		case scanStateDone, scanStateIgnored:
			continue
		}

		validationQueue, err = scheduleTarget(ctx, sched, graph, t, nil, sched.prepare, validationQueue)
		if err != nil {
			return fmt.Errorf("failed in schedule %s: %w", sched.path.MaybeToRelative(ctx, string(targetPath(ctx, graph, t))), err)
		}
	}
	if !sched.prepare {
		for len(validationQueue) > 0 {
			t := validationQueue[0]
			validationQueue = validationQueue[1:]
			switch sched.plan.targets[t].scan {
			case scanStateNotVisited:
			case scanStateVisiting:
				return fmt.Errorf("scan state %q: visiting", sched.path.MaybeToRelative(ctx, string(targetPath(ctx, graph, t))))
			case scanStateDone, scanStateIgnored:
				continue
			}
			validationQueue, err = scheduleTarget(ctx, sched, graph, t, nil, false, validationQueue)
			if err != nil {
				return fmt.Errorf("failed in schedule %s: %w", sched.path.MaybeToRelative(ctx, string(targetPath(ctx, graph, t))), err)
			}
		}
	}
	return sched.finish(ctx, started, graph)
}

// DependencyCycleError is error type for dependency cycle.
type DependencyCycleError struct {
	Targets []string
}

func (d DependencyCycleError) Error() string {
	return fmt.Sprintf("dependency cycle: %s", strings.Join(d.Targets, " -> "))
}

// scheduleTarget schedules a build plan for target, which is required to next StepDef, from graph into sched.
// It also returns validationQueue updated while scheduling.
func scheduleTarget(ctx context.Context, sched *scheduler, graph Graph, target Target, next StepDef, ignore bool, validationQueue []Target) ([]Target, error) {
	targets := sched.plan.targets
	scanState := targets[target].scan
	switch scanState {
	case scanStateNotVisited:
		targets[target].scan = scanStateVisiting
		defer func() {
			if ignore {
				targets[target].scan = scanStateIgnored
				clog.Infof(ctx, "scan state ignore target %s", targetPath(ctx, graph, target))
				return
			}
			if log.V(1) {
				clog.Infof(ctx, "scan state done target %s", targetPath(ctx, graph, target))
			}
			targets[target].scan = scanStateDone
		}()
	case scanStateVisiting:
		return validationQueue, DependencyCycleError{
			Targets: []string{sched.path.MaybeToRelative(ctx, string(targetPath(ctx, graph, target)))},
		}
	case scanStateIgnored:
		if ignore {
			return validationQueue, nil
		}
		clog.Infof(ctx, "need to scan ignored target %s", targetPath(ctx, graph, target))
		targets[target].scan = scanStateVisiting
		defer func() {
			targets[target].scan = scanStateDone
		}()

	case scanStateDone:
		return validationQueue, nil
	}
	if targets[target].source {
		if log.V(1) {
			clog.Infof(ctx, "sched target already marked: %v", targetPath(ctx, graph, target))
		}
		return validationQueue, nil
	}
	if log.V(1) {
		clog.Infof(ctx, "schedule target %v state=%v ignore:%t", targetPath(ctx, graph, target), scanState, ignore)
	}
	newEdge, err := graph.Edge(ctx, target, next)
	switch {
	case err == nil:
		// need to schedule.
	case errors.Is(err, ErrNoTarget):
		if log.V(1) {
			clog.Infof(ctx, "sched target not found? %v", targetPath(ctx, graph, target))
		}
		return validationQueue, err
	case errors.Is(err, ErrTargetIsSource):
		if log.V(1) {
			clog.Infof(ctx, "sched target is source? %v", targetPath(ctx, graph, target))
		}
		return validationQueue, sched.mark(ctx, graph, target, next)
	case errors.Is(err, ErrDuplicateStep):
		if scanState == scanStateIgnored {
			// need to check again.
			// It was ignored, but now required to generate *.h
			clog.Infof(ctx, "need to sched duplicate step for %s", targetPath(ctx, graph, target))
			break
		}
		// this step is already processed.
		if log.V(1) {
			clog.Infof(ctx, "sched duplicate step for %v", targetPath(ctx, graph, target))
		}
		return validationQueue, nil
	default:
		if log.V(1) {
			clog.Warningf(ctx, "sched error for %v: %v", target, err)
		}
		return validationQueue, err
	}
	// mark all other outputs are done, or ignored.
	defer func() {
		nextState := scanStateDone
		if ignore {
			nextState = scanStateIgnored
		}
		for _, out := range newEdge.Outputs {
			switch targets[out].scan {
			case scanStateNotVisited:
				targets[out].scan = nextState
				if log.V(1) {
					clog.Infof(ctx, "scan state %v other target %s", nextState, targetPath(ctx, graph, out))
				}
			}
		}
	}()
	// Add a back reference from targetInfo to Edge.
	targets[target].edge = newEdge

	// Add found validations to the validations queue to be scheduled later.
	validationQueue = append(validationQueue, newEdge.Validations...)
	newStep := newEdge.StepDef
	isPhonyOutput := newStep.Binding("phony_output") != ""
	targets[target].phonyOutput = isPhonyOutput
	if log.V(1) {
		clog.Infof(ctx, "schedule %v inputs:%d outputs:%d", newStep, len(newEdge.Inputs), len(newEdge.Outputs))
	}
	sched.visited++
	next = newStep
	select {
	case <-ctx.Done():
		return validationQueue, fmt.Errorf("interrupted in schedule: %w", context.Cause(ctx))
	default:
	}

	if ignore && sched.prepareHeaderOnly {
		clog.Infof(ctx, "check outputs=%d for %s", len(newEdge.Outputs), targetPath(ctx, graph, target))
		// If this step generates header (even if build dependency
		// doesn't explicitly depend on the header), don't ignore this.
		// b/358693473
	outCheck:
		for _, out := range newEdge.Outputs {
			fname, err := graph.TargetPath(ctx, out)
			if err != nil {
				return validationQueue, fmt.Errorf("schedule bad target %s: %w", targetPath(ctx, graph, out), err)
			}
			switch filepath.Ext(fname) {
			case ".h", ".hxx", ".hpp", ".inc":
				clog.Infof(ctx, "need to schedule for %s", fname)
				ignore = false
				break outCheck
			}
			if log.V(1) {
				clog.Infof(ctx, "schedule %s ignore output=%s", targetPath(ctx, graph, target), fname)
			}
		}
	}
	if log.V(1) {
		clog.Infof(ctx, "target=%s ignore=%t prepareHeaderOnly=%t", targetPath(ctx, graph, target), ignore, sched.prepareHeaderOnly)
	}

	// we might not need to use depfile's dependencies to construct
	// build graph.
	// - if depfile's dependency is source file, the file already exists
	//   so no need to wait for it. doesn't change build graph.
	// - if depfile's dependency is generated file
	//   - and if it is included in step's inputs, or indirect inputs,
	//     then, it just adds redundant edge to build graph. Without
	//     the edge, step's order won't be changed, so no need to add
	//     such edge.
	//   - otherwise, it will change the build graph.
	//     it means original build graph without depfile contains
	//     missing dependencies. It would be better to fix gn/ninja's
	//     build graph, rather than mitigating here in the siso.
	step := &Step{
		def:   newStep,
		state: &stepState{},
	}
	targets[target].step = step
	orderOnlyIndex := len(newEdge.Inputs)
	for i, in := range append(newEdge.Inputs, newEdge.OrderOnly...) {
		if targets[in].scan != scanStateDone {
			// if this target is ignored, but "in" is header,
			// then it will not ignore steps to generate "in"
			// and "in"'s inputs recursively.
			var inIgnore bool
			if ignore && sched.prepareHeaderOnly {
				fname, err := graph.TargetPath(ctx, in)
				if err != nil {
					return validationQueue, fmt.Errorf("schedule bad target %s: %w", targetPath(ctx, graph, in), err)
				}
				switch filepath.Ext(fname) {
				case ".h", ".hxx", ".hpp", ".inc":
				default:
					clog.Infof(ctx, "may ignore schedule for %s", fname)
					inIgnore = true
				}
			}
			validationQueue, err = scheduleTarget(ctx, sched, graph, in, next, inIgnore, validationQueue)
			if err != nil {
				if cycleErr, ok := errors.AsType[DependencyCycleError](err); ok {
					if len(cycleErr.Targets) <= 1 || cycleErr.Targets[0] != cycleErr.Targets[len(cycleErr.Targets)-1] {
						cur := sched.path.MaybeToRelative(ctx, string(targetPath(ctx, graph, in)))
						cycleErr.Targets = append(cycleErr.Targets, cur)
					}
					return validationQueue, cycleErr
				}
				if missingErr, ok := errors.AsType[MissingSourceError](err); ok {
					cur := sched.path.MaybeToRelative(ctx, string(targetPath(ctx, graph, in)))
					missingErr.Deps = append(missingErr.Deps, cur)
					return validationQueue, missingErr
				}
				return validationQueue, fmt.Errorf("schedule %s: %w", targetPath(ctx, graph, in), err)
			}
		}
		if !targets[in].source && !ignore {
			// If in is not marked (i.e. source), some step
			// will generate it, so need to wait for it
			// before running this step.
			//
			// If this step is ignored, no need to add the step
			// to in's wait.  Otherwise, ignored step may be
			// needed in other dependency chain, and add step
			// for that case, so step appeared several times
			// in targets[in].waits, which would run the same
			// step multiple time, and would fail with race.
			targets[in].waits = append(targets[in].waits, step)
			step.nwaits++
		}
		if i < orderOnlyIndex && targets[in].phonyOutput && !isPhonyOutput {
			// non-phony_output rule can't depend on phony_output.
			// i.e. step's outputs should be phony outputs
			// phony_output is always dirty,
			// so such non-phony output rule always rebuild.
			return validationQueue, fmt.Errorf("schedule: non phony_output %q depends on phony_output %q", targetPath(ctx, graph, target), targetPath(ctx, graph, in))
		}
	}
	if ignore {
		clog.Infof(ctx, "sched: ignore target %s", targetPath(ctx, graph, target))
		return validationQueue, nil
	}
	if log.V(1) {
		clog.Infof(ctx, "sched: add target %s: %s", targetPath(ctx, graph, target), newStep)
	}
	step.outputs = newEdge.Outputs
	sched.addStep(ctx, step, graph, target)
	return validationQueue, nil
}

// newScheduler creates new scheduler.
func newScheduler(ctx context.Context, opt schedulerOption) *scheduler {
	var prepareHeaderOnly bool
	if opt.Prepare {
		clog.Infof(ctx, "schedule: prepare mode")
		if experiments.Enabled("prepare-header-only", "prepare header only") {
			clog.Infof(ctx, "schedule: prepare header only mode")
			prepareHeaderOnly = true
		}
	}
	if opt.EnableTrace {
		clog.Infof(ctx, "schedule: enable trace")
	}
	clog.Infof(ctx, "schedule: new: targets=%d", opt.NumTargets)
	var ready priorityQueue
	heap.Init(&ready)
	targets := make([]targetInfo, opt.NumTargets)
	for i := range opt.NumTargets {
		// Default weight is 1.
		// If StepWeight is set, non-phony steps override this in addStep.
		// If the target is phony, it will be set to 0 during scheduling.
		targets[i].weight = 1
	}
	return &scheduler{
		path:   opt.Path,
		hashFS: opt.HashFS,
		plan: &plan{
			q:             make(chan *Step, 1),
			ready:         ready,
			targets:       targets,
			targetsSorted: make([]Target, 0, opt.NumTargets),
		},
		locallyNeeded:     opt.LocallyNeeded,
		stepWeight:        opt.StepWeight,
		prepare:           opt.Prepare,
		prepareHeaderOnly: prepareHeaderOnly,
		enableTrace:       opt.EnableTrace,
	}
}

// classifyLocallyNeeded fills s.locallyNeeded for -output_local_strategy=graph:
// the remote outputs a local step will read, keyed in hashfs.MakeFullpath form.
// The caller runs it only when locallyNeeded != nil.
//
// Handler-added outputs aren't in the graph yet; they fall back to the Lstat
// check in Builder.outputs.
func (s *scheduler) classifyLocallyNeeded(ctx context.Context, graph Graph) {
	set := s.locallyNeeded
	var root string
	if s.path != nil {
		root = s.path.WorkspaceRoot
	}
	// Per output, in reverse topological order: an output is locally-needed if
	// it is an explicitly requested target, if a local (non-phony, non-remote)
	// consumer reads it from disk, or if it feeds a phony/remote consumer that
	// reaches a local one (reaches[c]; over-marks through remote consumers,
	// wasting only bandwidth). Per output, not per step: keeps a compile's .o
	// while dropping its unconsumed .dwo sibling, and keeps a requested output
	// even when an edge sibling has consumers. reaches is indexed by dense
	// Step.idnum (a slice, not a map).
	reaches := make([]bool, s.total+1)
	for _, primary := range slices.Backward(s.plan.targetsSorted) {
		info := &s.plan.targets[primary]
		step := info.step
		if step == nil || info.edge == nil {
			continue
		}
		// IsRemoteRule reads rule.Remote, zero until EnsureRule runs.
		step.def.EnsureRule(ctx)
		isRemote := step.def.IsRemoteRule()

		anyNeeded := false
		for _, o := range info.edge.Outputs {
			needed := s.plan.targets[o].requested
			for _, c := range s.plan.targets[o].waits {
				if needed {
					break
				}
				if c == nil || c.def == nil {
					continue
				}
				// Local consumer, or a phony/remote one that reaches.
				if (!c.def.IsPhony() && !c.def.IsRemoteRule()) || reaches[c.idnum] {
					needed = true
				}
			}
			if !needed {
				continue
			}
			anyNeeded = true
			if !isRemote {
				continue
			}
			// Key as NeedFlush will: MakeFullpath, not a bare Join (it
			// handles an already-absolute target path).
			path, err := graph.TargetPath(ctx, o)
			if err != nil {
				clog.Warningf(ctx, "graph classify: TargetPath for output %v: %v", o, err)
				continue
			}
			set.add(hashfs.MakeFullpath(root, path))
		}
		reaches[step.idnum] = anyNeeded
	}
	set.Freeze()
	clog.Infof(ctx, "schedule: locally-needed outputs %d of %d steps", len(set.m), s.total)
}

// mark marks target (workspace relative) as source file.
func (s *scheduler) mark(ctx context.Context, graph Graph, target Target, next StepDef) error {
	fname, err := graph.TargetPath(ctx, target)
	if err != nil {
		return err
	}
	fi, err := s.hashFS.StatMtime(ctx, s.path.WorkspaceRoot, path.New(fname))
	if err == nil && fi.Target() != "" {
		// resolve symlink for source file.
		fsys := s.hashFS.FileSystem(ctx, s.path.WorkspaceRoot)
		_, err = fsys.StatMtime(fname)
	}
	if err != nil {
		var neededBy string
		if next != nil && len(next.Outputs(ctx)) > 0 {
			neededBy = s.path.MaybeToRelative(ctx, string(next.Outputs(ctx)[0]))
		}
		return MissingSourceError{
			Target:   s.path.MaybeToRelative(ctx, fname),
			NeededBy: neededBy,
			Err:      err,
		}
	}
	s.plan.targets[target].source = true
	return nil
}

func (s *scheduler) progressReport(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	ui.Default.PrintLines(msg)
}

// finish finishes the scheduling.
func (s *scheduler) finish(ctx context.Context, started time.Time, graph Graph) error {
	s.plan.mu.Lock()
	defer s.plan.mu.Unlock()

	// Propagate and accumulate weights in reverse topological order.
	for _, t := range slices.Backward(s.plan.targetsSorted) {
		curTarget := &s.plan.targets[t]
		// Downstream steps may consume any sibling output in edge.Outputs,
		// accumulating criticalPathWeight (which already includes
		// outTarget.weight) onto that sibling rather than t. Including
		// outTarget.weight in max also seeds terminal steps that have no
		// downstream consumers (criticalPathWeight == 0).
		var cpw int
		for _, out := range curTarget.edge.Outputs {
			outTarget := &s.plan.targets[out]
			cpw = max(cpw, outTarget.criticalPathWeight, outTarget.weight)
		}
		curTarget.criticalPathWeight = cpw
		if curTarget.step != nil {
			curTarget.step.weight = cpw
		}
		for _, in := range append(curTarget.edge.Inputs, curTarget.edge.OrderOnly...) {
			inTarget := &s.plan.targets[in]
			if inTarget.source {
				continue
			}
			inTarget.criticalPathWeight = max(inTarget.criticalPathWeight, addCriticalPathWeight(curTarget.criticalPathWeight, inTarget.weight))
			if inTarget.step != nil {
				inTarget.step.weight = inTarget.criticalPathWeight
			}
		}
		step := curTarget.step
		if step != nil && step.ReadyToRun("", Target(0)) {
			if log.V(1) {
				clog.Infof(ctx, "step state: %s ready to run", step.String())
			}
			s.plan.addReady(step)
		}
	}

	// graph strategy only (locallyNeeded is nil otherwise): classify
	// locally-needed outputs before freeing targetsSorted.
	if s.locallyNeeded != nil {
		s.classifyLocallyNeeded(ctx, graph)
	}

	// free up s.plan.targetsSorted since it's not necessary anymore.
	s.plan.targetsSorted = nil

	s.plan.pushReadyUnlocked()
	nready := len(s.plan.q) + len(s.plan.ready)
	npendings := s.plan.npendings
	err := s.actionOutputDirs.wait()
	d := time.Since(started)
	clog.Infof(ctx, "schedule finish pending:%d+ready:%d (node:%d edge:%d) in %s", npendings, nready, len(s.plan.targets), s.visited, d)
	if d < ui.DurationThreshold {
		return err
	}
	s.progressReport("%6s schedule pending:%d+ready:%d (node:%d edge:%d)\n", ui.FormatDuration(d), npendings, nready, len(s.plan.targets), s.visited)
	return err
}

func addCriticalPathWeight(a, b int) int {
	if a == math.MaxInt || b == math.MaxInt || (b > 0 && a > math.MaxInt-b) {
		return math.MaxInt
	}
	return a + b
}

// addStep adds a Step to the scheduler.
// This is called before finish().
func (s *scheduler) addStep(ctx context.Context, step *Step, graph Graph, target Target) {
	s.plan.mu.Lock()
	defer s.plan.mu.Unlock()
	defer func() {
		if time.Since(s.lastProgress) < 1*time.Second {
			return
		}
		nready := len(s.plan.q) + len(s.plan.ready)
		npendings := s.plan.npendings
		s.progressReport("schedule pending:%d+ready:%d (node:%d edge:%d)", npendings, nready, len(s.plan.targets), s.visited)
		s.lastProgress = time.Now()
	}()
	s.total++
	step.idnum = s.total
	s.plan.targetsSorted = append(s.plan.targetsSorted, target)
	if step.def.IsPhony() {
		// It turned out that the step is phony, which doesn't have any weight.
		for _, output := range step.outputs {
			s.plan.targets[output].weight = 0
		}
	} else {
		stepWeight := 1
		if s.stepWeight != nil {
			stepWeight = max(s.stepWeight(step), 1)
		}
		// don't add output for phony targets. https://crbug.com/1517575
		for _, output := range step.outputs {
			s.plan.targets[output].output = true
			s.plan.targets[output].weight = stepWeight
			tp := targetPath(ctx, graph, output)
			// Clear a stale file sitting where a dir output now lives, so
			// the action can create its directory there.
			if IsDirTarget(string(tp)) {
				if err := s.hashFS.ClearStaleFileForDirOutput(ctx, s.path.WorkspaceRoot, DirTargetPath(string(tp))); err != nil {
					clog.Warningf(ctx, "clear stale file for dir output %s: %v", tp, err)
				}
			}
			s.actionOutputDirs.ensure(tp)
		}
	}
	if log.V(1) {
		clog.Infof(ctx, "pending to run: %s (waits: %d)", step, step.NumWaits())
	}
	s.plan.npendings++
}

type planStats struct {
	npendings int
	nready    int
}

func (p *plan) stats() planStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return planStats{
		npendings: p.npendings,
		nready:    len(p.q) + len(p.ready),
	}
}

// pushReady pushes a ready step to q.
// This function is called after planning is done.
func (p *plan) pushReady() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pushReadyUnlocked()
}

// pushReadyUnlocked pushes the highest priority step from ready pool to q.
// This function should be called with mutex held.
func (p *plan) pushReadyUnlocked() {
	if len(p.ready) == 0 {
		return
	}
	select {
	case p.q <- p.ready[0]:
		sent := heap.Pop(&p.ready).(*Step)
		sent.queueDuration = time.Since(sent.queueTime)
	default:
	}
}

// addReady adds a step to ready pool.
// This function should be called with mutex held.
func (p *plan) addReady(s *Step) {
	s.queueTime = time.Now()
	s.queueSize = len(p.ready)
	heap.Push(&p.ready, s)
	p.npendings--
}

func (p *plan) hasReady() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.q) > 0 || len(p.ready) > 0
}

// completeStep updates waiting pools and pushes unblocked steps to ready pool.
func (p *plan) completeStep(ctx context.Context, step *Step) {
	step.releaseTime = time.Now()
	outs := step.outputs
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		clog.Infof(ctx, "build already finished. nothing triggers")
		return
	}

	// Unblock waiting steps and send them to the queue if they are ready.
	npendings := p.npendings
	nready := 0
	for _, out := range outs {
		if log.V(1) {
			clog.Infof(ctx, "done %v", out)
		}
		i := 0
		for _, s := range p.targets[out].waits {
			prevProcessed := step.String()
			if step.metrics.skip || step.def.IsPhony() {
				prevProcessed = step.prevStepIDs[0]
			}
			if s.ReadyToRun(prevProcessed, out) {
				nready++
				if log.V(1) {
					clog.Infof(ctx, "step state: %s ready to run %q", s.String(), s.def.Outputs(ctx)[0])
				}
				p.addReady(s)
				continue
			}
			p.targets[out].waits[i] = s
			i++
		}
		for j := i; j < len(p.targets[out].waits); j++ {
			p.targets[out].waits[j] = nil
		}
		if i == 0 {
			p.targets[out].waits = nil
			continue
		}
		p.targets[out].waits = p.targets[out].waits[:i]
	}
	if log.V(1) {
		if nready > 0 {
			clog.Infof(ctx, "trigger %d. pendings %d -> %d", nready, npendings, p.npendings)
		} else {
			clog.Infof(ctx, "zero-trigger outs=%v", outs)
		}
	}
	p.pushReadyUnlocked()
	if len(p.ready) == 0 && p.npendings == 0 && !p.closed {
		p.closed = true
		clog.Infof(ctx, "no step in pending. closing q")
		close(p.q)
	}
}

func (p *plan) dump(ctx context.Context, graph Graph) {
	p.mu.Lock()
	defer p.mu.Unlock()
	clog.Infof(ctx, "queue = %d pendings=%d", len(p.q), p.npendings)
	clog.Infof(ctx, "closed=%t", p.closed)
	var steps []*Step
	seen := make(map[*Step]bool)
	waits := make(map[string]bool)
	ready := make([]string, 0, len(p.ready))
	for _, s := range p.ready {
		ready = append(ready, s.String())
		seen[s] = true
		steps = append(steps, s)
	}
	waitTargets := 0
	clog.Infof(ctx, "ready=%q", ready)
	for node, ti := range p.targets {
		ws := ti.waits
		if len(ws) > 0 {
			waitTargets++
		}
		path, err := graph.TargetPath(ctx, Target(node))
		if err != nil {
			clog.Warningf(ctx, "invalid node %v: %v", node, err)
			continue
		}
		waits[path] = true
		for _, s := range ws {
			if seen[s] {
				continue
			}
			seen[s] = true
			steps = append(steps, s)
		}
	}
	for _, s := range steps {
		for _, o := range s.def.Outputs(ctx) {
			os := string(o)
			if !waits[os] {
				clog.Infof(ctx, "step %s output:%s no trigger", s, o)
				continue
			}
			delete(waits, os)
		}
	}
	outs := make([]string, 0, len(waits))
	for out := range waits {
		outs = append(outs, out)
	}
	sort.Strings(outs)
	clog.Infof(ctx, "waits=%d no-trigger=%d", waitTargets, len(outs))
	clog.Infof(ctx, "no steps will trigger %q", outs)
}

func suggestTargets(ctx context.Context, sched *scheduler, graph Graph, args ...string) []string {
	rel, err := filepath.Rel(sched.path.AbsBase(), sched.path.WorkspaceRoot)
	if err != nil {
		clog.Warningf(ctx, "failed to get rel to workspace: %v", err)
		return nil
	}
	var suggests []string
	for _, arg := range args {
		_, err := graph.Targets(ctx, arg)
		if err == nil {
			// this target is ok as is.
			suggests = append(suggests, arg)
			continue
		}
		target := strings.TrimSuffix(arg, "^")
		_, err = sched.hashFS.Stat(ctx, sched.path.WorkspaceRoot, path.New(sched.path.MaybeFromRelative(ctx, target)))
		if err == nil {
			// just missing ^?
			target := string(path.New(target)) + "^"
			_, err = graph.Targets(ctx, target)
			if err == nil {
				suggests = append(suggests, target)
				continue
			}
		}
		_, err = sched.hashFS.Stat(ctx, sched.path.WorkspaceRoot, path.New(target))
		if err == nil {
			// wrong relative dir?
			target := filepath.Join(rel, target) + "^"
			_, err = graph.Targets(ctx, target)
			if err == nil {
				suggests = append(suggests, target)
				continue
			}
		}
		t, err := graph.SpellcheckTarget(target)
		if err == nil {
			suggests = append(suggests, t)
			continue
		}
		t, err = graph.SpellcheckTarget(filepath.Join(rel, target))
		if err == nil {
			suggests = append(suggests, t+"^")
			continue
		}
	}
	return suggests
}

// prepare all output directories for local process,
// to minimize mkdir operations.
// if we create out dirs before each action concurrently,
// need to check the dir many times and worry about race.
// SISO_EXPERIMENTS=disable-ensure-output-dirs disables this.
// gn: https://crbug.com/gn/461698357
// soong: b/461917619 - ok to disable
type ensureActionOutputDirs struct {
	req       chan path.Path
	done      chan error
	err       error
	knownDirs map[path.Path]struct{}
}

func (e *ensureActionOutputDirs) init() {
	e.req = make(chan path.Path, 1000)
	e.done = make(chan error)
	e.knownDirs = make(map[path.Path]struct{})
}

func (e *ensureActionOutputDirs) run(ctx context.Context, bpath *Path) {
	defer func() {
		e.done <- e.err
	}()
	for {
		select {
		case <-ctx.Done():
			e.err = context.Cause(ctx)
			return
		case fname, ok := <-e.req:
			if !ok {
				return
			}
			// Make the parent of each output, not the output itself: a dir
			// output is the action's to create, like a file. DirTargetPath
			// strips the trailing slash so a dir target yields its parent.
			dir := path.New(DirTargetPath(string(fname))).Dir()
			if !dir.IsAbs() {
				dir = path.JoinRoot(bpath.WorkspaceRoot, dir)
			}
			_, found := e.knownDirs[dir]
			if found {
				continue
			}
			err := os.MkdirAll(dir.OSPath(), 0755)
			if err != nil && e.err == nil {
				e.err = err
				continue
			}
			e.knownDirs[dir] = struct{}{}
			dir = dir.Dir()
			for {
				_, found := e.knownDirs[dir]
				if found {
					break
				}
				e.knownDirs[dir] = struct{}{}
				pdir := dir.Dir()
				if pdir == dir {
					break
				}
				dir = pdir
			}
		}
	}
}

func (e *ensureActionOutputDirs) ensure(fname path.Path) {
	if e.req == nil {
		return
	}
	e.req <- fname
}

func (e *ensureActionOutputDirs) wait() error {
	if e.req == nil {
		return nil
	}
	close(e.req)
	return <-e.done
}
