// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package e2etests

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/google/go-cmp/cmp"

	rpb "go.chromium.org/build/remote-apis/build/bazel/remote/execution/v2"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/build/ninjabuild"
	"go.chromium.org/build/siso/hashfs"
	"go.chromium.org/build/siso/o11y/trace"
	"go.chromium.org/build/siso/reapi/reapitest"
)

func TestBuild_Trace_remote(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	ctx := t.Context()
	dir := tempDir(t)

	runNinjaTest := func(t *testing.T, refake *reapitest.Fake) (build.Stats, error) {
		t.Helper()
		var ds build.DataSource
		defer func() {
			err := ds.Close(ctx)
			if err != nil {
				t.Error(err)
			}
		}()
		ds.Client = reapitest.New(ctx, t, refake)
		ds.Cache = ds.Client.CacheStore()

		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile:  ".siso_fs_state",
			DataSource: ds,
		})
		defer cleanup()
		opt.REAPIClient = ds.Client
		tracer, err := trace.NewTracer(ctx, "siso_trace.json")
		if err != nil {
			return build.Stats{}, err
		}
		defer tracer.Close(ctx)
		opt.Tracer = tracer
		return ninjabuild.Run(ctx, graph, opt, nil, ninjabuild.RunNinjaOpts{})
	}

	setupFiles(t, dir, t.Name(), nil)

	fakere := &reapitest.Fake{
		ExecuteFunc: func(fakere *reapitest.Fake, action *rpb.Action) (*rpb.ActionResult, error) {
			tree := reapitest.InputTree{CAS: fakere.CAS, Root: action.InputRootDigest}
			fn, err := tree.LookupFileNode(ctx, "foo.in")
			if err != nil {
				t.Logf("missing file foo.in in input")
				return &rpb.ActionResult{
					ExitCode:  1,
					StderrRaw: fmt.Appendf(nil, "../../foo.in: File not found: %v", err),
				}, nil
			}
			d := fn.Digest
			return &rpb.ActionResult{
				ExitCode: 0,
				OutputFiles: []*rpb.OutputFile{
					{
						Path:   "gen/remote/foo.out",
						Digest: d,
					},
				},
			}, nil
		},
	}
	stats, err := runNinjaTest(t, fakere)
	if err != nil {
		t.Fatalf("ninja %v: want nil err", err)
	}
	if stats.Done != stats.Total || stats.Remote != 1 {
		t.Errorf("done=%d remote=%d total=%d; want done=total, remote=1: %#v", stats.Done, stats.Remote, stats.Total, stats)
	}

	buf, err := os.ReadFile(filepath.Join(dir, "out/siso/siso_trace.json"))
	if err != nil {
		t.Fatal(err)
	}
	trace := make(map[string]any)
	err = json.Unmarshal(buf, &trace)
	if err != nil {
		t.Fatalf("unmarshal siso_trace.json: %v", err)
	}
	v, ok := trace["traceEvents"]
	if !ok {
		t.Fatalf("no traceEvents in siso_trace.json: %s", buf)
	}
	events, ok := v.([]any)
	if !ok {
		t.Fatalf("traceEvents is not array: %v", v)
	}
	names := make(map[string]int)
	var localPID, preprocPID, remotePID, rbePID, criticalPID int
	var critical int
	for i, v := range events {
		ev, ok := v.(map[string]any)
		if !ok {
			t.Errorf("traceEvents[%d] not map: %v", i, v)
			continue
		}
		ph, ok := ev["ph"].(string)
		if !ok {
			t.Errorf("no ph in traceEvents[%d] %v", i, v)
			continue
		}
		switch ph {
		case "M": // process_name
		case "X": // step
		default:
			// ignore counters etc
			continue
		}
		name, ok := ev["name"].(string)
		if !ok {
			t.Errorf("no name in traceEvents[%d] %v", i, v)
			continue
		}
		if cat, _ := ev["cat"].(string); cat == "critical" {
			critical++
			if pid, _ := ev["pid"].(float64); int(pid) != criticalPID {
				t.Errorf("pid of critical %s: %d; want %d", name, int(pid), criticalPID)
			}
			continue
		}
		names[name]++
		switch name {
		case "process_name":
			args, ok := ev["args"].(map[string]any)
			if !ok {
				t.Errorf("no args in traceEvents[%d] %v", i, v)
				continue
			}
			pname, ok := args["name"].(string)
			if !ok {
				t.Errorf("no name in traceEvents[%d].args: %v", i, args)
				continue
			}
			pid, ok := ev["pid"].(float64)
			if !ok {
				t.Errorf("no pid in traceEvents[%d] %v", i, v)
				continue
			}
			t.Logf("-- process_name:%s = %d", pname, int(pid))
			switch pname {
			case "preproc":
				preprocPID = int(pid)
			case "local-exec":
				localPID = int(pid)
			case "remote-exec":
				remotePID = int(pid)
			case "rbe":
				rbePID = int(pid)
			case "critical path":
				criticalPID = int(pid)
			}
			continue
		case "out/siso/gen/remote/foo.out":
			pid, ok := ev["pid"].(float64)
			if !ok {
				t.Errorf("no pid in traceEvents[%d] %v", i, v)
				continue
			}
			if int(pid) != preprocPID && int(pid) != remotePID && int(pid) != rbePID {
				t.Errorf("pid of %s: %d; want %d or %d or %d", name, int(pid), preprocPID, remotePID, rbePID)
			}

		case "out/siso/gen/local/foo.out":
			pid, ok := ev["pid"].(float64)
			if !ok {
				t.Errorf("no pid in traceEvents[%d] %v", i, v)
				continue
			}
			if int(pid) != localPID {
				t.Errorf("pid of %s: %d; want %d", name, int(pid), localPID)
			}
		}
	}
	want := map[string]int{
		"process_name":                10,
		"process_sort_index":          1, // critical path
		"out/siso/gen/remote/foo.out": 3, // preproc, remote-exec and rbe
		"out/siso/gen/local/foo.out":  1,
		"thread_name":                 2, // critical path and rbe worker
	}
	if diff := cmp.Diff(want, names); diff != "" {
		t.Errorf("event names diff -want +got:\n%s", diff)
		t.Logf("trace json:\n%s", buf)
	}
	// Both steps are roots, so the path is just whichever finished last.
	if critical != 1 {
		t.Errorf("critical path slices: %d; want 1", critical)
	}
}

