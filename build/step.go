// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"iter"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	log "github.com/golang/glog"

	"go.chromium.org/build/hashigo/digest"
	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/execute"
	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/o11y/trace"
	sisopath "go.chromium.org/build/siso/path"
	"go.chromium.org/build/siso/reapi"
)

// StepDef is a build step definition.
// Unless specified, path is workspace relative.
type StepDef interface {
	// String returns id of the step.
	String() string

	// Next returns next step's def.
	Next() StepDef

	// Ensure siso rule is applied
	EnsureRule(context.Context)

	// RuleName returns rule name of the step. empty for no rule.
	RuleName() string

	// ActionName returns action name of the step.
	ActionName() string

	// Args returns command line arguments of the step.
	Args(context.Context) []string

	// IsPhony returns true if the step is phony.
	IsPhony() bool

	// CmdHash returns command hash of the step.
	CmdHash() []byte

	// Binding returns binding value.
	Binding(string) string

	// Depfile returns workspace relative depfile path, or empty if not set.
	Depfile(context.Context) sisopath.Path

	// Rspfile returns workspace relative rspfile path, or empty if not set.
	Rspfile(context.Context) sisopath.Path

	// Inputs returns inputs of the step.
	Inputs(context.Context) []sisopath.Path

	// TriggerInputs returns inputs of the step that would trigger
	// the step's action.  no order-only.
	// For  deps in deps log, use DepInputs.
	TriggerInputs(context.Context) []sisopath.Path

	// ExpandedTriggerInputs returns trigger inputs of the step with phony
	// targets expanded recursively to their underlying trigger inputs.
	ExpandedTriggerInputs(context.Context) []sisopath.Path

	// DepInputs returns iterator for inputs via depfile of the step.
	// if depfile is not set, returns emptyIter, nil
	// if depfile or deplog is not found, returns wrapped ErrMissingDeps.
	DepInputs(context.Context) (iter.Seq[sisopath.Path], error)

	// DepsBaseInputs returns inputs of the step, which will be combined
	// with scandeps results (bool arg false),
	// or will be trimmed down by scandeps results (bool arg true).
	DepsBaseInputs(context.Context, []sisopath.Path, bool) []sisopath.Path

	// ToolInputs returns tool inputs of the step.
	// ToolInputs is added to deps inputs.
	ToolInputs(context.Context) []sisopath.Path

	// ExpandedCaseSensitives returns expanded filenames if platform is case-sensitive.
	ExpandedCaseSensitives(context.Context, []sisopath.Path) []sisopath.Path

	// ExpandedInputs returns expanded inputs of the step.
	ExpandedInputs(context.Context) []sisopath.Path

	// RemoteInputs maps file used in remote to file exists on local.
	// path in remote action -> local path
	RemoteInputs() map[sisopath.Path]sisopath.Path

	// CheckMissingDeps checks if depInputs can be found in direct/indirect inputs.
	// It returns whether any missing dependency is a known issue, and an error describing
	// any missing dependencies found.
	CheckMissingDeps(context.Context, []sisopath.Path) (known bool, err error)

	// Handle runs a handler for the cmd.
	Handle(context.Context, *execute.Cmd) error

	// Outputs returns outputs of the step.
	Outputs(context.Context) []sisopath.Path

	// LocalOutputs returns outputs of the step that should be written to the local disk.
	LocalOutputs(context.Context) []sisopath.Path

	// AuxiliaryLogOutputFiles returns output files that siso explicitly logs digest of
	// but doesn't download to disk.
	AuxiliaryLogOutputFiles(context.Context) []sisopath.Path

	// AuxiliaryLogOutputDirs returns output directories that siso explicitly logs digest of
	// but doesn't download to disk.
	AuxiliaryLogOutputDirs(context.Context) []sisopath.Path

	// Pure indicates the step is pure or not.
	Pure() bool

	// Platform returns platform properties for remote execution.
	Platform() map[string]string

	// IsRemoteRule reports whether the step is declared remote-executable in
	// the stepconfig (the static rule.Remote flag). Unlike Platform(), it is
	// stable from scheduling on, even when Platform() is cleared at runtime
	// (pool=console, scandeps fallback).
	IsRemoteRule() bool

	// Sandbox returns properties for action sandboxing.
	Sandbox() map[string]string

	// RecordDeps records deps.
	RecordDeps(context.Context, string, time.Time, digest.Digest, []string) (bool, error)

	// RuleFix returns required fix for the rule of the step.
	RuleFix(ctx context.Context, inadds, outadds []string) []byte
}

