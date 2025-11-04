// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package metricscmd

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/subcommands"

	"go.chromium.org/build/siso/build"
)

const summaryUsage = `summarize siso_metrics.json

 $ siso metrics summary -C <dir> \
    [--step_types <types>] \
    [--elapsed_time_sorting] \
    [--elapsed_time=run|step] \
    [--input siso_metrics.json]

summarize <dir>/.siso_metrics.json (--input)
as depot_tools/post_ninja_build_summary.py does.
`

func (*summaryCommand) Name() string {
	return "summary"
}

func (*summaryCommand) Synopsis() string {
	return "summarize siso_metrics.json"
}

func (*summaryCommand) Usage() string {
	return summaryUsage
}

type summaryCommand struct {
	dir                string
	input              string
	stepTypes          string
	elapsedTime        string
	elapsedTimeSorting bool
}

func (c *summaryCommand) SetFlags(flagSet *flag.FlagSet) {
	flagSet.StringVar(&c.dir, "C", ".", "ninja running directory, where siso_metrics.json exists")
	flagSet.StringVar(&c.input, "input", "siso_metrics.json", "filename of siso_metrics.json to summarize")
	flagSet.StringVar(&c.stepTypes, "step_types", "", "semicolon separated glob patterns (go filepath.Match) for build-step grouping")
	flagSet.StringVar(&c.elapsedTime, "elapsed_time", "run", `metrics to use for elapsed time. "run" or "step". "run": time to run local command or call remote execution.  "step": full duration for the step, including preproc, waiting resource to run command etc.`)
	flagSet.BoolVar(&c.elapsedTimeSorting, "elapsed_time_sorting", false, "Sort output by elapsed time instead of weighted time")
}

func (c *summaryCommand) Execute(ctx context.Context, flagSet *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	err := c.run()
	if err != nil {
		switch {
		case errors.Is(err, flag.ErrHelp):
			fmt.Fprintf(os.Stderr, "%v\n%s\n", err, summaryUsage)
			return subcommands.ExitUsageError
		default:
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return subcommands.ExitFailure
		}
	}
	return subcommands.ExitSuccess
}

