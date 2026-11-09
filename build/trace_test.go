// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"go.chromium.org/build/siso/o11y/trace"
)

func TestFlowEnds(t *testing.T) {
	for _, tc := range []struct {
		name     string
		events   []trace.Event
		src, dst flowPoint
		want     bool
	}{
		{
			name: "empty",
		},
		{
			name:   "single",
			events: []trace.Event{{T: 100, Dur: 30, Pid: 2, Tid: 7}},
			src:    flowPoint{ts: 115, pid: 2, tid: 7},
			dst:    flowPoint{ts: 100, pid: 2, tid: 7},
			want:   true,
		},
		{
			name:   "zero_width",
			events: []trace.Event{{T: 100, Pid: 2, Tid: 7}},
		},
		{
			name: "skips_zero_width",
			events: []trace.Event{
				{T: 50, Pid: 2, Tid: 7},
				{T: 100, Dur: 30, Pid: 3, Tid: 8},
				{T: 400, Pid: 4, Tid: 9},
			},
			src:  flowPoint{ts: 115, pid: 3, tid: 8},
			dst:  flowPoint{ts: 100, pid: 3, tid: 8},
			want: true,
		},
		{
			// Sub-microsecond spans (in nanoseconds) keep a non-zero width.
			name:   "sub_microsecond",
			events: []trace.Event{{T: 100000, Dur: 500, Pid: 2, Tid: 7}},
			src:    flowPoint{ts: 100250, pid: 2, tid: 7},
			dst:    flowPoint{ts: 100000, pid: 2, tid: 7},
			want:   true,
		},
		{
			// Exact nanosecond arithmetic needs no float64 rounding.
			name:   "exact_midpoint",
			events: []trace.Event{{T: 7473334, Dur: 26073276, Pid: 2, Tid: 7}},
			src:    flowPoint{ts: 20509972, pid: 2, tid: 7},
			dst:    flowPoint{ts: 7473334, pid: 2, tid: 7},
			want:   true,
		},
		{
			name: "latest_end",
			events: []trace.Event{
				{T: 100, Dur: 10, Pid: 2, Tid: 7},
				{T: 120, Dur: 500, Pid: 3, Tid: 8},
				{T: 200, Dur: 10, Pid: 4, Tid: 9},
			},
			src:  flowPoint{ts: 370, pid: 3, tid: 8},
			dst:  flowPoint{ts: 100, pid: 2, tid: 7},
			want: true,
		},
		{
			name: "earliest_start",
			events: []trace.Event{
				{T: 300, Dur: 10, Pid: 2, Tid: 7},
				{T: 50, Dur: 10, Pid: 3, Tid: 8},
			},
			src:  flowPoint{ts: 305, pid: 2, tid: 7},
			dst:  flowPoint{ts: 50, pid: 3, tid: 8},
			want: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src, dst, ok := flowEnds(tc.events)
			if ok != tc.want {
				t.Errorf("flowEnds(...) ok=%t; want %t", ok, tc.want)
			}
			if src != tc.src || dst != tc.dst {
				t.Errorf("flowEnds(...) = %+v, %+v; want %+v, %+v", src, dst, tc.src, tc.dst)
			}
		})
	}
}