// Edge contains StepDef and the dependency targets of the edge.
type Edge struct {
	StepDef     StepDef
	Inputs      []Target
	OrderOnly   []Target
	Outputs     []Target
	Validations []Target
}

// Step is a build step.
type Step struct {
	idnum                      int
	def                        StepDef
	nwaits                     int
	weight                     int
	outputs                    []Target
	outputPaths                []string // target name in ninja, i.e. output path relative to wd.
	enforceDepfileOnlyPromotes bool

	cmd *execute.Cmd

	readyTime time.Time
	// prevStepIDs[0] is the step that most recently released one of this
	// step's inputs, prevStepIDs[1] the distinct one before it.
	prevStepIDs   [2]string
	queueTime     time.Time
	queueSize     int
	queueDuration time.Duration

	// releaseTime is when completeStep handed the outputs to the steps
	// waiting on them. Work after it, such as the depfile flush, delays
	// nothing downstream.
	releaseTime time.Time

	// startTime is the time that the step starts running in a dedicated goroutine.
	// There might be other semaphore throttling for local exec, scandeps, remote exec.
	startTime time.Time

	// cmdRunTime is set immediately prior to calling `Run` on local
	// steps, indicating that the command has begun executing.
	cmdRunTime time.Time

	// endTime is the time that the step ends.
	endTime time.Time

	metrics StepMetric

	// Whether we have reported the start of this step via BuildActionStarted()
	startReported *sync.Once

	// Whether we have reported the finish of this step via BuildActionFinished/Canceled()
	finishReported *sync.Once

	state *stepState

	// depsExpanded is set once depsExpandInputs has run. See ensureExpanded.
	depsExpanded bool
}

// Clone creates a shallow clone of the step with a cloned Cmd
// and fresh mutable state, suitable for the local racer in racing mode.
func (s *Step) Clone() *Step {
	return &Step{
		idnum:          s.idnum,
		def:            s.def,
		weight:         s.weight,
		outputs:        s.outputs,
		outputPaths:    s.outputPaths,
		cmd:            s.cmd.Clone(),
		readyTime:      s.readyTime,
		startTime:      s.startTime,
		state:          &stepState{},
		startReported:  s.startReported,
		finishReported: s.finishReported,
		metrics:        s.metrics,
		depsExpanded:   s.depsExpanded,
	}
}

// IDNum is a step identification number.
func (s *Step) IDNum() int { return s.idnum }

// Metrics is the collection of metrics for the step.
func (s *Step) Metrics() StepMetric {
	return s.metrics
}

// Desc returns step's description.
func (s *Step) Desc() string {
	return s.cmd.Desc
}

// Command returns step's command line.
func (s *Step) Command() string {
	return s.def.Binding("command")
}

// Binding returns binding value of the step for name.
func (s *Step) Binding(name string) string {
	return s.def.Binding(name)
}

// NewStepForTest returns a Step initialized for external package testing.
func NewStepForTest(idnum int, def StepDef) *Step {
	return &Step{
		idnum: idnum,
		def:   def,
		cmd:   &execute.Cmd{},
		state: &stepState{},
	}
}

// SetMetricsForTest sets the StepMetric of the step for external package testing.
func (s *Step) SetMetricsForTest(metrics StepMetric) {
	s.metrics = metrics
}

// SetCmdForTest sets the Cmd of the step for external package testing.
func (s *Step) SetCmdForTest(cmd *execute.Cmd) {
	s.cmd = cmd
}

// Outputs returns step's outputs (target name in ninja, i.e. output path relative to wd.)
func (s *Step) Outputs() []string {
	return s.outputPaths
}

// IsConsole reports whether it uses console pool.
func (s *Step) IsConsole() bool {
	return s.cmd.Console
}

// ExitCode returns exit code of the step.
func (s *Step) ExitCode() int32 {
	return s.cmd.ExitCode()
}

// Stdout returns step's stdout.
func (s *Step) Stdout() []byte {
	return s.cmd.Stdout()
}

// Stderr returns step's stderr.
func (s *Step) Stderr() []byte {
	return s.cmd.Stderr()
}

// OutputResult returns output result of the step.
func (s *Step) OutputResult() string {
	return s.cmd.OutputResult()
}

