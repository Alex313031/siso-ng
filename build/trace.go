// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"go.chromium.org/build/siso/build/metadata"
	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/o11y/iometrics"
	"go.chromium.org/build/siso/o11y/trace"
	"go.chromium.org/build/siso/sync/semaphore"
)

type traceEvents struct {
	// metadata of the build.
	metadata metadata.Metadata

	// filename of trace json file.
	fname string

	// number of traces written.
	num int
	// start time of the trace.
	start time.Time

	// pass traceEventObject from Add to write.
	q chan traceEventObject
	// signals to terminate trace writer.
	quit, done chan struct{}

	// memstats record of siso.
	memstats runtime.MemStats
	// resource usage record of siso.
	rusage usageRecord
	// system resource record
	sys sysRecord

	// iometrics to emit in trace json.
	ioms []*iometrics.IOMetrics
	// iostats to emit in trace json.
	iostats []iometrics.Stats
	// semaphores to emit in trace json.
	semas []semaphore.Monitorable
	// last number of requests using in semaphore.
	semaReqs []int

	mu sync.Mutex
	// RBE worker id -> index of worker.
	rbeWorkers map[string]int
}

func newTraceEvents(fname string, metadata metadata.Metadata) *traceEvents {
	return &traceEvents{
		metadata:   metadata,
		fname:      fname,
		q:          make(chan traceEventObject, 10000),
		quit:       make(chan struct{}),
		done:       make(chan struct{}),
		rbeWorkers: make(map[string]int),
	}
}

func (te *traceEvents) Start(ctx context.Context, semas []semaphore.Monitorable, ioms []*iometrics.IOMetrics) {
	te.semas = semas
	te.semaReqs = make([]int, len(semas))
	te.ioms = ioms
	te.iostats = make([]iometrics.Stats, len(ioms))
	te.start = time.Now()
	go te.loop(ctx)
}

func (te *traceEvents) loop(ctx context.Context) {
	clog.Infof(ctx, "trace loop start")
	defer close(te.done)
	runtime.ReadMemStats(&te.memstats)
	te.rusage.get()
	te.sys.get(ctx)
	w := io.Discard
	if te.fname != "" {
		tmpname := te.fname + ".tmp"
		f, err := os.Create(te.fname + ".tmp")
		if err != nil {
			clog.Warningf(ctx, "Failed to create %s: %v", tmpname, err)
			return
		}
		w = bufio.NewWriterSize(f, 256*1024)
		defer func() {
			fmt.Fprintf(w, "\n]")
			if fw, ok := w.(interface{ Flush() error }); ok {
				err := fw.Flush()
				if err != nil {
					clog.Warningf(ctx, "Failed to flush %s: %v", tmpname, err)
				}
			}
			err = f.Close()
			if err != nil {
				clog.Warningf(ctx, "Failed to close %s: %v", tmpname, err)
			}
		}()
	}
	fmt.Fprintf(w, "[\n")
	te.rusage.start = te.start
	te.sys.start = te.start
	te.write(ctx, w, traceEventObject{
		Name: "process_name",
		Ph:   "M",
		Pid:  sysPid,
		Tid:  sysTid,
		Args: map[string]any{
			"name": "sys",
		},
	})
	te.write(ctx, w, traceEventObject{
		Name: "process_name",
		Ph:   "M",
		Pid:  sisoPid,
		Tid:  sisoTid,
		Args: map[string]any{
			"name": "siso",
		},
	})
	te.write(ctx, w, traceEventObject{
		Name: "process_name",
		Ph:   "M",
		Pid:  sisoSemaPid,
		Tid:  sisoTid,
		Args: map[string]any{
			"name": "siso-sema",
		},
	})
	te.write(ctx, w, traceEventObject{
		Name: "process_name",
		Ph:   "M",
		Pid:  sisoPreprocPid,
		Tid:  sisoTid,
		Args: map[string]any{
			"name": "preproc",
		},
	})
	te.write(ctx, w, traceEventObject{
		Name: "process_name",
		Ph:   "M",
		Pid:  sisoLocalPid,
		Tid:  sisoTid,
		Args: map[string]any{
			"name": "local-exec",
		},
	})
	te.write(ctx, w, traceEventObject{
		Name: "process_name",
		Ph:   "M",
		Pid:  sisoRemotePid,
		Tid:  sisoTid,
		Args: map[string]any{
			"name": "remote-exec",
		},
	})
	te.write(ctx, w, traceEventObject{
		Name: "process_name",
		Ph:   "M",
		Pid:  sisoRBEPid,
		Tid:  sisoTid,
		Args: map[string]any{
			"name": "rbe",
		},
	})
	for i, sema := range te.ioms {
		te.write(ctx, w, traceEventObject{
			Name: "process_name",
			Ph:   "M",
			Pid:  int64(sisoIOPid + i),
			Tid:  sisoTid,
			Args: map[string]any{
				"name": "siso-io-" + sema.Name(),
			},
		})
	}
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-te.quit:
			clog.Infof(ctx, "trace loop quit len=%d", len(te.q))
			var timeout = time.After(1 * time.Second)
		quit:
			for len(te.q) > 0 {
				select {
				case obj := <-te.q:
					te.write(ctx, w, obj)
				case <-timeout:
					clog.Warningf(ctx, "timed out")
					break quit
				}
			}
			clog.Infof(ctx, "trace loop quit done")
			return

		case t := <-ticker.C:
			te.sample(ctx, w, t)

		case obj := <-te.q:
			te.write(ctx, w, obj)
		}
	}
}

