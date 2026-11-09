// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"go.chromium.org/build/siso/execute"
	"go.chromium.org/build/siso/o11y/trace"
	"go.chromium.org/build/siso/path"
)

// Arbitrary fixed build start time.
var buildStart = time.Date(2024, time.July, 22, 0, 0, 0, 0, time.UTC)

// testSpan returns a span starting at offset after [buildStart].
func testSpan(name string, offset, dur time.Duration) trace.SpanData {
	start := buildStart.Add(offset)
	return trace.SpanData{
		Name:  name,
		Start: start,
		End:   start.Add(dur),
	}
}

func TestStepMetricsDone_NoExecutionMetadata(t *testing.T) {
	ctx := t.Context()
	step := &Step{
		state: &stepState{},
		cmd:   &execute.Cmd{},
	}
	var m StepMetric
	m.done(ctx, nil, step, time.Now())
	t.Logf("m.done passed without panic")
}

func TestStepMetricsDone_OutputsAndOutputDirs(t *testing.T) {
	ctx := t.Context()
	b := &Builder{
		path: NewPath("/src", "out/Release"),
	}
	step := &Step{
		state: &stepState{},
		cmd: &execute.Cmd{
			Outputs:    []path.Path{path.New("out/Release/obj/foo.o"), path.New("out/Release/gen/extra.h")},
			OutputDirs: []path.Path{path.New("out/Release/gen/outdir")},
		},
	}
	var m StepMetric
	m.done(ctx, b, step, time.Now())

	wantOutputs := []string{"obj/foo.o", "gen/extra.h", "gen/outdir/"}
	if diff := cmp.Diff(wantOutputs, m.Outputs); diff != "" {
		t.Errorf("m.Outputs mismatch (-want +got):\n%s", diff)
	}
}

func TestStepMetricsJSON_Spans(t *testing.T) {
	m := StepMetric{
		Rule: "test_rule",
		Spans: []MetricSpan{
			{
				Name:          "step:cxx",
				StartNanos:    100,
				DurationNanos: 500,
			},
		},
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("json.Marshal(m) failed: %v", err)
	}
	got := string(b)
	want := `"spans":[{"name":"step:cxx","start_ns":100,"duration_ns":500}]`
	if !strings.Contains(got, want) {
		t.Errorf("json.Marshal(m) = %s; want to contain %s", got, want)
	}
}

func TestUpdateStepMetricsFromTrace_CanonicalRunTime(t *testing.T) {
	for _, tc := range []struct {
		name    string
		metrics StepMetric
		spans   []trace.SpanData
		want    IntervalMetric
	}{
		{
			name:    "handler",
			metrics: StepMetric{NoExec: true},
			spans: []trace.SpanData{
				testSpan(spanHandleStepRun, time.Second, 5*time.Second),
			},
			want: IntervalMetric(5 * time.Second),
		},
		{
			name:    "local",
			metrics: StepMetric{IsLocal: true},
			spans: []trace.SpanData{
				testSpan(spanExecLocalRun, time.Second, 7*time.Second),
			},
			want: IntervalMetric(7 * time.Second),
		},
		{
			name:    "remote_exec",
			metrics: StepMetric{IsRemote: true},
			spans: []trace.SpanData{
				// If there was multiple attempts, the last one is the winner.
				testSpan(spanExecRemoteExecAttempt, time.Second, 2*time.Second),
				testSpan(spanExecRemoteExecAttempt, 2*time.Second, 9*time.Second),
				testSpan(spanExecRemoteExecPostProc, 11*time.Second, 3*time.Second),
			},
			want: IntervalMetric(12 * time.Second),
		},
		{
			name:    "remote_exec_attempt_only",
			metrics: StepMetric{IsRemote: true},
			spans: []trace.SpanData{
				testSpan(spanExecRemoteExecAttempt, time.Second, 9*time.Second),
			},
			want: IntervalMetric(9 * time.Second),
		},
		{
			name:    "cache_hit",
			metrics: StepMetric{IsRemote: false, Cached: true},
			spans: []trace.SpanData{
				testSpan(spanExecRemoteCacheCheck, time.Second, 1500*time.Millisecond),
				testSpan(spanExecRemoteCacheRun, time.Second, 2*time.Second),
			},
			want: IntervalMetric(2 * time.Second),
		},
		{
			// On the chance that [Builder.execRemoteCache] misses and we fall
			// through to remoteexec, it is possible for RBE to return a cached
			// result. In this case, IsRemote and Cached are both true.
			// No spanExecRemoteCacheRun is logged on that path, so
			// checking Cached first would report a zero RunTime.
			name:    "RBE_cache_hit",
			metrics: StepMetric{IsRemote: true, Cached: true},
			spans: []trace.SpanData{
				testSpan(spanExecRemoteExecAttempt, time.Second, 3*time.Second),
			},
			want: IntervalMetric(3 * time.Second),
		},
		{
			// Local fallback leaves IsRemote set from the failed remote attempt.
			name:    "local_fallback_after_remote",
			metrics: StepMetric{IsRemote: true, IsLocal: true, Fallback: true},
			spans: []trace.SpanData{
				testSpan(spanExecRemoteExecAttempt, time.Second, 4*time.Second),
				testSpan(spanExecLocalRun, 5*time.Second, 6*time.Second),
			},
			want: IntervalMetric(6 * time.Second),
		},
		{
			// Racing clears IsRemote when local wins, but the losing remote
			// attempt still logged its span into the shared trace context.
			name:    "racing_local_won",
			metrics: StepMetric{IsLocal: true, Racing: true, RacingWinner: "local"},
			spans: []trace.SpanData{
				testSpan(spanExecRemoteExecAttempt, time.Second, 8*time.Second),
				testSpan(spanExecLocalRun, time.Second, 3*time.Second),
			},
			want: IntervalMetric(3 * time.Second),
		},
		{
			// Racing clears IsLocal when remote wins; the canceled local racer
			// still logged a span.
			name:    "racing_remote_won",
			metrics: StepMetric{IsRemote: true, Racing: true, RacingWinner: "remote"},
			spans: []trace.SpanData{
				testSpan(spanExecLocalRun, time.Second, 8*time.Second),
				testSpan(spanExecRemoteExecAttempt, time.Second, 3*time.Second),
			},
			want: IntervalMetric(3 * time.Second),
		},
		{
			// Retries log one attempt span each; the last one produced the result.
			name:    "remote_exec_retried",
			metrics: StepMetric{IsRemote: true, RemoteRetry: 2},
			spans: []trace.SpanData{
				// Span start times deliberately out-of-order.
				testSpan(spanExecRemoteExecAttempt, time.Second, 2*time.Second),
				testSpan(spanExecRemoteExecAttempt, 10*time.Second, 4*time.Second),
				testSpan(spanExecRemoteExecAttempt, 5*time.Second, 3*time.Second),
			},
			want: IntervalMetric(4 * time.Second),
		},
		{
			name:    "no_run_spans",
			metrics: StepMetric{IsRemote: true},
			spans:   []trace.SpanData{testSpan(spanMaterializeInputs, time.Second, time.Second)},
			want:    0,
		},
		{
			name:    "no_strategy_flags",
			metrics: StepMetric{},
			spans:   []trace.SpanData{testSpan(spanExecLocalRun, time.Second, time.Second)},
			want:    0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := tc.metrics
			m.updateFromTrace(tc.spans, buildStart)
			if m.RunTime != tc.want {
				t.Errorf("RunTime=%v; want %v", time.Duration(m.RunTime), time.Duration(tc.want))
			}
		})
	}
}