type stepState struct {
	mu               sync.Mutex
	phase            stepPhase
	weightedDuration time.Duration

	// start time to wait the step to serv.
	// i.e. local semaphore, remote semaphore.
	waitStart time.Time

	// accumulated wait duration in the step.
	waitDuration time.Duration
}

func (s *stepState) SetPhase(phase stepPhase) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.phase = phase
	if phase.isWait() {
		s.waitStart = time.Now()
	} else {
		if !s.waitStart.IsZero() {
			s.waitDuration += time.Since(s.waitStart)
		}
		s.waitStart = time.Time{}
	}
}

func (s *stepState) Phase() stepPhase {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.phase
}

func (s *stepState) WaitDuration() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	dur := s.waitDuration
	if !s.waitStart.IsZero() {
		dur += time.Since(s.waitStart)
	}
	return dur
}

// copyExecResult copies stepState fields from a local racing clone back
// to the original stepState. weightedDuration is excluded: it is
// progress-owned and only accumulated on the original step.
func (s *stepState) copyExecResult(src *stepState) {
	s.mu.Lock()
	src.mu.Lock()
	defer s.mu.Unlock()
	defer src.mu.Unlock()
	s.phase = src.phase
	s.waitStart = src.waitStart
	s.waitDuration = src.waitDuration
}

// NumWaits returns number of waits for the step.
func (s *Step) NumWaits() int {
	return s.nwaits
}

// ReadyToRun checks whether the step is ready to run
// when prev step's out becomes ready.
func (s *Step) ReadyToRun(prev string, out Target) bool {
	if out != 0 {
		s.nwaits--
	}
	if prev != s.prevStepIDs[0] {
		s.prevStepIDs[1] = s.prevStepIDs[0]
		s.prevStepIDs[0] = prev
	}
	ready := s.nwaits == 0
	if ready {
		s.readyTime = time.Now()
	}
	return ready
}

// String returns id of the step.
func (s *Step) String() string {
	return s.def.String()
}

// stepPhase is a named phase of a step.
// It's used for multiple purposes:
//  1. Named display of a step's state in the progress ui via [stepPhase.String].
//  2. Accumulating time the step waited on something, using FooWait and FooRun pairs.
//     In [stepState.SetPhase], if a phase matches a known "wait" phase we start
//     measuring time until a non-wait phase.
type stepPhase int

const (
	stepPhaseNone stepPhase = iota
	stepStart
	stepHandler
	stepPreproc
	stepInput
	stepLocalWait
	stepLocalRun
	stepREWrapperWait
	stepREWrapperRun
	stepRemoteWait
	stepRemoteRun
	stepFallbackWait
	stepFallbackRun
	stepRetryWait
	stepRetryRun
	stepOutput
	stepCacheCheck
	stepCacheWrite
	stepCacheWriteWait
	stepDone
)

func (s stepPhase) String() string {
	switch s {
	case stepPhaseNone:
		return "none"
	case stepStart:
		return "start"
	case stepHandler:
		return "handler"
	case stepPreproc:
		return "prep"
	case stepInput:
		return "input"
	case stepLocalWait:
		return "wait-local"
	case stepLocalRun:
		return "local"
	case stepREWrapperWait:
		return "wait-rewrap"
	case stepREWrapperRun:
		return "rewrap"
	case stepRemoteWait:
		return "wait-remote"
	case stepRemoteRun:
		return "remote"
	case stepFallbackWait:
		return "wait-fallback"
	case stepFallbackRun:
		return "fallback"
	case stepRetryWait:
		return "wait-retry"
	case stepRetryRun:
		return "retry"
	case stepOutput:
		return "output"
	case stepCacheCheck:
		return "cache-check"
	case stepCacheWrite:
		return "cache-write"
	case stepCacheWriteWait:
		return "wait-cache-write"
	case stepDone:
		return "done"
	default:
		return "unknown"
	}
}

var phaseWaitPairs = [...]struct {
	run  stepPhase
	wait stepPhase
}{
	{stepLocalRun, stepLocalWait},
	{stepREWrapperRun, stepREWrapperWait},
	{stepRemoteRun, stepRemoteWait},
	{stepFallbackRun, stepFallbackWait},
	{stepRetryRun, stepRetryWait},
	{stepCacheWrite, stepCacheWriteWait},
}

