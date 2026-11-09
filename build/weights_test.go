// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"bytes"
	"compress/gzip"
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

type lookupTargetGraph struct {
	fakeGraph
	paths  map[Target]string
	byPath map[string]Target
}

func (g lookupTargetGraph) LookupTarget(p string) (Target, bool) {
	t, ok := g.byPath[p]
	return t, ok
}

func (g lookupTargetGraph) TargetPath(ctx context.Context, t Target) (string, error) {
	if p, ok := g.paths[t]; ok {
		return p, nil
	}
	return g.fakeGraph.TargetPath(ctx, t)
}

func writeGzipFile(t *testing.T, filename string, content []byte) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(content); err != nil {
		t.Fatalf("gzip.Write(%q) failed: %v", filename, err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip.Close(%q) failed: %v", filename, err)
	}
	if err := os.MkdirAll(filepath.Dir(filename), 0755); err != nil {
		t.Fatalf("os.MkdirAll(%q) failed: %v", filename, err)
	}
	if err := os.WriteFile(filename, buf.Bytes(), 0644); err != nil {
		t.Fatalf("os.WriteFile(%q) failed: %v", filename, err)
	}
}

func TestLoadStepWeights(t *testing.T) {
	dir := t.TempDir()
	rawJSON := []byte(`{
		"outputs": {
			"obj/base/logging.o": 4200,
			"obj/base/vlog.o": 3100,
			"huge_link": 2000000000
		},
		"rules": [
			{"action": "(?:.+_)?link", "weight": 15000},
			{"action": "(?:.+_)?cxx", "weight": 6000},
			{"action": "action|__.*", "weight": 1000},
			{"action": "huge_rule", "weight": 2000000000}
		],
		"default": 500
	}`)

	plainFile := filepath.Join(dir, "weights.json")
	if err := os.WriteFile(plainFile, rawJSON, 0644); err != nil {
		t.Fatalf("os.WriteFile(%q) failed: %v", plainFile, err)
	}
	gzipFile := filepath.Join(dir, "weights.json.gz")
	writeGzipFile(t, gzipFile, rawJSON)

	wantOutputs := map[string]int{
		"obj/base/logging.o": 4200,
		"obj/base/vlog.o":    3100,
		"huge_link":          maxStepWeight,
	}

	for _, tc := range []struct {
		name string
		path string
	}{
		{name: "plain_json", path: plainFile},
		{name: "gzipped_json", path: gzipFile},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := loadStepWeights(tc.path)
			if err != nil {
				t.Fatalf("loadStepWeights(%q) returned unexpected error: %v", tc.path, err)
			}
			if diff := cmp.Diff(wantOutputs, got.outputs); diff != "" {
				t.Errorf("loadStepWeights(%q) outputs diff (-want +got):\n%s", tc.path, diff)
			}
			if got.defaultWeight != 500 {
				t.Errorf("loadStepWeights(%q) defaultWeight = %d, want 500", tc.path, got.defaultWeight)
			}
			fn := got.actionWeightFn()
			if fn == nil {
				t.Fatalf("loadStepWeights(%q) actionWeightFn() = nil, want non-nil", tc.path)
			}
			for _, ac := range []struct {
				name   string
				action string
				want   int
			}{
				{name: "link", action: "link", want: 15000},
				{name: "toolchain_link", action: "win_clang_x64_link", want: 15000},
				{name: "unmatched_alink_falls_through", action: "alink", want: 500},
				{name: "cxx", action: "cxx", want: 6000},
				{name: "toolchain_cxx", action: "irt_x64_cxx", want: 6000},
				{name: "gn_rule", action: "__chrome_browser_mojom__rule", want: 1000},
				{name: "alternation_is_fully_anchored_left", action: "action_not_matched", want: 500},
				{name: "alternation_is_fully_anchored_right", action: "mid__not_matched", want: 500},
				{name: "clamped_rule", action: "huge_rule", want: maxStepWeight},
				{name: "default_fallback", action: "custom_other", want: 500},
			} {
				t.Run(ac.name, func(t *testing.T) {
					if w := fn(ac.action); w != ac.want {
						t.Errorf("actionWeightFn(%q) = %d, want %d", ac.action, w, ac.want)
					}
				})
			}
		})
	}
}

