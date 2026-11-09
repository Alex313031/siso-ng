// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"errors"
	"math"
	"runtime"
	"slices"
	"testing"
	"time"
)

func TestLocallyNeededSet(t *testing.T) {
	var nilSet *LocallyNeededSet
	if !nilSet.Has("anything") {
		t.Errorf("nil set should return true (conservative default)")
	}

	s := NewLocallyNeededSet()
	if s.Has("not-yet-populated") {
		t.Errorf("unfrozen set should return false (keeps CAS-only outputs valid at load)")
	}

	s.add("needed/foo.o")
	s.add("needed/bar.o")

	if s.Has("not-in-map-but-unfrozen") {
		t.Errorf("unfrozen set should still return false even after adds")
	}

	s.Freeze()

	if !s.Has("needed/foo.o") {
		t.Errorf("frozen set: expected Has(foo.o)=true")
	}
	if s.Has("not-needed/baz.o") {
		t.Errorf("frozen set: expected Has(baz.o)=false")
	}
	if got, want := len(s.m), 2; got != want {
		t.Errorf("len=%d; want %d", got, want)
	}

	// OutputLocal is the predicate face; it mirrors Has.
	ctx := t.Context()
	if !s.OutputLocal(ctx, "needed/foo.o") || s.OutputLocal(ctx, "not-needed/baz.o") {
		t.Errorf("OutputLocal should mirror Has")
	}
}

// classifyStepDef embeds fakeStepDef and makes IsRemoteRule/IsPhony
// controllable, the two facts classifyLocallyNeeded reads off a step.
type classifyStepDef struct {
	fakeStepDef
	remote bool
	phony  bool
}

func (d classifyStepDef) IsRemoteRule() bool { return d.remote }
func (d classifyStepDef) IsPhony() bool      { return d.phony }

// classifyGraph embeds fakeGraph and resolves Target -> path from a map,
// the only graph method classifyLocallyNeeded uses.
type classifyGraph struct {
	fakeGraph
	paths map[Target]string
}

func (g classifyGraph) TargetPath(_ context.Context, t Target) (string, error) {
	if p, ok := g.paths[t]; ok {
		return p, nil
	}
	return "", errors.New("classifyGraph: no path for target")
}

// TestClassifyLocallyNeeded covers the per-output decision: a remote compile's
// .o (consumed by a local link) is marked, its .dwo (consumed by nothing) is
// not; a remote leaf is marked; a local step's own output is left alone.
func TestClassifyLocallyNeeded(t *testing.T) {
	ctx := t.Context()
	const (
		oObj Target = iota // remote compile .o, consumed by the local link
		oDwo               // remote compile .dwo, consumed by nothing
		oBin               // local link output (a leaf)
		oArt               // remote leaf output (requested artifact)
	)
	compileStep := NewStepForTest(1, classifyStepDef{remote: true})
	linkStep := NewStepForTest(2, classifyStepDef{remote: false})
	leafStep := NewStepForTest(3, classifyStepDef{remote: true})

	targets := make([]targetInfo, 4)
	targets[oObj] = targetInfo{
		step:  compileStep,
		edge:  &Edge{Outputs: []Target{oObj, oDwo}},
		waits: []*Step{linkStep}, // the local link consumes the .o
	}
	// oBin and oArt are the explicitly requested build targets.
	targets[oBin] = targetInfo{step: linkStep, edge: &Edge{Outputs: []Target{oBin}}, requested: true}
	targets[oArt] = targetInfo{step: leafStep, edge: &Edge{Outputs: []Target{oArt}}, requested: true}

	set := NewLocallyNeededSet()
	s := &scheduler{
		path:          &Path{WorkspaceRoot: "/w"},
		locallyNeeded: set,
		total:         3, // max Step.idnum; sizes the reaches slice
		plan: &plan{
			targets:       targets,
			targetsSorted: []Target{oObj, oBin, oArt}, // producer-first
		},
	}
	g := classifyGraph{paths: map[Target]string{
		oObj: "out/foo.o", oDwo: "out/foo.dwo", oBin: "out/chrome", oArt: "out/art",
	}}

	s.classifyLocallyNeeded(ctx, g)

	// Has is queried in the absolute makeFullpath form, so assert that form.
	if !set.Has("/w/out/foo.o") {
		t.Errorf(".o with a local consumer should be materialized")
	}
	if set.Has("/w/out/foo.dwo") {
		t.Errorf(".dwo (no consumer, sibling of the marked .o) must stay in CAS")
	}
	if !set.Has("/w/out/art") {
		t.Errorf("remote leaf (requested artifact) should be materialized")
	}
	if set.Has("/w/out/chrome") {
		t.Errorf("a local step's own output is on disk already, must not be marked")
	}
	if got, want := len(set.m), 2; got != want {
		t.Errorf("len=%d; want %d (.o and the remote leaf)", got, want)
	}
}

