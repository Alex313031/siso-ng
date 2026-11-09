// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"fmt"
	"iter"
	"strings"
	"testing"
	"time"

	"go.chromium.org/build/hashigo/digest"

	"go.chromium.org/build/siso/execute"
	"go.chromium.org/build/siso/path"
)

type fakeStepDef struct {
	actionName     string
	command        string
	triggerInputs  []path.Path
	outputs        []path.Path
	pure           bool
	platform       map[string]string
	bindings       map[string]string
	expandedInputs func(context.Context) []path.Path
}

func (f fakeStepDef) String() string { return fmt.Sprintf("%#v", f) }
func (f fakeStepDef) Next() StepDef  { return nil }

func (f fakeStepDef) EnsureRule(context.Context) {}

func (f fakeStepDef) RuleName() string   { return "" }
func (f fakeStepDef) ActionName() string { return f.actionName }
func (f fakeStepDef) Args(context.Context) []string {
	return strings.Split(f.command, " ")
}
func (fakeStepDef) IsPhony() bool   { return false }
func (fakeStepDef) CmdHash() []byte { return []byte("<cmdhash>") }
func (f fakeStepDef) Binding(b string) string {
	if v, ok := f.bindings[b]; ok {
		return v
	}
	switch b {
	case "command":
		return f.command
	}
	return ""
}

func (f fakeStepDef) Depfile(context.Context) path.Path { return "" }
func (f fakeStepDef) Rspfile(context.Context) path.Path { return "" }

func (fakeStepDef) Inputs(context.Context) []path.Path                { return nil }
func (f fakeStepDef) TriggerInputs(context.Context) []path.Path       { return f.triggerInputs }
func (fakeStepDef) ExpandedTriggerInputs(context.Context) []path.Path { return nil }

func (fakeStepDef) DepInputs(context.Context) (iter.Seq[path.Path], error) {
	return func(yield func(path.Path) bool) {}, nil
}

func (fakeStepDef) DepsBaseInputs(ctx context.Context, inputs []path.Path, includeOrderOnly bool) []path.Path {
	return inputs
}

func (fakeStepDef) ToolInputs(context.Context) []path.Path { return nil }
func (fakeStepDef) ExpandedCaseSensitives(ctx context.Context, in []path.Path) []path.Path {
	return in
}
func (f fakeStepDef) ExpandedInputs(ctx context.Context) []path.Path {
	if f.expandedInputs == nil {
		return nil
	}
	return f.expandedInputs(ctx)
}

func (fakeStepDef) RemoteInputs() map[path.Path]path.Path { return nil }

func (fakeStepDef) CheckMissingDeps(context.Context, []path.Path) (bool, error) {
	return false, nil
}

func (fakeStepDef) Handle(context.Context, *execute.Cmd) error { return nil }

func (f fakeStepDef) Outputs(context.Context) []path.Path {
	return f.outputs
}

func (fakeStepDef) AuxiliaryLogOutputFiles(context.Context) []path.Path { return nil }
func (fakeStepDef) AuxiliaryLogOutputDirs(context.Context) []path.Path  { return nil }

func (fakeStepDef) LocalOutputs(context.Context) []path.Path { return nil }
func (f fakeStepDef) Pure() bool                             { return f.pure }
func (f fakeStepDef) Platform() map[string]string            { return f.platform }
func (fakeStepDef) IsRemoteRule() bool                       { return false }
func (fakeStepDef) Sandbox() map[string]string               { return nil }
func (fakeStepDef) RecordDeps(context.Context, string, time.Time, digest.Digest, []string) (bool, error) {
	return false, nil
}
func (fakeStepDef) RuleFix(context.Context, []string, []string) []byte { return nil }

func TestStepSpanName(t *testing.T) {
	for _, tc := range []struct {
		name    string
		stepDef fakeStepDef
		want    string
	}{
		{
			name: "cxx",
			stepDef: fakeStepDef{
				actionName: "cxx",
			},
			want: "cxx",
		},
		{
			name: "irt_x64_cxx",
			stepDef: fakeStepDef{
				actionName: "irt_x64_cxx",
			},
			want: "irt_x64_cxx",
		},
		{
			name: "mojo_generate_type_mappings.py",
			stepDef: fakeStepDef{
				actionName: "__content_browser_attribution_reporting_store_source_result_mojom_blink__type_mappings___build_toolchain_win_win_clang_x64__rule",
				command:    "python3 ../../mojo/public/tools/bindings/generate_type_mappings.py --output gen/content/browser/attribution_reporting/store_source_result_mojom_blink__type_mappings",
			},
			want: "../../mojo/public/tools/bindings/generate_type_mappings.py",
		},
		{
			name: "win_mojo_generate_type_mappings.py",
			stepDef: fakeStepDef{
				actionName: "__content_browser_attribution_reporting_store_source_result_mojom_blink__type_mappings___build_toolchain_win_win_clang_x64__rule",
				command:    "C:/b/s/w/ir/cipd_bin_packages/cpython3/bin/python3.exe ../../mojo/public/tools/bindings/generate_type_mappings.py --output gen/content/browser/attribution_reporting/store_source_result_mojom_blink__type_mappings",
			},
			want: "../../mojo/public/tools/bindings/generate_type_mappings.py",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := stepSpanName(tc.stepDef)
			if got != tc.want {
				t.Errorf("stepSpanName(stepDef)=%q; want=%q", got, tc.want)
			}
		})
	}
}