const (
	sysPid = iota + 1
	sisoPid
	sisoSemaPid
	sisoPreprocPid
	sisoLocalPid
	sisoRemotePid
	sisoRBEPid
	sisoIOPid
)

const (
	sysTid  = 1
	sisoTid = 1
)

// traceEventObject is trace event for trace json.
// see https://docs.google.com/document/d/1CvAClvFfyA5R-PhYUmn5OOQtYMH4h6I0nSsKchNAySU/preview
type traceEventObject struct {
	// The name of the event, as displayed in trace viewer.
	Name string `json:"name"`

	// The event categories.
	// This is comma separated list of categories for the event.
	// The categories can be used to hide events in the trace viewer UI.
	Cat string `json:"cat,omitempty"`

	// The event type.
	// This is a single character which changes depending on the type
	// of event being output.
	Ph string `json:"ph"`

	// The tracing clock timestamp of the event.
	// The timestamps are provided at microsecond granularity.
	T int64 `json:"ts"`

	// The process ID of the process that output this event.
	Pid int64 `json:"pid"`

	// The thread ID of the thread that output this event.
	Tid int64 `json:"tid"`

	// The tracing clock duration of complete events in microseconds.
	// Used for "ph"="X".
	Dur int64 `json:"dur,omitempty"`

	// Any arguments provided for the event.
	Args map[string]any `json:"args,omitempty"`
}

func (te *traceEvents) sample(ctx context.Context, w io.Writer, t time.Time) {
	for _, o := range te.traceMemStats(t) {
		te.write(ctx, w, o)
	}
	for _, o := range te.rusage.sample(t) {
		te.write(ctx, w, o)
	}
	for _, o := range te.sys.sample(ctx, t) {
		te.write(ctx, w, o)
	}

	for i, sema := range te.semas {
		if sema == nil {
			continue
		}
		for _, o := range te.traceSemaphore(t, sema, &te.semaReqs[i]) {
			te.write(ctx, w, o)
		}
	}
	for i, m := range te.ioms {
		if m == nil {
			continue
		}
		for _, o := range te.traceIOMetrics(t, int64(sisoIOPid+i), m, &te.iostats[i]) {
			te.write(ctx, w, o)
		}
	}
}