// TestClassifyLocallyNeeded_RequestedSibling: an explicitly requested output
// is materialized even when an edge sibling has a consumer.
func TestClassifyLocallyNeeded_RequestedSibling(t *testing.T) {
	ctx := t.Context()
	const (
		oObj Target = iota // remote .o, consumed by the local link
		oReq               // remote sibling, no consumer, but explicitly requested
		oBin               // local link output
	)
	compileStep := NewStepForTest(1, classifyStepDef{remote: true})
	linkStep := NewStepForTest(2, classifyStepDef{remote: false})

	targets := make([]targetInfo, 3)
	targets[oObj] = targetInfo{
		step:  compileStep,
		edge:  &Edge{Outputs: []Target{oObj, oReq}},
		waits: []*Step{linkStep},
	}
	targets[oReq].requested = true // user asked for the unconsumed sibling
	targets[oBin] = targetInfo{step: linkStep, edge: &Edge{Outputs: []Target{oBin}}, requested: true}

	set := NewLocallyNeededSet()
	s := &scheduler{
		path:          &Path{WorkspaceRoot: "/w"},
		locallyNeeded: set,
		total:         2,
		plan:          &plan{targets: targets, targetsSorted: []Target{oObj, oBin}},
	}
	g := classifyGraph{paths: map[Target]string{oObj: "out/foo.o", oReq: "out/foo.req", oBin: "out/chrome"}}

	s.classifyLocallyNeeded(ctx, g)

	if !set.Has("/w/out/foo.o") {
		t.Errorf(".o with a local consumer should be materialized")
	}
	if !set.Has("/w/out/foo.req") {
		t.Errorf("requested sibling must be materialized even though edge sibling .o has a consumer")
	}
}

// TestClassifyLocallyNeeded_Propagation exercises the reaches[c] branch: an
// output whose only consumer is a phony/remote intermediary is still marked
// when that intermediary reaches a local consumer (the case the strategy
// exists for, and the branch TestClassifyLocallyNeeded never hits).
func TestClassifyLocallyNeeded_Propagation(t *testing.T) {
	ctx := t.Context()
	for _, tc := range []struct {
		name         string
		intermediate classifyStepDef // the sole consumer of oA
	}{
		{"through remote intermediary", classifyStepDef{remote: true}},
		{"through phony intermediary", classifyStepDef{phony: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const (
				oA Target = iota // remote-produced; consumed only by the intermediary
				oB               // produced by the intermediary; consumed by a local step
				oC               // produced by the local step (leaf)
			)
			producer := NewStepForTest(1, classifyStepDef{remote: true})
			intermediate := NewStepForTest(2, tc.intermediate)
			local := NewStepForTest(3, classifyStepDef{remote: false})

			targets := make([]targetInfo, 3)
			targets[oA] = targetInfo{step: producer, edge: &Edge{Outputs: []Target{oA}}, waits: []*Step{intermediate}}
			targets[oB] = targetInfo{step: intermediate, edge: &Edge{Outputs: []Target{oB}}, waits: []*Step{local}}
			targets[oC] = targetInfo{step: local, edge: &Edge{Outputs: []Target{oC}}}

			set := NewLocallyNeededSet()
			s := &scheduler{
				path:          &Path{WorkspaceRoot: "/w"},
				locallyNeeded: set,
				total:         3,
				plan:          &plan{targets: targets, targetsSorted: []Target{oA, oB, oC}},
			}
			g := classifyGraph{paths: map[Target]string{oA: "out/a", oB: "out/b", oC: "out/c"}}

			s.classifyLocallyNeeded(ctx, g)

			if !set.Has("/w/out/a") {
				t.Errorf("oA reaches a local consumer through the intermediary; should be marked")
			}
		})
	}
}