func TestUpdateStepMetricsFromTrace_Filtering(t *testing.T) {
	spans := []trace.SpanData{
		testSpan("step:cxx", 0, 30*time.Second),
		testSpan(spanDepsCmd, time.Second, 4*time.Second),
		testSpan(spanScandepsRun, 2*time.Second, 3*time.Second),
		testSpan(spanMaterializeInputs, 6*time.Second, 5*time.Second),
		testSpan(spanExecRemoteCacheCheck, 11*time.Second, 2*time.Second),
		testSpan(spanExecRemoteCacheRun, 11*time.Second, 8*time.Second),
		testSpan(spanMaterializeOutputs, 17*time.Second, time.Second),
		testSpan("exec-remote-download", 19*time.Second, time.Second),
	}

	m := StepMetric{Cached: true}
	m.updateFromTrace(spans, buildStart)

	want := StepMetric{
		Cached:                 true,
		DepsScanTime:           IntervalMetric(4 * time.Second),
		ScandepsTime:           IntervalMetric(3 * time.Second),
		ScandepsStartTime:      IntervalMetric(2 * time.Second),
		CacheTime:              IntervalMetric(2 * time.Second),
		CacheStartTime:         IntervalMetric(11 * time.Second),
		MaterializeInputsTime:  IntervalMetric(5 * time.Second),
		MaterializeOutputsTime: IntervalMetric(time.Second),
		RunTime:                IntervalMetric(8 * time.Second),
		// deps-cmd and exec-remote-download are not metric spans, so they set
		// the above fields but are not logged below.
		Spans: []MetricSpan{
			{
				Name:          "step:cxx",
				StartNanos:    0,
				DurationNanos: (30 * time.Second).Nanoseconds(),
			},
			{
				Name:          spanScandepsRun,
				StartNanos:    (2 * time.Second).Nanoseconds(),
				DurationNanos: (3 * time.Second).Nanoseconds(),
			},
			{
				Name:          spanMaterializeInputs,
				StartNanos:    (6 * time.Second).Nanoseconds(),
				DurationNanos: (5 * time.Second).Nanoseconds(),
			},
			{
				Name:          spanExecRemoteCacheCheck,
				StartNanos:    (11 * time.Second).Nanoseconds(),
				DurationNanos: (2 * time.Second).Nanoseconds(),
			},
			{
				Name:          spanExecRemoteCacheRun,
				StartNanos:    (11 * time.Second).Nanoseconds(),
				DurationNanos: (8 * time.Second).Nanoseconds(),
			},
			{
				Name:          spanMaterializeOutputs,
				StartNanos:    (17 * time.Second).Nanoseconds(),
				DurationNanos: time.Second.Nanoseconds(),
			},
		},
	}
	if diff := cmp.Diff(want, m, cmp.AllowUnexported(StepMetric{})); diff != "" {
		t.Errorf("updateFromTrace metrics diff -want +got:\n%s", diff)
	}
}