func (te *traceEvents) traceMemStats(t time.Time) []traceEventObject {
	ret := []traceEventObject{
		{
			Name: "memstats",
			Ph:   "C",
			T:    t.Sub(te.start).Microseconds(),
			Pid:  sisoPid,
			Tid:  sisoTid,
			Args: map[string]any{
				"alloc":       te.memstats.Alloc,
				"total_alloc": te.memstats.TotalAlloc,
				"sys":         te.memstats.Sys,
				"pause":       te.memstats.PauseTotalNs,
				"gc":          te.memstats.NumGC,
			},
		},
	}
	runtime.ReadMemStats(&te.memstats)
	return ret
}

func (te *traceEvents) traceSemaphore(t time.Time, sema semaphore.Monitorable, reqs *int) []traceEventObject {
	r := sema.NumRequests()
	rate := r - *reqs
	*reqs = r
	return []traceEventObject{
		{
			Name: sema.Name(),
			Ph:   "C",
			T:    t.Sub(te.start).Microseconds(),
			Pid:  sisoSemaPid,
			Tid:  sisoTid,
			Args: map[string]any{
				"queue": sema.NumWaits(),
				"serv":  sema.NumServs(),
				"rate":  rate,
			},
		},
	}
}

func (te *traceEvents) traceIOMetrics(t time.Time, pid int64, m *iometrics.IOMetrics, s *iometrics.Stats) []traceEventObject {
	stats := m.Stats()

	o := traceEventObject{
		Ph:  "C",
		T:   t.Sub(te.start).Microseconds(),
		Pid: pid,
		Tid: sisoTid,
	}
	ret := make([]traceEventObject, 0, 3)
	o.Name = m.Name() + "-ops"
	o.Args = map[string]any{
		"ops/s":  stats.Ops - s.Ops,
		"errs/s": stats.OpsErrs - s.OpsErrs,
	}
	ret = append(ret, o)
	o.Name = m.Name() + "-read"
	o.Args = map[string]any{
		"ops/s":   stats.ROps - s.ROps,
		"bytes/s": stats.RBytes - s.RBytes,
		"errs/s":  stats.RErrs - s.RErrs,
	}
	ret = append(ret, o)
	o.Name = m.Name() + "-write"
	o.Args = map[string]any{
		"ops/s":   stats.WOps - s.WOps,
		"bytes/s": stats.WBytes - s.WBytes,
		"errs/s":  stats.WErrs - s.WErrs,
	}
	ret = append(ret, o)
	*s = stats
	return ret
}

func (te *traceEvents) write(ctx context.Context, w io.Writer, obj traceEventObject) {
	if te.num > 0 {
		fmt.Fprintf(w, ",\n ")
	}
	buf, err := json.Marshal(obj)
	if err != nil {
		clog.Warningf(ctx, "Failed to marshal %v: %v", obj, err)
		return
	}
	te.num++
	w.Write(buf)
}

func (te *traceEvents) Add(ctx context.Context, tc *trace.Context) {
	spans := tc.Spans()
	if len(spans) == 0 {
		return
	}

	attr := newSpanEventAttr(spans[0].Attrs)

	for _, span := range spans[1:] {
		var obj traceEventObject
		switch span.NameKind() {
		case "serv:prepcoc":
			obj = te.runPreprocSpanEvent(span, attr)
		case "serv:localexec":
			obj = te.runLocalSpanEvent(span, attr)
		case "serv:remoteexec", "serv:reproxyexec", "serv:rewrap":
			obj = te.runRemoteSpanEvent(span, attr)
		case "rbe:worker":
			worker, _ := span.Attrs["worker"].(string)
			te.mu.Lock()
			workerID, ok := te.rbeWorkers[worker]
			if !ok {
				workerID = len(te.rbeWorkers)
				te.rbeWorkers[worker] = workerID
			}
			te.mu.Unlock()
			obj = te.rbeWorkerSpanEvent(span, attr, workerID)
		default:
			if strings.HasPrefix(span.Name, "serv:pool=") {
				obj = te.runLocalSpanEvent(span, attr)
			} else {
				continue
			}
		}
		te.q <- obj
	}
}