// TestClassifyLocallyNeeded_AbsoluteTargetPath guards the key form for an
// output whose target path is already absolute: the classifier must key it via
// hashfs.MakeFullpath (which passes absolutes through unchanged), matching how
// NeedFlush queries the predicate. A bare filepath.Join(root, abs) would
// double-prefix and silently never match, so the output would be skipped.
func TestClassifyLocallyNeeded_AbsoluteTargetPath(t *testing.T) {
	ctx := t.Context()
	const (
		oAbs Target = iota // remote-produced, target path is absolute
		oBin               // the local consumer's own output
	)
	// "/abs/..." is absolute on Unix but not on Windows (no volume), so build
	// a path the host OS treats as absolute to exercise the passthrough branch.
	absPath := "/abs/gen/foo.o"
	if runtime.GOOS == "windows" {
		absPath = `C:/abs/gen/foo.o`
	}
	producer := NewStepForTest(1, classifyStepDef{remote: true})
	local := NewStepForTest(2, classifyStepDef{remote: false})

	targets := make([]targetInfo, 2)
	targets[oAbs] = targetInfo{step: producer, edge: &Edge{Outputs: []Target{oAbs}}, waits: []*Step{local}}
	targets[oBin] = targetInfo{step: local, edge: &Edge{Outputs: []Target{oBin}}}

	set := NewLocallyNeededSet()
	s := &scheduler{
		path:          &Path{WorkspaceRoot: "/w"},
		locallyNeeded: set,
		total:         2,
		plan:          &plan{targets: targets, targetsSorted: []Target{oAbs, oBin}},
	}
	g := classifyGraph{paths: map[Target]string{oAbs: absPath, oBin: "out/bin"}}

	s.classifyLocallyNeeded(ctx, g)

	// makeFullpath passes an absolute path through unchanged (not joined under
	// the workspace root), matching how NeedFlush queries the predicate.
	if !set.Has(absPath) {
		t.Errorf("absolute target path should be keyed as-is, not joined; set=%v", set.m)
	}
}