// newTestTracer is enabled so newDepRecorder builds one, never started so
// nothing is written.
func newTestTracer(t *testing.T) *trace.Tracer {
	t.Helper()
	tracer, err := trace.NewTracer(t.Context(), filepath.Join(t.TempDir(), "siso_trace.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tracer.Close(t.Context()) })
	return tracer
}

func slice(ts, dur trace.Micros, pid, tid int64) []trace.Event {
	return []trace.Event{{T: ts, Dur: dur, Pid: pid, Tid: tid}}
}

// step is a stepTrace with just the ids an arrow needs.
func step(id, prev string) stepTrace {
	return stepTrace{attr: spanEventAttr{id: id, prevID: prev}}
}

// critical is a step the critical path can draw: name labels its slice, the
// times bound it.
func critical(id, prev, name string, start, release, end time.Duration) stepTrace {
	st := step(id, prev)
	st.attr.output0 = name
	st.start, st.release, st.end = start, release, end
	return st
}

// flowArrow is one arrow rebuilt from its event pair.
type flowArrow struct {
	src, dst flowPoint
}

// arrows rebuilds arrows from drain(), checking labels, pairing and id
// uniqueness. Sorted by target; flows are emitted in map order.
func arrows(t *testing.T, events []trace.Event) []flowArrow {
	t.Helper()
	if len(events)%2 != 0 {
		t.Fatalf("drain() returned %d events; want start and finish pairs", len(events))
	}
	seen := make(map[int64]bool)
	var got []flowArrow
	for i := 0; i < len(events); i += 2 {
		start, finish := events[i], events[i+1]
		if start.Ph != "s" || finish.Ph != "f" || finish.Bp != "e" {
			t.Errorf("arrow %d: ph %q/%q bp %q; want s, f, e", i/2, start.Ph, finish.Ph, finish.Bp)
		}
		for _, ev := range []trace.Event{start, finish} {
			if ev.Name != flowName || ev.Cat != flowCat {
				t.Errorf("arrow %d: %q/%q; want %q/%q", i/2, ev.Name, ev.Cat, flowName, flowCat)
			}
		}
		if start.ID != finish.ID {
			t.Errorf("arrow %d: start id %d, finish id %d; want one id", i/2, start.ID, finish.ID)
		}
		if seen[start.ID] {
			t.Errorf("arrow %d: flow id %d handed out twice", i/2, start.ID)
		}
		seen[start.ID] = true
		got = append(got, flowArrow{
			src: flowPoint{ts: start.T, pid: start.Pid, tid: start.Tid},
			dst: flowPoint{ts: finish.T, pid: finish.Pid, tid: finish.Tid},
		})
	}
	slices.SortFunc(got, func(a, b flowArrow) int {
		switch {
		case a.dst.ts < b.dst.ts:
			return -1
		case a.dst.ts > b.dst.ts:
			return 1
		}
		return 0
	})
	return got
}

var allowFlowUnexported = cmp.AllowUnexported(flowArrow{}, flowPoint{})

func TestDepRecorder(t *testing.T) {
	f := newDepRecorder(t.Context(), newTestTracer(t))
	f.add(step("root", ""), slice(10, 40, 2, 1))
	f.add(step("mid", "root"), slice(60, 20, 3, 4))
	f.add(step("leaf", "mid"), slice(90, 5, 3, 5))
	f.add(step("orphan", "untraced"), slice(95, 5, 3, 6)) // predecessor never traced
	f.add(step("empty", "root"), nil)                     // no slices at all

	want := []flowArrow{
		{src: flowPoint{ts: 30, pid: 2, tid: 1}, dst: flowPoint{ts: 60, pid: 3, tid: 4}},
		{src: flowPoint{ts: 70, pid: 3, tid: 4}, dst: flowPoint{ts: 90, pid: 3, tid: 5}},
	}
	if diff := cmp.Diff(want, arrows(t, f.drain()), allowFlowUnexported); diff != "" {
		t.Errorf("arrows diff -want +got:\n%s", diff)
	}
}

// A step with no slice of its own must pass the arrow on, not swallow it.
func TestDepRecorderWalksThroughUnanchoredSteps(t *testing.T) {
	f := newDepRecorder(t.Context(), newTestTracer(t))
	f.add(step("root", ""), slice(10, 40, 2, 1))
	f.add(step("hub", "root"), slice(60, 0, 3, 4))
	f.add(step("hub2", "hub"), slice(61, 0, 3, 4))
	f.add(step("leaf", "hub2"), slice(90, 5, 3, 5))
	f.add(step("leaf2", "hub"), slice(95, 5, 3, 6))

	// Both reach past the two sliceless steps, back to root.
	want := []flowArrow{
		{src: flowPoint{ts: 30, pid: 2, tid: 1}, dst: flowPoint{ts: 90, pid: 3, tid: 5}},
		{src: flowPoint{ts: 30, pid: 2, tid: 1}, dst: flowPoint{ts: 95, pid: 3, tid: 6}},
	}
	if diff := cmp.Diff(want, arrows(t, f.drain()), allowFlowUnexported); diff != "" {
		t.Errorf("arrows diff -want +got:\n%s", diff)
	}
}

func TestDepRecorderChainStopsAtUnanchoredRoot(t *testing.T) {
	f := newDepRecorder(t.Context(), newTestTracer(t))
	f.add(step("root", ""), slice(10, 0, 2, 1))
	f.add(step("leaf", "root"), slice(90, 5, 3, 5))
	if got := f.drain(); len(got) != 0 {
		t.Errorf("drain() = %v; want none", got)
	}
}

// A cycle must not spin forever.
func TestDepRecorderBoundsTheWalk(t *testing.T) {
	f := newDepRecorder(t.Context(), newTestTracer(t))
	f.add(step("a", "b"), slice(10, 0, 2, 1))
	f.add(step("b", "a"), slice(20, 0, 2, 1))
	f.add(step("leaf", "a"), slice(90, 5, 3, 5))
	if got := f.drain(); len(got) != 0 {
		t.Errorf("drain() = %v; want none", got)
	}
}

// Builders share a trace file, so a repeated id pairs one build's arrow with
// another's.
func TestDepRecorderIDsAreUniquePerTracer(t *testing.T) {
	tracer := newTestTracer(t)
	seen := map[int64]bool{}
	for range 2 {
		f := newDepRecorder(t.Context(), tracer)
		f.add(step("root", ""), slice(10, 40, 2, 1))
		f.add(step("leaf", "root"), slice(60, 20, 3, 4))
		for _, ev := range f.drain() {
			if ev.ID == 0 {
				t.Errorf("flow event with no id: %+v", ev)
			}
			if ev.Ph == "s" && seen[ev.ID] {
				t.Errorf("flow id %d handed out twice", ev.ID)
			}
			if ev.Ph == "s" {
				seen[ev.ID] = true
			}
		}
	}
	if len(seen) != 2 {
		t.Errorf("got %d distinct flow ids; want 2", len(seen))
	}
}

// A build can return before its steps do.
func TestDepRecorderAddAfterDrain(t *testing.T) {
	f := newDepRecorder(t.Context(), newTestTracer(t))
	f.add(step("root", ""), slice(10, 40, 2, 1))
	f.drain()
	f.add(step("late", "root"), slice(60, 20, 3, 4))
	if got := f.drain(); len(got) != 0 {
		t.Errorf("drain() = %v; want none after the table is released", got)
	}
}

func TestDepRecorderDisabled(t *testing.T) {
	f := newDepRecorder(t.Context(), nil)
	if f != nil {
		t.Fatalf("newDepRecorder(t.Context(), nil) = %v; want nil", f)
	}
	// Must stay inert rather than panic.
	f.add(step("root", ""), slice(10, 40, 2, 1))
	if got := f.drain(); got != nil {
		t.Errorf("drain() = %v; want nil", got)
	}
	f.record()
}

func TestDepRecorderCriticalPath(t *testing.T) {
	f := newDepRecorder(t.Context(), newTestTracer(t))
	const us = time.Microsecond
	// a -> b -> d is the chain; c is a branch that finished earlier. b keeps
	// working for 20us after releasing d, which is off the path; d's tail
	// counts, nothing came after it.
	f.add(critical("a", "", "a.o", 0, 10*us, 12*us), slice(0, 10, 2, 1))
	f.add(critical("b", "a", "b.o", 15*us, 40*us, 60*us), slice(15, 25, 2, 1))
	f.add(critical("c", "a", "c.o", 15*us, 20*us, 20*us), slice(15, 5, 2, 2))
	f.add(critical("d", "b", "d.so", 45*us, 90*us, 95*us), slice(45, 45, 2, 1))

	var slices, hops []trace.Event
	for _, ev := range f.drain() {
		switch {
		case ev.Ph == "X":
			slices = append(slices, ev)
		case ev.Cat == criticalCat:
			hops = append(hops, ev)
		}
	}
	want := []trace.Event{
		{Name: "a.o", Cat: criticalCat, Ph: "X", T: 0, Dur: trace.Micros(10 * us), Pid: f.pid, Tid: f.tid},
		{Name: "b.o", Cat: criticalCat, Ph: "X", T: trace.Micros(15 * us), Dur: trace.Micros(25 * us), Pid: f.pid, Tid: f.tid},
		{Name: "d.so", Cat: criticalCat, Ph: "X", T: trace.Micros(45 * us), Dur: trace.Micros(50 * us), Pid: f.pid, Tid: f.tid},
	}
	if diff := cmp.Diff(want, slices, cmpopts.IgnoreFields(trace.Event{}, "Args")); diff != "" {
		t.Errorf("critical path slices diff -want +got:\n%s", diff)
	}
	if len(slices) == 3 {
		if got, want := slices[1].Args["prev_id"], "a"; got != want {
			t.Errorf("b.o prev_id = %v; want %v", got, want)
		}
		if got, want := slices[1].Args["after_release_ms"], 0.02; got != want {
			t.Errorf("b.o after_release_ms = %v; want %v", got, want)
		}
	}
	if len(hops) != 4 {
		t.Fatalf("got %d hop events; want 2 hops", len(hops))
	}
	for i := 0; i < len(hops); i += 2 {
		start, finish := hops[i], hops[i+1]
		if start.Ph != "s" || finish.Ph != "f" || finish.Bp != "e" || start.ID != finish.ID {
			t.Errorf("hop %d malformed: %+v %+v", i/2, start, finish)
		}
		if !inSlice(slices, start) || !startsSlice(slices, finish) {
			t.Errorf("hop %d does not connect two slices: %v -> %v", i/2, start.T, finish.T)
		}
	}
}

func TestDepRecorderCriticalPathArgs(t *testing.T) {
	st := critical("a", "", "a.o", 0, 100*time.Millisecond, 112*time.Millisecond)
	st.attr.action = "cxx"
	st.times = stepTimes{cache: 1500 * time.Microsecond, materializeOutputs: 48239648071}
	want := map[string]any{
		"id":                     "a",
		"description":            "",
		"action":                 "cxx",
		"command":                "",
		"backtrace":              "",
		"prev_id":                "",
		"cache_ms":               1.5,
		"materialize_outputs_ms": 48239.648,
		"after_release_ms":       12.0,
	}
	if diff := cmp.Diff(want, st.args()); diff != "" {
		t.Errorf("args diff -want +got:\n%s", diff)
	}
}

func inSlice(slices []trace.Event, ev trace.Event) bool {
	for _, s := range slices {
		if s.Pid == ev.Pid && s.Tid == ev.Tid && ev.T >= s.T && ev.T < s.T+s.Dur {
			return true
		}
	}
	return false
}

func startsSlice(slices []trace.Event, ev trace.Event) bool {
	for _, s := range slices {
		if s.Pid == ev.Pid && s.Tid == ev.Tid && s.T == ev.T {
			return true
		}
	}
	return false
}