func TestStepWeights_ActionWeightFn_OptionalFallbacks(t *testing.T) {
	dir := t.TempDir()

	outputsOnly := filepath.Join(dir, "outputs_only.json")
	if err := os.WriteFile(outputsOnly, []byte(`{"outputs":{"obj/a.o":1200}}`), 0644); err != nil {
		t.Fatalf("os.WriteFile(%q) failed: %v", outputsOnly, err)
	}
	sw, err := loadStepWeights(outputsOnly)
	if err != nil {
		t.Fatalf("loadStepWeights(%q) failed: %v", outputsOnly, err)
	}
	if fn := sw.actionWeightFn(); fn != nil {
		t.Errorf("outputs-only actionWeightFn() = non-nil, want nil")
	}

	rulesWithoutDefault := filepath.Join(dir, "rules_only.json")
	if err := os.WriteFile(rulesWithoutDefault, []byte(`{"rules":[{"action":"link","weight":9000}]}`), 0644); err != nil {
		t.Fatalf("os.WriteFile(%q) failed: %v", rulesWithoutDefault, err)
	}
	swRules, err := loadStepWeights(rulesWithoutDefault)
	if err != nil {
		t.Fatalf("loadStepWeights(%q) failed: %v", rulesWithoutDefault, err)
	}
	fn := swRules.actionWeightFn()
	if fn == nil {
		t.Fatalf("rules-only actionWeightFn() = nil, want non-nil")
	}
	if got := fn("link"); got != 9000 {
		t.Errorf("fn(\"link\") = %d, want 9000", got)
	}
	if got := fn("unmatched"); got != 1 {
		t.Errorf("fn(\"unmatched\") without default = %d, want 1", got)
	}

	defaultOnly := filepath.Join(dir, "default_only.json")
	if err := os.WriteFile(defaultOnly, []byte(`{"default":750}`), 0644); err != nil {
		t.Fatalf("os.WriteFile(%q) failed: %v", defaultOnly, err)
	}
	swDefault, err := loadStepWeights(defaultOnly)
	if err != nil {
		t.Fatalf("loadStepWeights(%q) failed: %v", defaultOnly, err)
	}
	fnDefault := swDefault.actionWeightFn()
	if fnDefault == nil {
		t.Fatalf("default-only actionWeightFn() = nil, want non-nil")
	}
	if got := fnDefault("cxx"); got != 750 {
		t.Errorf("fnDefault(\"cxx\") = %d, want 750", got)
	}

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 50 {
				if got := fn("link"); got != 9000 {
					t.Errorf("concurrent fn(%q) = %d, want 9000", "link", got)
					return
				}
				if got := fn("unmatched"); got != 1 {
					t.Errorf("concurrent fn(%q) = %d, want 1", "unmatched", got)
					return
				}
			}
		})
	}
	wg.Wait()
}

func TestLoadStepWeights_Errors(t *testing.T) {
	dir := t.TempDir()

	writeCase := func(name, content string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0644); err != nil {
			t.Fatalf("os.WriteFile(%q) failed: %v", p, err)
		}
		return p
	}

	// Highly compressible payload whose uncompressed length exceeds maxWeightsBytes.
	oversizedGz := filepath.Join(dir, "oversized.json.gz")
	oversizedPayload := `{"outputs":{"` + strings.Repeat("x", maxWeightsBytes) + `":1}}`
	writeGzipFile(t, oversizedGz, []byte(oversizedPayload))

	for _, tc := range []struct {
		name string
		path string
	}{
		{name: "missing_file", path: filepath.Join(dir, "does_not_exist.json.gz")},
		{name: "malformed_json", path: writeCase("invalid.json", `{"outputs":`)},
		{name: "corrupt_gzip_with_gz_suffix", path: writeCase("corrupt.json.gz", `{"outputs":{"plain_not_gzip":100}}`)},
		{name: "unknown_top_level_field", path: writeCase("flat_map.json", `{"obj/base/logging.o":4200}`)},
		{name: "trailing_json_data", path: writeCase("trailing.json", `{"outputs":{}} {"extra":1}`)},
		{name: "empty_output_path", path: writeCase("empty_output.json", `{"outputs":{"":100}}`)},
		{name: "zero_output_weight", path: writeCase("zero_output.json", `{"outputs":{"obj/zero.o":0}}`)},
		{name: "negative_output_weight", path: writeCase("neg_output.json", `{"outputs":{"obj/neg.o":-15}}`)},
		{name: "empty_rule_action", path: writeCase("empty_action.json", `{"rules":[{"action":"","weight":1000}]}`)},
		{name: "zero_rule_weight", path: writeCase("zero_rule.json", `{"rules":[{"action":"cxx","weight":0}]}`)},
		{name: "negative_rule_weight", path: writeCase("neg_rule.json", `{"rules":[{"action":"cxx","weight":-5}]}`)},
		{name: "invalid_rule_regex", path: writeCase("bad_regex.json", `{"rules":[{"action":"[unclosed","weight":1000}]}`)},
		{name: "zero_default_weight", path: writeCase("zero_default.json", `{"default":0}`)},
		{name: "negative_default_weight", path: writeCase("neg_default.json", `{"default":-10}`)},
		{name: "exceeds_max_uncompressed_bytes", path: oversizedGz},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := loadStepWeights(tc.path)
			if err == nil {
				t.Errorf("loadStepWeights(%q) = (%v, nil), want non-nil error", tc.path, got)
			}
		})
	}
}