func (s stepPhase) wait() stepPhase {
	for _, p := range phaseWaitPairs {
		if s == p.run {
			return p.wait
		}
	}
	return s
}

func (s stepPhase) isWait() bool {
	for _, p := range phaseWaitPairs {
		if s == p.wait {
			return true
		}
	}
	return false
}

// setPhase sets a phase of the step.
func (s *Step) setPhase(phase stepPhase) {
	s.state.SetPhase(phase)
}

// phase returns the phase of the step.
func (s *Step) phase() stepPhase {
	if s.state == nil {
		return stepPhaseNone
	}
	return s.state.Phase()
}

func (s *Step) servDuration() time.Duration {
	if s.state == nil {
		return time.Since(s.startTime)
	}
	return time.Since(s.startTime) - s.state.WaitDuration()
}

func (s *Step) displayDuration() time.Duration {
	if s.phase().isWait() {
		if s.state != nil {
			s.state.mu.Lock()
			defer s.state.mu.Unlock()
			if !s.state.waitStart.IsZero() {
				return time.Since(s.state.waitStart)
			}
		}
		return 0
	}
	return s.servDuration()
}

// Done checks the step is done.
func (s *Step) Done() bool {
	return s.state.Phase() == stepDone
}

func (s *Step) addWeightedDuration(d time.Duration) {
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	if s.state.phase == stepDone {
		return
	}
	s.state.weightedDuration += d
}

func (s *Step) getWeightedDuration() time.Duration {
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	return s.state.weightedDuration
}

func stepSpanName(stepDef StepDef) string {
	if !strings.HasPrefix(stepDef.ActionName(), "__") {
		return stepDef.ActionName()
	}
	cmd := stepDef.Binding("command")
	i := strings.Index(cmd, " ")
	if i > 0 {
		// use python script as step name, not python binary itself.
		arg0 := cmd[:i]
		if strings.HasSuffix(strings.TrimSuffix(arg0, ".exe"), "python3") {
			cmd = cmd[i+1:]
		}
	}
	i = strings.Index(cmd, " ")
	if i > 0 {
		cmd = cmd[:i]
	}
	return cmd
}

func stepBacktraces(ctx context.Context, step *Step) []string {
	var locs []string
	var prev string
	for s := step.def; s != nil; s = s.Next() {
		var loc string
		if gnTarget := s.Binding("gn_target"); gnTarget != "" {
			loc = gnTarget
		} else {
			outs := s.Outputs(ctx)
			loc = stepSpanName(s)
			if len(outs) > 0 {
				out := string(outs[0])
				if odir := filepath.Dir(out); odir != "." {
					out = odir
				}
				loc = fmt.Sprintf("%s %s", loc, out)
			}
		}
		if loc != prev {
			locs = append(locs, loc)
			prev = loc
		}
	}
	return locs
}

func (s *Step) init(ctx context.Context, b *Builder, stepManifest *stepManifest) {
	ctx, span := trace.NewSpan(ctx, "step-init")
	defer span.Close(nil)
	s.startReported = new(sync.Once)
	s.finishReported = new(sync.Once)
	s.def.EnsureRule(ctx)
	s.outputPaths = make([]string, 0, len(stepManifest.outputs))
	for _, out := range stepManifest.outputs {
		s.outputPaths = append(s.outputPaths, b.path.MaybeToRelative(ctx, string(out)))
	}
	s.cmd = newCmd(ctx, b, s.def, stepManifest)
	if log.V(1) {
		clog.Infof(ctx, "cmdhash:%s", base64.StdEncoding.EncodeToString(s.cmd.CmdHash))
	}
}