type spanEventAttr struct {
	id          string
	description string
	action      string
	spanName    string
	output0     string
	command     string
	backtrace   string
	prevID      string
	prevOut     string
}

func newSpanEventAttr(attr map[string]any) spanEventAttr {
	id, _ := attr["id"].(string)
	description, _ := attr["description"].(string)
	action, _ := attr["action"].(string)
	spanName, _ := attr["span_name"].(string)
	output0, _ := attr["output0"].(string)
	command, _ := attr["command"].(string)
	args := strings.Split(command, " ")
	backtraces, _ := attr[logLabelKeyBacktraces].([]string)
	if len(args) > 7 {
		args = args[:7]
		args = append(args, "...")
	}
	prevID, _ := attr["prev"].(string)
	prevOut, _ := attr["prev_out"].(string)
	return spanEventAttr{
		id:          id,
		description: description,
		action:      action,
		spanName:    spanName,
		output0:     output0,
		command:     strings.Join(args, " "),
		backtrace:   strings.Join(backtraces, "<"),
		prevID:      prevID,
		prevOut:     prevOut,
	}
}

func (te *traceEvents) runPreprocSpanEvent(span trace.SpanData, attr spanEventAttr) traceEventObject {
	return traceEventObject{
		Name: attr.output0,
		Cat:  attr.spanName,
		Ph:   "X",
		T:    span.Start.Sub(te.start).Microseconds(),
		Pid:  sisoPreprocPid,
		Tid:  int64(span.Attrs["tid"].(int)),
		Dur:  span.Duration().Microseconds(),
		Args: map[string]any{
			"id":          attr.id,
			"description": attr.description,
			"action":      attr.action,
			"command":     attr.command,
			"backtrace":   attr.backtrace,
			"prev_id":     attr.prevID,
			"prev_out":    attr.prevOut,
		},
	}
}

func (te *traceEvents) runLocalSpanEvent(span trace.SpanData, attr spanEventAttr) traceEventObject {
	return traceEventObject{
		Name: attr.output0,
		Cat:  attr.spanName,
		Ph:   "X",
		T:    span.Start.Sub(te.start).Microseconds(),
		Pid:  sisoLocalPid,
		Tid:  int64(span.Attrs["tid"].(int)),
		Dur:  span.Duration().Microseconds(),
		Args: map[string]any{
			"id":          attr.id,
			"description": attr.description,
			"action":      attr.action,
			"command":     attr.command,
			"backtrace":   attr.backtrace,
			"prev_id":     attr.prevID,
			"prev_out":    attr.prevOut,
		},
	}
}

func (te *traceEvents) runRemoteSpanEvent(span trace.SpanData, attr spanEventAttr) traceEventObject {
	return traceEventObject{
		Name: attr.output0,
		Cat:  attr.spanName,
		Ph:   "X",
		T:    span.Start.Sub(te.start).Microseconds(),
		Pid:  sisoRemotePid,
		Tid:  int64(span.Attrs["tid"].(int)),
		Dur:  span.Duration().Microseconds(),
		Args: map[string]any{
			"id":          attr.id,
			"description": attr.description,
			"action":      attr.action,
			"command":     attr.command,
			"backtrace":   attr.backtrace,
			"prev_id":     attr.prevID,
			"prev_out":    attr.prevOut,
		},
	}
}

