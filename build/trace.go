// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"go.chromium.org/build/siso/o11y/trace"
)

func (b *Builder) traceEvents(ctx context.Context, tc *trace.Context) []trace.Event {
	spans := tc.Spans()
	if len(spans) == 0 {
		return nil
	}

	attr := newSpanEventAttr(spans[0].Attrs)

	events := make([]trace.Event, 0, len(spans[:1]))
	for _, span := range spans[1:] {
		var obj trace.Event
		switch span.NameKind() {
		case "serv:preproc":
			obj = runPreprocSpanEvent(span, attr, b.tracePidPreproc)
		case "serv:localexec":
			obj = runLocalSpanEvent(span, attr, b.tracePidLocal)
		case "serv:remoteexec", "serv:rewrap":
			obj = runRemoteSpanEvent(span, attr, b.tracePidRemote)
		case "rbe:worker":
			worker, _ := span.Attrs["worker"].(string)
			workerID := b.tracer.Thread(ctx, b.tracePidWorker, worker)
			obj = rbeWorkerSpanEvent(span, attr, b.tracePidWorker, workerID)
		default:
			if strings.HasPrefix(span.Name, "serv:pool=") {
				obj = runLocalSpanEvent(span, attr, b.tracePidLocal)
			} else {
				continue
			}
		}
		events = append(events, obj)
	}
	return events
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
	return spanEventAttr{
		id:          id,
		description: description,
		action:      action,
		spanName:    spanName,
		output0:     output0,
		command:     strings.Join(args, " "),
		backtrace:   strings.Join(backtraces, "<"),
		prevID:      prevID,
	}
}

func runPreprocSpanEvent(span trace.SpanData, attr spanEventAttr, pid int64) trace.Event {
	return trace.Event{
		Name: attr.output0,
		Cat:  attr.spanName,
		Ph:   "X",
		T:    span.Start.Sub(trace.StartTime()).Microseconds(),
		Pid:  pid,
		Tid:  int64(span.Tid),
		Dur:  span.Duration().Microseconds(),
		Args: map[string]any{
			"id":          attr.id,
			"description": attr.description,
			"action":      attr.action,
			"command":     attr.command,
			"backtrace":   attr.backtrace,
			"prev_id":     attr.prevID,
		},
	}
}

func runLocalSpanEvent(span trace.SpanData, attr spanEventAttr, pid int64) trace.Event {
	return trace.Event{
		Name: attr.output0,
		Cat:  attr.spanName,
		Ph:   "X",
		T:    span.Start.Sub(trace.StartTime()).Microseconds(),
		Pid:  pid,
		Tid:  int64(span.Tid),
		Dur:  span.Duration().Microseconds(),
		Args: map[string]any{
			"id":          attr.id,
			"description": attr.description,
			"action":      attr.action,
			"command":     attr.command,
			"backtrace":   attr.backtrace,
			"prev_id":     attr.prevID,
		},
	}
}

func runRemoteSpanEvent(span trace.SpanData, attr spanEventAttr, pid int64) trace.Event {
	return trace.Event{
		Name: attr.output0,
		Cat:  attr.spanName,
		Ph:   "X",
		T:    span.Start.Sub(trace.StartTime()).Microseconds(),
		Pid:  pid,
		Tid:  int64(span.Tid),
		Dur:  span.Duration().Microseconds(),
		Args: map[string]any{
			"id":          attr.id,
			"description": attr.description,
			"action":      attr.action,
			"command":     attr.command,
			"backtrace":   attr.backtrace,
			"prev_id":     attr.prevID,
		},
	}
}

func rbeWorkerSpanEvent(span trace.SpanData, attr spanEventAttr, pid int64, workerID int) trace.Event {
	return trace.Event{
		Name: attr.output0,
		Cat:  attr.spanName,
		Ph:   "X",
		T:    span.Start.Sub(trace.StartTime()).Microseconds(),
		Pid:  pid,
		Tid:  int64(workerID),
		Dur:  span.Duration().Microseconds(),
		Args: map[string]any{
			"id":          attr.id,
			"description": attr.description,
			"action":      attr.action,
			"command":     attr.command,
			"backtrace":   attr.backtrace,
			"prev_id":     attr.prevID,
			"worker":      span.Attrs["worker"].(string),
		},
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