func newCmd(ctx context.Context, b *Builder, stepDef StepDef, stepManifest *stepManifest) *execute.Cmd {
	// we need to use stepDef.Outputs instead of stepManifest.outputs
	// to reflect outputs specified by siso rule.
	// reusing stepManifest.outputs is optimization.
	outputs := stepManifest.outputs
	if stepDef.RuleName() != "" {
		// siso rule applied. update outputs by rule
		newouts := stepDef.Outputs(ctx)
		if !slices.Equal(outputs, newouts) {
			outputs = newouts
		}
	}

	// add build.ninja as outputs of gn step.
	// gn uses
	//
	//  build build.ninja.stamp: gn
	//    generator = 1
	//    depfile = build.ninja.d
	//  build build.ninja: phony build.ninja.stamp
	//    generator = 1
	//
	// so, step "gn" has build.ninja.stamp as output, but
	// not build.ninja. but it generates build.ninja
	// so add it as output to make timestamp of build.ninja
	// correctly managed by Siso.
	// This workaround is needed to make second build as null build.
	if stepDef.ActionName() == "gn" && len(outputs) == 1 && filepath.Base(string(outputs[0])) == "build.ninja.stamp" {
		outputs = append(outputs, sisopath.New(b.path.MaybeFromRelative(ctx, "build.ninja")))
	}

	cmd := &execute.Cmd{
		ID:                      stepDef.String(),
		Desc:                    stepDescription(stepDef),
		ActionName:              stepDef.ActionName(),
		Args:                    b.argTab.InternSlice(stepDef.Args(ctx)),
		RSPFile:                 stepDef.Rspfile(ctx),
		RSPFileContent:          []byte(stepDef.Binding("rspfile_content")),
		CmdHash:                 stepManifest.cmdHash,
		WorkspaceRoot:           b.path.WorkspaceRoot, // use step binding?
		WorkDir:                 sisopath.Path(b.path.BaseDir),
		Inputs:                  stepInputs(ctx, stepDef),
		ToolInputs:              stepDef.ToolInputs(ctx),
		Outputs:                 stepFileOutputs(outputs),
		OutputDirs:              stepDirOutputs(outputs),
		EdgeHash:                stepManifest.edgeHash,
		UseSystemInput:          stepDef.Binding("use_system_input") != "",
		Deps:                    stepDef.Binding("deps"),
		Depfile:                 stepDef.Depfile(ctx),
		AuxiliaryLogOutputFiles: stepDef.AuxiliaryLogOutputFiles(ctx),
		AuxiliaryLogOutputDirs:  stepDef.AuxiliaryLogOutputDirs(ctx),

		Restat:        stepDef.Binding("restat") != "",
		RestatContent: stepRestatContent(stepDef),

		Pure: stepDef.Pure(),

		HashFS: b.hashFS,

		REAPIVersion:    b.reapiclient.APIVersion(),
		Platform:        stepDef.Platform(),
		RemoteWrapper:   stepDef.Binding("remote_wrapper"),
		RemoteCommand:   stepDef.Binding("remote_command"),
		RemoteInputs:    stepDef.RemoteInputs(),
		CanonicalizeDir: stepDef.Binding("canonicalize_dir") != "false",

		// TODO(b/266518906): enable DoNotCache for read-only client
		// DoNotCache: !b.reCacheEnableWrite,
		SkipCacheLookup: !b.reCacheEnableRead,
		Timeout:         stepTimeout(ctx, stepDef.Binding("timeout")),
		ExecTimeout:     execTimeout(ctx, stepDef.Binding("exec_timeout")),
		ActionSalt:      b.actionSalt,
	}
	if _, ok := cmd.Platform["InputRootAbsolutePath"]; ok {
		// non relocatable request. disable canonicalize dir.
		cmd.CanonicalizeDir = false
	}
	if stepDef.Binding("phony_output") != "" {
		clog.Infof(ctx, "phony_output: no outputs by cmd")
		cmd.Outputs = nil
		cmd.OutputDirs = nil
	}
	if envfile := stepDef.Binding("envfile"); envfile != "" {
		cmd.Env = b.loadEnvfile(ctx, envfile)
	}
	if stepDef.Binding("pool") == "console" {
		// pool=console needs to attach stdin/stdout/stderr
		// so run locally.
		cmd.Platform = nil
		cmd.RemoteWrapper = ""
		cmd.RemoteCommand = ""
		cmd.RemoteInputs = nil
	} else if experiments.Enabled("gvisor", "Force gVisor") {
		if len(cmd.Platform) == 0 {
			cmd.Platform = map[string]string{}
		}
		cmd.Platform["dockerRuntime"] = "runsc"
	}
	if stepDef.Binding("cache_disabled") == "true" {
		cmd.SkipCacheLookup = true
		cmd.DoNotCache = true
		clog.Infof(ctx, "cache disabled")
	}
	cmd.InitOutputs()

	if experiments.Enabled("oom-score-adj", "Set oom_score_adj=1000 on local exec") {
		score := 1000 // OOM_SCORE_ADJ_MAX. likely to be killed by OOM
		cmd.OOMScoreAdj = &score
	}
	return cmd
}