func (te *traceEvents) rbeWorkerSpanEvent(span trace.SpanData, attr spanEventAttr, workerID int) traceEventObject {
	return traceEventObject{
		Name: attr.output0,
		Cat:  attr.spanName,
		Ph:   "X",
		T:    span.Start.Sub(te.start).Microseconds(),
		Pid:  sisoRBEPid,
		Tid:  int64(workerID),
		Dur:  span.Duration().Microseconds(),
		Args: map[string]any{
			"id":          attr.id,
			"descroption": attr.description,
			"action":      attr.action,
			"command":     attr.command,
			"backtrace":   attr.backtrace,
			"prev_id":     attr.prevID,
			"prev_out":    attr.prevOut,
			"worker":      span.Attrs["worker"].(string),
		},
	}
}

func (te *traceEvents) Close(ctx context.Context) {
	close(te.quit)
	<-te.done
	clog.Infof(ctx, "trace finalize")

	if te.fname == "" {
		return
	}
	tmpname := te.fname + ".tmp"
	buf, err := os.ReadFile(tmpname)
	if err != nil {
		clog.Warningf(ctx, "Failed to read %s: %v", tmpname, err)
		return
	}
	defer os.Remove(tmpname)

	traceData := map[string]any{
		"traceEvents":     json.RawMessage(buf),
		"displayTimeUnit": "ms",
	}
	for _, key := range te.metadata.Keys() {
		traceData[key] = te.metadata.Get(key)
	}
	traceDataJson, err := json.Marshal(traceData)
	if err != nil {
		clog.Warningf(ctx, "Failed to marshal trace data: %v", err)
		return
	}

	err = os.WriteFile(te.fname, traceDataJson, 0644)
	if err != nil {
		clog.Warningf(ctx, "Failed to write %s: %v", te.fname, err)
	}
}

type traceStats struct {
	mu sync.Mutex
	s  map[string]*TraceStat
}

func newTraceStats() *traceStats {
	return &traceStats{
		s: make(map[string]*TraceStat),
	}
}

// TraceStat is trace statistics.
type TraceStat struct {
	// Name is trace name.
	Name string

	// N is count of the trace.
	N int

	// NErr is error count of the trace.
	NErr int

	// Total is total duration of the trace.
	Total time.Duration

	// Max is max duration of the trace.
	Max time.Duration

	// Buckets are buckets of trace durations.
	//
	//  0: [0,10ms)
	//  1: [10ms, 100ms)
	//  2: [100ms, 1s)
	//  3: [1s, 10s)
	//  4: [10s, 1m)
	//  5: [1m, 10m)
	//  6: >=10m
	Buckets [7]int
}

func bucketIndex(dur time.Duration) int {
	switch {
	case dur < 10*time.Millisecond:
		return 0
	case dur < 100*time.Millisecond:
		return 1
	case dur < 1*time.Second:
		return 2
	case dur < 10*time.Second:
		return 3
	case dur < 1*time.Minute:
		return 4
	case dur < 10*time.Minute:
		return 5
	default:
		return 6
	}
}

func (t *TraceStat) update(dur time.Duration, isErr bool) {
	t.N++
	if isErr {
		t.NErr++
	}
	t.Total += dur
	if t.Max < dur {
		t.Max = dur
	}
	t.Buckets[bucketIndex(dur)]++
}

// Avg returns average duration of the trace.
func (t *TraceStat) Avg() time.Duration {
	return t.Total / time.Duration(int64(t.N))
}

func (s *traceStats) update(tc *trace.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, span := range tc.Spans() {
		ts, ok := s.s[span.Name]
		if !ok {
			ts = &TraceStat{Name: span.Name}
			s.s[span.Name] = ts
		}
		ts.update(span.Duration(), span.Status != nil)
	}
}

func (s *traceStats) get() []*TraceStat {
	var ret []*TraceStat
	s.mu.Lock()
	for _, ts := range s.s {
		ret = append(ret, ts)
	}
	s.mu.Unlock()
	sort.Slice(ret, func(i, j int) bool {
		return ret[i].Avg() > ret[j].Avg()
	})
	return ret
}