func (c *summaryCommand) run() error {
	switch c.elapsedTime {
	case "run", "step":
	default:
		return fmt.Errorf(`wrong --elapsed_time=%s  "run" or "step". %w`, c.elapsedTime, flag.ErrHelp)
	}

	err := os.Chdir(c.dir)
	if err != nil {
		return err
	}
	metrics, err := loadMetrics(c.input)
	if err != nil {
		return err
	}

	var filteredMetrics []build.StepMetric
	for _, s := range metrics {
		if s.StepID == "" {
			// this is special entry for build metrics, not per step metrics.
			continue
		}
		filteredMetrics = append(filteredMetrics, s)
	}
	metrics = filteredMetrics

	if len(metrics) == 0 {
		return nil
	}

	// TODO(ukai): deduce wait time from the duration?

	// same algorithm with post_build_ninja_summary.py
	// https://chromium.googlesource.com/chromium/tools/depot_tools/+/80226254ea024e756b9ad5e8a39160405880cbb1/post_build_ninja_summary.py#214
	var accumulatedDuration time.Duration
	var m []*targetMetric
	var events []buildEvent
	var earliest, latest time.Duration
	for _, s := range metrics {
		var start, end time.Duration
		switch c.elapsedTime {
		case "run":
			start = time.Duration(s.ActionStartTime)
			end = start + time.Duration(s.RunTime)
		case "step":
			start = time.Duration(s.Ready) + time.Duration(s.Start)
			end = start + time.Duration(s.Duration)
		}
		if start == 0 && end == 0 {
			// handler only step (i.e. stamp, copy) doesn't have ActionStartTime/RunTime, so ignore such steps.
			continue
		}
		if earliest == 0 || start < earliest {
			earliest = start
		}
		if end > latest {
			latest = end
		}
		target := &targetMetric{
			Output: s.Output,
			Start:  start,
			End:    end,
		}
		m = append(m, target)
		accumulatedDuration += target.Duration()
		events = append(events, buildEvent{
			ts:     target.Start,
			event:  eventStart,
			target: target,
		}, buildEvent{
			ts:     target.End,
			event:  eventStop,
			target: target,
		})
	}
	if len(events) == 0 {
		return nil
	}
	length := latest - earliest

	// sort by time/event records
	slices.SortFunc(events, func(a, b buildEvent) int {
		return cmp.Or(cmp.Compare(a.ts, b.ts), cmp.Compare(a.event, b.event))
	})
	// current running task -> weighted time when the task started.
	runningTasks := make(map[*targetMetric]time.Duration)
	lastTime := events[0].ts
	var lastWeightedTime time.Duration
	var accumulatedWeightedDuration time.Duration
	for _, ev := range events {
		numRunning := len(runningTasks)
		if numRunning > 0 {
			lastWeightedTime += time.Duration(float64(ev.ts-lastTime) / float64(numRunning))
		}
		switch ev.event {
		case eventStart:
			runningTasks[ev.target] = lastWeightedTime
		case eventStop:
			ev.target.weighted = lastWeightedTime - runningTasks[ev.target]
			accumulatedWeightedDuration += ev.target.weighted
			delete(runningTasks, ev.target)
		}
		lastTime = ev.ts
	}
	// Warn if the sum of weighted times is off by more than half a second.
	wdiff := length - accumulatedWeightedDuration
	if wdiff.Abs() > 500*time.Millisecond {
		fmt.Printf("Warning: Possible corrupt siso_metrics.json, result may be untrustworthy. length = %s, weighted total = %s\n", formatDuration(length), formatDuration(accumulatedWeightedDuration))
	}

	// Print the slowest build steps:
	fmt.Println("    Longest build steps:")
	if c.elapsedTimeSorting {
		sort.Slice(m, func(i, j int) bool {
			return m[i].Duration() > m[j].Duration()
		})
	} else {
		sort.Slice(m, func(i, j int) bool {
			return m[i].WeightedDuration() > m[j].WeightedDuration()
		})
	}
	longCount := 10
	topMetrics := m
	if len(topMetrics) > longCount {
		topMetrics = topMetrics[:longCount]
	}
	for i := len(topMetrics) - 1; i >= 0; i-- {
		tm := topMetrics[i]
		fmt.Printf("      %8s weighted to build %s (%s elapsed time)\n",
			formatDuration(tm.WeightedDuration()),
			tm.Output,
			formatDuration(tm.Duration()))
	}

	// Sum up the time by file extension/type of the output file
	am, err := c.aggregate(m)
	if err != nil {
		return err
	}
	fmt.Println("    Time by build-step type:")
	if c.elapsedTimeSorting {
		sort.Slice(am, func(i, j int) bool {
			return am[i].Duration > am[j].Duration
		})
	} else {
		sort.Slice(am, func(i, j int) bool {
			return am[i].WeightedDuration > am[j].WeightedDuration
		})
	}
	// Print the slowest build target types:
	longCount += len(strings.Split(c.stepTypes, ";"))
	if len(am) > longCount {
		am = am[:longCount]
	}
	for i := len(am) - 1; i >= 0; i-- {
		s := am[i]
		fmt.Printf("      %8s weighted to generate %d %s files (%s elapsed time sum)\n",
			formatDuration(s.WeightedDuration),
			s.Count,
			s.Type,
			formatDuration(s.Duration))
	}
	if length == 0 {
		return nil
	}
	fmt.Printf("    %s weighted time (%s elapsed time sum, %1.1fx parallelism)\n",
		formatDuration(length),
		formatDuration(accumulatedDuration),
		accumulatedDuration.Seconds()/length.Seconds())
	fmt.Printf("    %d build steps completed, average of %1.2f/s\n",
		len(metrics),
		float64(len(metrics))/length.Seconds())
	return nil
}

type targetMetric struct {
	Output   string
	Start    time.Duration
	End      time.Duration
	weighted time.Duration
}

func (t targetMetric) Duration() time.Duration {
	return t.End - t.Start
}

func (t targetMetric) WeightedDuration() time.Duration {
	return t.weighted
}

type eventType int

const (
	eventStart eventType = iota
	eventStop
)

type buildEvent struct {
	ts     time.Duration
	event  eventType
	target *targetMetric
}

type aggregatedMetric struct {
	Type             string
	Count            int
	Duration         time.Duration
	WeightedDuration time.Duration
}

func (c *summaryCommand) aggregate(metrics []*targetMetric) ([]aggregatedMetric, error) {
	pats := strings.Split(c.stepTypes, ";")
	am := make(map[string]aggregatedMetric)
	for _, m := range metrics {
		t, err := stepType(m, pats)
		if err != nil {
			return nil, err
		}
		a := am[t]
		a.Type = t
		a.Count++
		a.Duration += m.Duration()
		a.WeightedDuration += m.WeightedDuration()
		am[t] = a
	}
	var ret []aggregatedMetric
	for _, v := range am {
		ret = append(ret, v)
	}
	return ret, nil
}

func stepType(metric *targetMetric, pats []string) (string, error) {
	for _, p := range pats {
		ok, err := filepath.Match(p, metric.Output)
		if err != nil {
			return "", err
		}
		if ok {
			return p, nil
		}
	}
	return filepath.Ext(metric.Output), nil
}

func formatDuration(d time.Duration) string {
	return fmt.Sprintf("%.1fs", d.Seconds())
}