func TestNewStepWeightFn(t *testing.T) {
	const (
		tFoo Target = iota + 1
		tBar
		tFail
		tUnmatched
	)
	g := lookupTargetGraph{
		byPath: map[string]Target{
			"obj/base/foo.o":  tFoo,
			"gen/bar.h":       tBar,
			"obj/base/fail.o": tFail,
		},
	}

	if fn, matched := newStepWeightFn(g, nil, nil); fn != nil || matched != 0 {
		t.Errorf("newStepWeightFn(g, nil, nil) = (non-nil=%t, %d), want (nil, 0)", fn != nil, matched)
	}

	unmatchedOnly := &stepWeights{
		outputs: map[string]int{"obj/missing/other.o": 3000},
	}
	if fn, matched := newStepWeightFn(g, unmatchedOnly, nil); fn != nil || matched != 0 {
		t.Errorf("newStepWeightFn(g, unmatchedOnly, nil) = (non-nil=%t, %d), want (nil, 0)", fn != nil, matched)
	}

	weightsFile := filepath.Join(t.TempDir(), "weights.json")
	rawJSON := []byte(`{
		"outputs": {
			"obj/base/foo.o": 4500,
			"gen/bar.h": 1200,
			"out/Default/gen/bar.h": 9999,
			"obj/base/fail.o": 6000,
			"obj/missing/other.o": 3000
		},
		"rules": [
			{"action": "(?:.+_)?cxx", "weight": 6000}
		],
		"default": 500
	}`)
	if err := os.WriteFile(weightsFile, rawJSON, 0644); err != nil {
		t.Fatalf("os.WriteFile(%q) failed: %v", weightsFile, err)
	}
	sw, err := loadStepWeights(weightsFile)
	if err != nil {
		t.Fatalf("loadStepWeights(%q) failed: %v", weightsFile, err)
	}

	fn, matched := newStepWeightFn(g, sw, []Target{tFail})
	if matched != 3 {
		t.Errorf("newStepWeightFn matched = %d, want 3", matched)
	}
	if fn == nil {
		t.Fatalf("newStepWeightFn returned nil, want non-nil")
	}

	for _, tc := range []struct {
		name    string
		action  string
		outputs []Target
		want    int
	}{
		{name: "single_matched_output", action: "cxx", outputs: []Target{tFoo}, want: 4500},
		{name: "multi_output_sibling_match", action: "action", outputs: []Target{tUnmatched, tBar}, want: 1200},
		{name: "multi_output_takes_max", action: "action", outputs: []Target{tBar, tFoo}, want: 4500},
		{name: "failed_target_overrides_output_weight", action: "cxx", outputs: []Target{tFoo, tFail}, want: math.MaxInt},
		{name: "unmatched_output_uses_rule_fallback", action: "irt_x64_cxx", outputs: []Target{tUnmatched}, want: 6000},
		{name: "unmatched_output_uses_default_fallback", action: "stamp", outputs: []Target{tUnmatched}, want: 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			step := NewStepForTest(1, fakeStepDef{actionName: tc.action})
			step.outputs = tc.outputs
			if got := fn(step); got != tc.want {
				t.Errorf("stepWeightFn(action=%q, outputs=%v) = %d, want %d", tc.action, tc.outputs, got, tc.want)
			}
		})
	}
}