func TestScheduler_MultiOutputWeightPropagation(t *testing.T) {
	ctx := t.Context()
	const (
		tStamp Target = iota
		tMojomCC
		tMojomH
		tPhonyA
		tPhonyB
		tObj
		tBin
	)
	g := classifyGraph{paths: map[Target]string{
		tStamp:   "out/Default/gen/input.stamp",
		tMojomCC: "out/Default/gen/foo.mojom.cc",
		tMojomH:  "out/Default/gen/foo.mojom.h",
		tPhonyA:  "out/Default/phony_a",
		tPhonyB:  "out/Default/phony_b",
		tObj:     "out/Default/obj/bar.o",
		tBin:     "out/Default/chrome",
	}}
	sched := newScheduler(ctx, schedulerOption{
		NumTargets: 7,
		Path:       NewPath(t.TempDir(), "out/Default"),
		StepWeight: func(step *Step) int {
			if slices.Contains(step.outputs, tMojomH) {
				return 10
			}
			return 1
		},
	})

	stampStep := NewStepForTest(1, fakeStepDef{actionName: "stamp"})
	stampStep.outputs = []Target{tStamp}
	sched.plan.targets[tStamp].step = stampStep
	sched.plan.targets[tStamp].edge = &Edge{Outputs: []Target{tStamp}}

	// Multi-output step scheduled via tMojomCC, while StepWeight and
	// downstream consumers only reference sibling output tMojomH.
	mojomStep := NewStepForTest(2, fakeStepDef{actionName: "__mojom__rule"})
	mojomStep.outputs = []Target{tMojomCC, tMojomH}
	mojomStep.nwaits = 1
	sched.plan.targets[tStamp].waits = []*Step{mojomStep}
	sched.plan.targets[tMojomCC].step = mojomStep
	sched.plan.targets[tMojomCC].edge = &Edge{Inputs: []Target{tStamp}, Outputs: []Target{tMojomCC, tMojomH}}

	// Multi-output phony step scheduled via tPhonyA, consumed via sibling tPhonyB.
	phonyStep := NewStepForTest(3, classifyStepDef{phony: true})
	phonyStep.outputs = []Target{tPhonyA, tPhonyB}
	phonyStep.nwaits = 1
	sched.plan.targets[tMojomH].waits = []*Step{phonyStep}
	sched.plan.targets[tPhonyA].step = phonyStep
	sched.plan.targets[tPhonyA].edge = &Edge{Inputs: []Target{tMojomH}, Outputs: []Target{tPhonyA, tPhonyB}}

	objStep := NewStepForTest(4, fakeStepDef{actionName: "cxx"})
	objStep.outputs = []Target{tObj}
	objStep.nwaits = 1
	sched.plan.targets[tPhonyB].waits = []*Step{objStep}
	sched.plan.targets[tObj].step = objStep
	sched.plan.targets[tObj].edge = &Edge{Inputs: []Target{tPhonyB}, Outputs: []Target{tObj}}

	binStep := NewStepForTest(5, fakeStepDef{actionName: "link"})
	binStep.outputs = []Target{tBin}
	binStep.nwaits = 1
	sched.plan.targets[tObj].waits = []*Step{binStep}
	sched.plan.targets[tBin].step = binStep
	sched.plan.targets[tBin].edge = &Edge{Inputs: []Target{tObj}, Outputs: []Target{tBin}}

	sched.addStep(ctx, stampStep, g, tStamp)
	sched.addStep(ctx, mojomStep, g, tMojomCC)
	sched.addStep(ctx, phonyStep, g, tPhonyA)
	sched.addStep(ctx, objStep, g, tObj)
	sched.addStep(ctx, binStep, g, tBin)
	if err := sched.finish(ctx, time.Now(), g); err != nil {
		t.Fatalf("sched.finish() failed: %v", err)
	}

	// bin (1) + obj (1) + phony (0) + mojom (StepWeight[tMojomH]=10) + stamp (1).
	if binStep.weight != 1 {
		t.Errorf("binStep.weight = %d, want 1", binStep.weight)
	}
	if objStep.weight != 2 {
		t.Errorf("objStep.weight = %d, want 2", objStep.weight)
	}
	if phonyStep.weight != 2 {
		t.Errorf("phonyStep.weight = %d, want 2", phonyStep.weight)
	}
	if mojomStep.weight != 12 {
		t.Errorf("mojomStep.weight = %d, want 12", mojomStep.weight)
	}
	if stampStep.weight != 13 {
		t.Errorf("stampStep.weight = %d, want 13", stampStep.weight)
	}
}

