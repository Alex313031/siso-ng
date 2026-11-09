// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sync"
)

const (
	// maxWeightsBytes caps the uncompressed size of a weights JSON file (64 MiB).
	maxWeightsBytes = 64 << 20

	// maxStepWeight caps an individual step weight below math.MaxInt (reserved
	// for prioritizing targets that failed in the previous build).
	maxStepWeight = 1 << 30
)

// weightsFileJSON defines the JSON schema for -weights_file.
// Weights are unitless positive integers representing relative step costs
// (for example, estimated execution durations in milliseconds so sub-second
// steps remain distinct from multi-second steps without rounding up to 1).
type weightsFileJSON struct {
	// Outputs maps canonical build-dir-relative output paths (e.g.
	// "obj/base/logging.o") to predicted positive step weights.
	Outputs map[string]int `json:"outputs"`

	// Rules lists fallback weights matched in order against the Ninja rule/action
	// name (for example, toolchain-prefixed or generated per-target rule names)
	// for steps whose outputs do not appear in Outputs.
	Rules []weightRuleJSON `json:"rules"`

	// Default is the optional positive fallback weight for steps not matched by
	// Outputs or Rules. When neither Rules nor Default is set, unmatched steps
	// default to weight 1.
	Default *int `json:"default"`
}

// weightRuleJSON maps a Ninja rule/action name pattern to a fallback weight.
type weightRuleJSON struct {
	// Action is an RE2 regular expression anchored to the full Ninja rule/action
	// name (compiled as ^(?:<action>)$).
	Action string `json:"action"`

	// Weight is the positive integer fallback weight assigned to matching steps.
	Weight int `json:"weight"`
}

type compiledWeightRule struct {
	actionRE *regexp.Regexp
	weight   int
}

// stepWeights holds predicted per-output step weights along with optional
// action-regex and default fallback weights loaded from a weights JSON file.
type stepWeights struct {
	outputs       map[string]int
	rules         []compiledWeightRule
	defaultWeight int
}

// loadStepWeights reads predicted per-output step weights and optional
// action/default fallback weights from a JSON or gzip-compressed JSON (.gz)
// file. Specified weights must be positive integers and are clamped to
// [1, maxStepWeight].
func loadStepWeights(filename string) (*stepWeights, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var r io.Reader = f
	if filepath.Ext(filename) == ".gz" {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return nil, fmt.Errorf("creating gzip reader for %q: %w", filename, err)
		}
		defer gz.Close()
		r = gz
	}

	lr := &io.LimitedReader{R: r, N: maxWeightsBytes + 1}
	data, err := io.ReadAll(lr)
	if err != nil {
		return nil, fmt.Errorf("reading weights file %q: %w", filename, err)
	}
	if len(data) > maxWeightsBytes {
		return nil, fmt.Errorf("weights file %q exceeds %d bytes", filename, maxWeightsBytes)
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var raw weightsFileJSON
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("unmarshaling weights file %q: %w", filename, err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("unexpected trailing data in weights file %q", filename)
	}

	outputs := make(map[string]int, len(raw.Outputs))
	for k, w := range raw.Outputs {
		if k == "" {
			return nil, fmt.Errorf("weights file %q: empty output path", filename)
		}
		if w <= 0 {
			return nil, fmt.Errorf("weights file %q: output %q has non-positive weight %d", filename, k, w)
		}
		outputs[k] = min(w, maxStepWeight)
	}

	rules := make([]compiledWeightRule, 0, len(raw.Rules))
	for i, rule := range raw.Rules {
		if rule.Action == "" {
			return nil, fmt.Errorf("weights file %q: rule[%d] has empty action", filename, i)
		}
		if rule.Weight <= 0 {
			return nil, fmt.Errorf("weights file %q: rule[%d] action %q has non-positive weight %d", filename, i, rule.Action, rule.Weight)
		}
		re, err := regexp.Compile("^(?:" + rule.Action + ")$")
		if err != nil {
			return nil, fmt.Errorf("weights file %q: compiling rule[%d] action %q: %w", filename, i, rule.Action, err)
		}
		rules = append(rules, compiledWeightRule{
			actionRE: re,
			weight:   min(rule.Weight, maxStepWeight),
		})
	}

	var defaultWeight int
	if raw.Default != nil {
		if *raw.Default <= 0 {
			return nil, fmt.Errorf("weights file %q: non-positive default weight %d", filename, *raw.Default)
		}
		defaultWeight = min(*raw.Default, maxStepWeight)
	}

	return &stepWeights{
		outputs:       outputs,
		rules:         rules,
		defaultWeight: defaultWeight,
	}, nil
}

// actionWeightFn returns a concurrency-safe fallback function mapping Ninja
// rule/action names to step weights when rules or defaultWeight are configured,
// or nil when neither is set. Matched weights are memoized per action name so
// each distinct Ninja rule evaluates regexes at most once.
func (sw *stepWeights) actionWeightFn() func(string) int {
	if sw == nil || (len(sw.rules) == 0 && sw.defaultWeight <= 0) {
		return nil
	}
	fallback := max(sw.defaultWeight, 1)
	rules := sw.rules
	if len(rules) == 0 {
		return func(string) int {
			return fallback
		}
	}
	var mu sync.RWMutex
	cache := make(map[string]int)
	return func(action string) int {
		mu.RLock()
		w, ok := cache[action]
		mu.RUnlock()
		if ok {
			return w
		}
		w = fallback
		for _, r := range rules {
			if r.actionRE.MatchString(action) {
				w = r.weight
				break
			}
		}
		mu.Lock()
		cache[action] = w
		mu.Unlock()
		return w
	}
}

// newStepWeightFn builds a step-weight lookup function combining failed targets
// from the previous build (math.MaxInt) with predicted per-output, action-regex,
// and default fallback weights from sw. It returns nil when neither failed
// targets nor sw provides any weights, along with the number of sw.outputs
// entries matched against graph.
func newStepWeightFn(graph Graph, sw *stepWeights, failedTargets []Target) (func(*Step) int, int) {
	if sw == nil && len(failedTargets) == 0 {
		return nil, 0
	}
	knownTargets := make(map[Target]int, len(failedTargets))
	var matched int
	if sw != nil && graph != nil {
		for out, w := range sw.outputs {
			t, ok := graph.LookupTarget(out)
			if !ok {
				continue
			}
			if prev, exists := knownTargets[t]; !exists || w > prev {
				knownTargets[t] = w
			}
			matched++
		}
	}
	for _, t := range failedTargets {
		knownTargets[t] = math.MaxInt
	}
	actionFn := sw.actionWeightFn()
	if len(knownTargets) == 0 && actionFn == nil {
		return nil, matched
	}
	return func(step *Step) int {
		var w int
		for _, out := range step.outputs {
			if tw, ok := knownTargets[out]; ok {
				w = max(w, tw)
			}
		}
		if w > 0 {
			return w
		}
		if actionFn != nil && step.def != nil {
			return actionFn(step.def.ActionName())
		}
		return 1
	}, matched
}