func TestScheduler_LoadedStepWeights(t *testing.T) {
	ctx := t.Context()
	const (
		tSrc Target = iota
		tStamp
		tGen
		tObj
		tPhony
		tBin
		tFailed
	)
	g := lookupTargetGraph{
		paths: map[Target]string{
			tSrc:    "base/foo.cc",
			tStamp:  "out/Default/gen/input.stamp",
			tGen:    "out/Default/gen/foo.h",
			tObj:    "out/Default/obj/foo.o",
			tPhony:  "out/Default/phony_deps",
			tBin:    "out/Default/chrome",
			tFailed: "out/Default/obj/failed.o",
		},
		byPath: map[string]Target{
			"gen/input.stamp": tStamp,
			"gen/foo.h":       tGen,
			"obj/foo.o":       tObj,
			"phony_deps":      tPhony,
			"chrome":          tBin,
			"obj/failed.o":    tFailed,
		},
	}
	dir := t.TempDir()
	weightsFile := filepath.Join(dir, "weights.json.gz")
	writeGzipFile(t, weightsFile, []byte(`{
		"outputs": {
			"gen/input.stamp": 1,
			"obj/foo.o": 7500,
			"obj/failed.o": 6000
		},
		"rules": [
			{"action": "link", "weight": 15000},
			{"action": "cxx", "weight": 6000},
			{"action": "__.*__rule", "weight": 1000},
			{"action": "stamp", "weight": 50}
		],
		"default": 500
	}`))
	sw, err := loadStepWeights(weightsFile)
	if err != nil {
		t.Fatalf("loadStepWeights(%q) failed: %v", weightsFile, err)
	}
	stepWeightFn, _ := newStepWeightFn(g, sw, []Target{tFailed})
	sched := newScheduler(ctx, schedulerOption{
		NumTargets: 7,
		Path:       NewPath(dir, "out/Default"),
		StepWeight: stepWeightFn,
	})
	sched.plan.targets[tSrc].source = true

	stampStep := NewStepForTest(1, fakeStepDef{actionName: "stamp"})
	stampStep.outputs = []Target{tStamp}
	sched.plan.targets[tStamp].step = stampStep
	sched.plan.targets[tStamp].edge = &Edge{Outputs: []Target{tStamp}}

	genStep := NewStepForTest(2, fakeStepDef{actionName: "__gen__rule"})
	genStep.outputs = []Target{tGen}
	genStep.nwaits = 1
	sched.plan.targets[tStamp].waits = []*Step{genStep}
	sched.plan.targets[tGen].step = genStep
	sched.plan.targets[tGen].edge = &Edge{Inputs: []Target{tStamp}, Outputs: []Target{tGen}}

	objStep := NewStepForTest(3, fakeStepDef{actionName: "cxx"})
	objStep.outputs = []Target{tObj}
	objStep.nwaits = 1
	sched.plan.targets[tGen].waits = []*Step{objStep}
	sched.plan.targets[tObj].step = objStep
	sched.plan.targets[tObj].edge = &Edge{Inputs: []Target{tSrc, tGen}, Outputs: []Target{tObj}}

	phonyStep := NewStepForTest(4, classifyStepDef{phony: true})
	phonyStep.outputs = []Target{tPhony}
	phonyStep.nwaits = 1
	sched.plan.targets[tObj].waits = []*Step{phonyStep}
	sched.plan.targets[tPhony].step = phonyStep
	sched.plan.targets[tPhony].edge = &Edge{Inputs: []Target{tObj}, Outputs: []Target{tPhony}}

	binStep := NewStepForTest(5, fakeStepDef{actionName: "link"})
	binStep.outputs = []Target{tBin}
	binStep.nwaits = 1
	sched.plan.targets[tPhony].waits = []*Step{binStep}
	sched.plan.targets[tBin].step = binStep
	sched.plan.targets[tBin].edge = &Edge{Inputs: []Target{tPhony}, Outputs: []Target{tBin}}

	failedStep := NewStepForTest(6, fakeStepDef{actionName: "cxx"})
	failedStep.outputs = []Target{tFailed}
	sched.plan.targets[tFailed].step = failedStep
	sched.plan.targets[tFailed].edge = &Edge{Inputs: []Target{tSrc}, Outputs: []Target{tFailed}}

	sched.addStep(ctx, stampStep, g, tStamp)
	sched.addStep(ctx, genStep, g, tGen)
	sched.addStep(ctx, objStep, g, tObj)
	sched.addStep(ctx, phonyStep, g, tPhony)
	sched.addStep(ctx, binStep, g, tBin)
	sched.addStep(ctx, failedStep, g, tFailed)
	if err := sched.finish(ctx, time.Now(), g); err != nil {
		t.Fatalf("sched.finish() failed: %v", err)
	}

	// link fallback (15000) -> phony (0) -> obj output weight (7500) -> __gen__rule fallback (1000) -> weight=1 output stamp (1).
	if binStep.weight != 15000 {
		t.Errorf("binStep.weight = %d, want 15000", binStep.weight)
	}
	if phonyStep.weight != 15000 {
		t.Errorf("phonyStep.weight = %d, want 15000", phonyStep.weight)
	}
	if objStep.weight != 22500 {
		t.Errorf("objStep.weight = %d, want 22500", objStep.weight)
	}
	if genStep.weight != 23500 {
		t.Errorf("genStep.weight = %d, want 23500", genStep.weight)
	}
	if stampStep.weight != 23501 {
		t.Errorf("stampStep.weight = %d, want 23501", stampStep.weight)
	}
	if failedStep.weight != math.MaxInt {
		t.Errorf("failedStep.weight = %d, want math.MaxInt (%d)", failedStep.weight, math.MaxInt)
	}
}