// stepRestatContent reports whether the step should use content-based restat.
// An explicit restat_content binding (in the Ninja rule/edge or the Starlark
// step config) always wins. Otherwise, with the auto-restat-content
// experiment, restat=true is treated as restat_content.
// Outputs written locally still restat by mtime; see computeOutputEntries.
func stepRestatContent(stepDef StepDef) bool {
	switch stepDef.Binding("restat_content") {
	case "true":
		return true
	case "false":
		return false
	}
	// TODO(b/575621006): enable by default once validated on builders.
	return stepDef.Binding("restat") != "" &&
		experiments.Enabled("auto-restat-content", "restat=true is treated as restat_content")
}

func stepTimeout(ctx context.Context, d string) time.Duration {
	const defaultTimeout = 1 * time.Hour
	if d == "" {
		return defaultTimeout
	}
	dur, err := time.ParseDuration(d)
	if err != nil {
		clog.Warningf(ctx, "failed to parse duration %q: %v", d, err)
		return defaultTimeout
	}
	return dur
}

func execTimeout(ctx context.Context, d string) time.Duration {
	if d == "" {
		return 0
	}
	dur, err := time.ParseDuration(d)
	if err != nil {
		clog.Warningf(ctx, "failed to parse duration %q: %v", d, err)
		return 0
	}
	return dur
}

func stepInputs(ctx context.Context, stepDef StepDef) []sisopath.Path {
	seen := make(map[sisopath.Path]bool)
	var inputs []sisopath.Path
	for _, in := range stepDef.Inputs(ctx) {
		if seen[in] {
			continue
		}
		seen[in] = true
		inputs = append(inputs, in)
	}
	depsIter, err := stepDef.DepInputs(ctx)
	if err != nil {
		return inputs
	}
	depsIter(func(in sisopath.Path) bool {
		if seen[in] {
			return true
		}
		seen[in] = true
		inputs = append(inputs, in)
		return true
	})
	return inputs
}

// stepFileOutputs returns file outputs (non-directory) with paths as-is.
func stepFileOutputs(outputs []sisopath.Path) []sisopath.Path {
	var files []sisopath.Path
	for _, out := range outputs {
		if !IsDirTarget(string(out)) {
			files = append(files, out)
		}
	}
	return files
}

// stepDirOutputs returns directory outputs with trailing slash stripped.
func stepDirOutputs(outputs []sisopath.Path) []sisopath.Path {
	var dirs []sisopath.Path
	for _, out := range outputs {
		if IsDirTarget(string(out)) {
			dirs = append(dirs, sisopath.New(DirTargetPath(string(out))))
		}
	}
	return dirs
}

func stepDescription(stepDef StepDef) string {
	s := stepDef.Binding("description")
	if s != "" {
		return s
	}
	return stepDef.Binding("command")
}

func validateRemoteActionResult(result *rpb.ActionResult) bool {
	return reapi.ValidateActionResult(result)
}

type envfile struct {
	once sync.Once
	envs []string
}

func (b *Builder) loadEnvfile(ctx context.Context, fname string) []string {
	env := &envfile{}
	v, loaded := b.envFiles.LoadOrStore(fname, env)
	if loaded {
		env = v.(*envfile)
	}
	env.once.Do(func() {
		// https://ninja-build.org/manual.html#_extra_tools
		// ninja -t msvc -e ENVFILE -- cl.exe <arguments>
		//  Where ENVFILE is a binary file that contains an environment block suitable for CreateProcessA() on Windows (i.e. a series of zero-terminated strings that look like NAME=VALUE, followed by an extra zero terminator).
		buf, err := b.hashFS.ReadFile(ctx, b.path.WorkspaceRoot, sisopath.New(b.path.MaybeFromRelative(ctx, fname)))
		if err != nil {
			clog.Warningf(ctx, "failed to load envfile %q: %v", fname, err)
			return
		}
		for len(buf) > 0 {
			i := bytes.IndexByte(buf, '\000')
			if i < 0 {
				env.envs = append(env.envs, string(buf))
				break
			}
			e := string(buf[:i])
			buf = buf[i+1:]
			if e != "" {
				env.envs = append(env.envs, e)
			}
		}
		clog.Infof(ctx, "load envfile %q: %d", fname, len(env.envs))
	})
	return env.envs
}
