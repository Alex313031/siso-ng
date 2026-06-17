// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package e2etests

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	var localPID, preprocPID, remotePID, rbePID int
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
		"process_name":                9,
		"out/siso/gen/remote/foo.out": 3, // preproc, remote-exec and rbe
		"out/siso/gen/local/foo.out":  1,
		"thread_name":                 1,
	}
	if diff := cmp.Diff(want, names); diff != "" {
		t.Errorf("event names diff -want +got:\n%s", diff)
		t.Logf("trace json:\n%s", buf)
	}
}