func TestBuild_Trace_flow(t *testing.T) {
	if !runInSubProcess(t) {
		return
	}
	ctx := t.Context()
	dir := tempDir(t)
	setupFiles(t, dir, t.Name(), nil)

	runNinjaTest := func(t *testing.T) (build.Stats, error) {
		t.Helper()
		opt, graph, cleanup := setupBuild(ctx, t, dir, hashfs.Option{
			StateFile: ".siso_fs_state",
		})
		defer cleanup()
		tracer, err := trace.NewTracer(ctx, "siso_trace.json")
		if err != nil {
			return build.Stats{}, err
		}
		defer tracer.Close(ctx) // flushes the footer; read the file after

		opt.Tracer = tracer
		return ninjabuild.Run(ctx, graph, opt, nil, ninjabuild.RunNinjaOpts{})
	}

	stats, err := runNinjaTest(t)
	if err != nil {
		t.Fatalf("ninja %v; want nil err", err)
	}
	if stats.Done != stats.Total || stats.Local != 3 {
		t.Errorf("done=%d local=%d total=%d; want done=total, local=3: %#v", stats.Done, stats.Local, stats.Total, stats)
	}

	buf, err := os.ReadFile(filepath.Join(dir, "out/siso/siso_trace.json"))
	if err != nil {
		t.Fatal(err)
	}
	var traceJSON struct {
		TraceEvents []trace.Event `json:"traceEvents"`
	}
	err = json.Unmarshal(buf, &traceJSON)
	if err != nil {
		t.Fatalf("unmarshal siso_trace.json: %v", err)
	}

	pids := make(map[string]int64)
	tids := make(map[string]int64)
	var slices, critical []trace.Event
	starts := make(map[int64]trace.Event)
	finishes := make(map[int64]trace.Event)
	hopStarts := make(map[int64]trace.Event)
	hopFinishes := make(map[int64]trace.Event)
	for _, ev := range traceJSON.TraceEvents {
		switch ev.Ph {
		case "M":
			name, _ := ev.Args["name"].(string)
			switch ev.Name {
			case "process_name":
				pids[name] = ev.Pid
			case "thread_name":
				tids[name] = ev.Tid
			}
		case "X":
			if ev.Cat == "critical" {
				critical = append(critical, ev)
			} else {
				slices = append(slices, ev)
			}
		case "s":
			if ev.Name != "dep" || (ev.Cat != "flow" && ev.Cat != "critical") {
				t.Errorf("flow start %q/%q; want dep/flow or dep/critical", ev.Name, ev.Cat)
				continue
			}
			if ev.Cat == "critical" {
				if _, ok := hopStarts[ev.ID]; ok {
					t.Errorf("duplicate hop start id=%d", ev.ID)
				}
				hopStarts[ev.ID] = ev
			} else {
				if _, ok := starts[ev.ID]; ok {
					t.Errorf("duplicate flow start id=%d", ev.ID)
				}
				starts[ev.ID] = ev
			}
		case "f":
			if ev.Name != "dep" || (ev.Cat != "flow" && ev.Cat != "critical") {
				t.Errorf("flow finish %q/%q; want dep/flow or dep/critical", ev.Name, ev.Cat)
				continue
			}
			if ev.Bp != "e" {
				t.Errorf("%s finish id=%d bp=%q; want e", ev.Cat, ev.ID, ev.Bp)
			}
			if ev.Cat == "critical" {
				if _, ok := hopFinishes[ev.ID]; ok {
					t.Errorf("duplicate hop finish id=%d", ev.ID)
				}
				hopFinishes[ev.ID] = ev
			} else {
				if _, ok := finishes[ev.ID]; ok {
					t.Errorf("duplicate flow finish id=%d", ev.ID)
				}
				finishes[ev.ID] = ev
			}
		}
	}
	// The chain is a -> b -> c, so two arrows.
	if len(starts) != 2 || len(finishes) != 2 {
		t.Fatalf("flow starts=%d finishes=%d; want 2 and 2:\n%s", len(starts), len(finishes), buf)
	}

	// Every end must land inside a slice, and follow the recorded step ids.
	enclosing := func(e trace.Event, slices []trace.Event) (trace.Event, bool) {
		for _, s := range slices {
			if s.Pid == e.Pid && s.Tid == e.Tid && e.T >= s.T && e.T < s.T+s.Dur {
				return s, true
			}
		}
		return trace.Event{}, false
	}
	stepArg := func(ev trace.Event, key string) string {
		v, _ := ev.Args[key].(string)
		return v
	}
	for id, start := range starts {
		finish, ok := finishes[id]
		if !ok {
			t.Errorf("flow id=%d has a start but no finish", id)
			continue
		}
		src, ok := enclosing(start, slices)
		if !ok {
			t.Errorf("flow id=%d start %+v is not inside any slice", id, start)
			continue
		}
		dst, ok := enclosing(finish, slices)
		if !ok {
			t.Errorf("flow id=%d finish %+v is not inside any slice", id, finish)
			continue
		}
		if got, want := stepArg(dst, "prev_id"), stepArg(src, "id"); got != want {
			t.Errorf("flow id=%d points %s -> %s; the target's prev_id is %s, not the source step %s",
				id, src.Name, dst.Name, got, want)
		}
		if start.T > finish.T {
			t.Errorf("flow id=%d runs backwards: start ts %g > finish ts %g", id, start.T, finish.T)
		}
	}

	// The critical path is the whole chain, oldest first, on one row of its
	// own track, each slice covering the step's exec slice.
	sort.Slice(critical, func(i, j int) bool { return critical[i].T < critical[j].T })
	var names []string
	for _, c := range critical {
		names = append(names, c.Name)
	}
	want := []string{"out/siso/gen/a.out", "out/siso/gen/b.out", "out/siso/gen/c.out"}
	if diff := cmp.Diff(want, names); diff != "" {
		t.Fatalf("critical path diff -want +got:\n%s\n%s", diff, buf)
	}
	for i, c := range critical {
		if c.Pid != pids["critical path"] || c.Tid != tids["critical path"] {
			t.Errorf("%s: pid=%d tid=%d; want the critical path track %d/%d", c.Name, c.Pid, c.Tid, pids["critical path"], tids["critical path"])
		}
		if i > 0 {
			if got, want := stepArg(c, "prev_id"), stepArg(critical[i-1], "id"); got != want {
				t.Errorf("%s: prev_id %q; want %q, the step before it", c.Name, got, want)
			}
			if prev := critical[i-1]; c.T < prev.T+prev.Dur {
				t.Errorf("%s starts at %g, before %s ends at %g", c.Name, c.T, prev.Name, prev.T+prev.Dur)
			}
		}
		for _, s := range slices {
			if s.Name == c.Name && (c.T > s.T || s.T+s.Dur > c.T+c.Dur) {
				t.Errorf("%s: critical slice [%g, %g] does not cover its %s slice [%g, %g]", c.Name, c.T, c.T+c.Dur, s.Cat, s.T, s.T+s.Dur)
			}
		}
	}
	if len(hopStarts) != 2 || len(hopFinishes) != 2 {
		t.Fatalf("critical hop starts=%d finishes=%d; want 2 and 2", len(hopStarts), len(hopFinishes))
	}
	for id, start := range hopStarts {
		finish, ok := hopFinishes[id]
		if !ok {
			t.Errorf("hop id=%d has a start but no finish", id)
			continue
		}
		src, ok := enclosing(start, critical)
		if !ok {
			t.Errorf("hop id=%d start %+v is not inside any critical slice", id, start)
			continue
		}
		dst, ok := enclosing(finish, critical)
		if !ok || dst.T != finish.T {
			t.Errorf("hop id=%d finish %+v does not start a critical slice", id, finish)
			continue
		}
		if got, want := stepArg(dst, "prev_id"), stepArg(src, "id"); got != want {
			t.Errorf("hop id=%d points %s -> %s; the target's prev_id is %s, not the source step %s",
				id, src.Name, dst.Name, got, want)
		}
	}
}