func TestStepRestatContent(t *testing.T) {
	for _, tc := range []struct {
		name          string
		experiment    bool
		restat        bool
		restatContent string
		want          bool
	}{
		{
			name:   "experiment_off_restat",
			restat: true,
			want:   false,
		},
		{
			name:       "experiment_on_restat",
			experiment: true,
			restat:     true,
			want:       true,
		},
		{
			name:       "experiment_on_without_restat",
			experiment: true,
			want:       false,
		},
		{
			name:          "experiment_on_explicit_restat_content_false",
			experiment:    true,
			restat:        true,
			restatContent: "false",
			want:          false,
		},
		{
			name:          "experiment_off_explicit_restat_content_true",
			restatContent: "true",
			want:          true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.experiment {
				SetExperimentForTest("auto-restat-content")
			} else {
				SetExperimentForTest("")
			}
			t.Cleanup(func() { SetExperimentForTest("") })
			bindings := map[string]string{}
			if tc.restat {
				bindings["restat"] = "true"
			}
			if tc.restatContent != "" {
				bindings["restat_content"] = tc.restatContent
			}
			stepDef := fakeStepDef{bindings: bindings}
			if got := stepRestatContent(stepDef); got != tc.want {
				t.Errorf("stepRestatContent()=%t; want %t", got, tc.want)
			}
		})
	}
}

func TestStepMetrics(t *testing.T) {
	m := StepMetric{
		BuildID: "my-build-id",
		StepID:  "my-step-id",
		Utime:   IntervalMetric(5 * time.Second),
		Stime:   IntervalMetric(2 * time.Second),
		Majflt:  42,
	}

	s := NewStepForTest(1, fakeStepDef{})
	s.SetMetricsForTest(m)

	got := s.Metrics()
	if got.BuildID != m.BuildID || got.StepID != m.StepID || got.Utime != m.Utime || got.Stime != m.Stime || got.Majflt != m.Majflt {
		t.Errorf("s.Metrics()=%+v; want=%+v", got, m)
	}
}

func TestStepReadyToRunRecordsLastTwoInputs(t *testing.T) {
	for _, tc := range []struct {
		name     string
		releases []string // releasing step per wait, in completion order
		wantPrev string
		want2    string
	}{
		{
			name:     "NoInputs",
			releases: nil,
		},
		{
			name:     "SingleInput",
			releases: []string{"a"},
			wantPrev: "a",
		},
		{
			name:     "ThreeInputs",
			releases: []string{"a", "b", "c"},
			wantPrev: "c",
			want2:    "b",
		},
		{
			name:     "SameStepReleasesLastTwoWaits",
			releases: []string{"a", "b", "b"},
			wantPrev: "b",
			want2:    "a",
		},
		{
			// A release forwarded from a skipped root has no step ID; it
			// still counts as an input, so prev2 reports it as unknown.
			name:     "ForwardedRootRelease",
			releases: []string{"a", "", "b"},
			wantPrev: "b",
			want2:    "",
		},
		{
			name:     "ForwardedRootReleaseLast",
			releases: []string{"a", ""},
			wantPrev: "",
			want2:    "a",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewStepForTest(1, fakeStepDef{})
			s.nwaits = len(tc.releases)
			if len(tc.releases) == 0 {
				if !s.ReadyToRun("", Target(0)) {
					t.Fatal("ReadyToRun with no waits = false; want true")
				}
			}
			for i, prev := range tc.releases {
				got := s.ReadyToRun(prev, Target(i+1))
				if want := i == len(tc.releases)-1; got != want {
					t.Fatalf("ReadyToRun(%q) #%d = %t; want %t", prev, i, got, want)
				}
			}
			if s.prevStepIDs != [2]string{tc.wantPrev, tc.want2} {
				t.Errorf("prevStepIDs=%q; want [%q %q]", s.prevStepIDs, tc.wantPrev, tc.want2)
			}
		})
	}
}