func TestScheduler_StepWeight(t *testing.T) {
	ctx := t.Context()
	const (
		tSrc Target = iota
		tStamp
		tObj
		tPhony
		tBin
		tFailed
	)
	g := classifyGraph{paths: map[Target]string{
		tSrc:    "base/foo.cc",
		tStamp:  "out/Default/gen/input.stamp",
		tObj:    "out/Default/obj/foo.o",
		tPhony:  "out/Default/phony_deps",
		tBin:    "out/Default/chrome",
		tFailed: "out/Default/obj/failed.o",
	}}
	sched := newScheduler(ctx, schedulerOption{
		NumTargets: 6,
		Path:       NewPath(t.TempDir(), "out/Default"),
		StepWeight: func(step *Step) int {
			if slices.Contains(step.outputs, tFailed) {
				return math.MaxInt
			}
			switch step.def.ActionName() {
			case "link":
				return 15000
			case "cxx":
				return 7500
			case "stamp":
				// Non-positive weights must clamp to 1 on non-phony steps.
				return 0
			default:
				return 500
			}
		},
	})
	sched.plan.targets[tSrc].source = true

	stampStep := NewStepForTest(1, fakeStepDef{actionName: "stamp"})
	stampStep.outputs = []Target{tStamp}
	sched.plan.targets[tStamp].step = stampStep
	sched.plan.targets[tStamp].edge = &Edge{Outputs: []Target{tStamp}}

	objStep := NewStepForTest(2, fakeStepDef{actionName: "cxx"})
	objStep.outputs = []Target{tObj}
	objStep.nwaits = 1
	sched.plan.targets[tStamp].waits = []*Step{objStep}
	sched.plan.targets[tObj].step = objStep
	sched.plan.targets[tObj].edge = &Edge{Inputs: []Target{tSrc, tStamp}, Outputs: []Target{tObj}}

	phonyStep := NewStepForTest(3, classifyStepDef{phony: true})
	phonyStep.outputs = []Target{tPhony}
	phonyStep.nwaits = 1
	sched.plan.targets[tObj].waits = []*Step{phonyStep}
	sched.plan.targets[tPhony].step = phonyStep
	sched.plan.targets[tPhony].edge = &Edge{Inputs: []Target{tObj}, Outputs: []Target{tPhony}}

	binStep := NewStepForTest(4, fakeStepDef{actionName: "link"})
	binStep.outputs = []Target{tBin}
	binStep.nwaits = 1
	sched.plan.targets[tPhony].waits = []*Step{binStep}
	sched.plan.targets[tBin].step = binStep
	sched.plan.targets[tBin].edge = &Edge{Inputs: []Target{tPhony}, Outputs: []Target{tBin}}

	failedStep := NewStepForTest(5, fakeStepDef{actionName: "cxx"})
	failedStep.outputs = []Target{tFailed}
	sched.plan.targets[tFailed].step = failedStep
	sched.plan.targets[tFailed].edge = &Edge{Inputs: []Target{tSrc}, Outputs: []Target{tFailed}}

	sched.addStep(ctx, stampStep, g, tStamp)
	sched.addStep(ctx, objStep, g, tObj)
	sched.addStep(ctx, phonyStep, g, tPhony)
	sched.addStep(ctx, binStep, g, tBin)
	sched.addStep(ctx, failedStep, g, tFailed)
	if err := sched.finish(ctx, time.Now(), g); err != nil {
		t.Fatalf("sched.finish() failed: %v", err)
	}

	// link (15000) + phony (0, ignores StepWeight=500) + obj (7500) + stamp (0 clamped to 1).
	if binStep.weight != 15000 {
		t.Errorf("binStep.weight = %d, want 15000", binStep.weight)
	}
	if phonyStep.weight != 15000 {
		t.Errorf("phonyStep.weight = %d, want 15000", phonyStep.weight)
	}
	if objStep.weight != 22500 {
		t.Errorf("objStep.weight = %d, want 22500", objStep.weight)
	}
	if stampStep.weight != 22501 {
		t.Errorf("stampStep.weight = %d, want 22501", stampStep.weight)
	}
	if failedStep.weight != math.MaxInt {
		t.Errorf("failedStep.weight = %d, want math.MaxInt (%d)", failedStep.weight, math.MaxInt)
	}
}
